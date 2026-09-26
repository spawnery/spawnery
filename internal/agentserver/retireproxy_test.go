/*
Copyright paul_wtf.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agentserver

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

func proxyPod(name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns", Annotations: annotations,
		Labels: podspec.ProxyLabels("production", "gateway"),
	}}
}

func proxyWriter(t *testing.T, objs ...client.Object) KubeWriter {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return KubeWriter{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), Clock: time.Now}
}

func TestRetireMarksAProxyWhenNoServerHasTheName(t *testing.T) {
	w := proxyWriter(t, proxyPod("gateway-abc", nil))
	applied, err := w.Retire(context.Background(), "ns", "gateway-abc")
	if err != nil || !applied {
		t.Fatalf("Retire = %v, %v; want applied", applied, err)
	}
	var pod corev1.Pod
	if err := w.Client.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "gateway-abc"}, &pod); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := pod.Annotations[podspec.AnnotationRetireRequested]; !ok {
		t.Error("the proxy carries no retire request")
	}
}

func TestRetireRefusesAProxyAlreadyDraining(t *testing.T) {
	for _, ann := range []string{podspec.AnnotationRetireRequested, podspec.AnnotationProxyDrainingSince} {
		w := proxyWriter(t, proxyPod("gateway-abc", map[string]string{ann: "2026-09-26T12:00:00Z"}))
		if applied, err := w.Retire(context.Background(), "ns", "gateway-abc"); err != nil || applied {
			t.Errorf("%s: Retire = %v, %v; want not applied", ann, applied, err)
		}
	}
}

func TestRetireDoesNotTouchAGamePodOfThatName(t *testing.T) {
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "ns",
		Labels: map[string]string{podspec.LabelRole: podspec.RoleServer}}}
	if _, err := proxyWriter(t, game).Retire(context.Background(), "ns", "lobby-a"); !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("err = %v, want ErrNoSuchServer", err)
	}
}

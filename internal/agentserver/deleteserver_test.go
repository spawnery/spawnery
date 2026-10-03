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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// The manager's cache holds only claims carrying this operator's label, so
// reading through it would answer NOT_FOUND where REFUSED is the truth.
func TestDeleteSeesAClaimTheCacheCannot(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupOnDemand},
	}
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: podspec.DataClaimName("private-servers-c0ffee"), Namespace: "minecraft",
		},
	}
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).Build()
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, foreign).Build()
	w := KubeWriter{Client: cached, Reader: direct, Clock: time.Now}

	_, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee")
	if !errors.Is(err, ErrForeignClaim) {
		t.Fatalf("err = %v, want ErrForeignClaim", err)
	}
}

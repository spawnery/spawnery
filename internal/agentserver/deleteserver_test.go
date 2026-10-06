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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/podspec"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return scheme
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

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
	scheme := testScheme(t)
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).Build()
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group, foreign).Build()
	w := KubeWriter{Client: cached, Reader: direct, Clock: time.Now}

	_, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee")
	if !errors.Is(err, ErrForeignClaim) {
		t.Fatalf("err = %v, want ErrForeignClaim", err)
	}
}

type fakeWorlds struct {
	exists  bool
	deleted []string
}

func (f *fakeWorlds) Exists(context.Context, string) (bool, error) { return f.exists, nil }
func (f *fakeWorlds) MarkDeleted(_ context.Context, w string) error {
	f.deleted = append(f.deleted, w)
	return nil
}

func objectStoreGroup() *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:    spawneryv1alpha1.ServerGroupOnDemand,
			Storage: &spawneryv1alpha1.StorageSpec{Backend: spawneryv1alpha1.StorageBackendObjectStore, Keep: []string{"world"}},
		},
	}
}

func TestDeleteOfAnObjectStoreWorldMarksItInTheBucket(t *testing.T) {
	c := fakeClient(t, objectStoreGroup())
	worlds := &fakeWorlds{exists: true}
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now, Worlds: worlds}
	got, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee")
	if err != nil {
		t.Fatal(err)
	}
	if !got.World || len(worlds.deleted) != 1 || worlds.deleted[0] != "minecraft/private-servers/c0ffee" {
		t.Fatalf("got %+v, marked %v", got, worlds.deleted)
	}
}

func TestDeleteOfAnObjectStoreWorldThatIsNowhereIsNotFound(t *testing.T) {
	c := fakeClient(t, objectStoreGroup())
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now, Worlds: &fakeWorlds{}}
	if _, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee"); !errors.Is(err, ErrNoSuchServer) {
		t.Fatalf("err = %v, want ErrNoSuchServer", err)
	}
}

func TestDeleteOfAnObjectStoreWorldWithSyncOffIsRefused(t *testing.T) {
	c := fakeClient(t, objectStoreGroup())
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now}
	_, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee")
	if !errors.Is(err, ErrWorldSyncOff) {
		t.Fatalf("err = %v, want ErrWorldSyncOff", err)
	}

	s := &Server{opts: Options{Writer: w}, requestRate: newRequestLimiter(time.Now)}
	resp := s.answerDeleteServer(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "minecraft", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy},
		7, &agentpb.DeleteServerRequest{Group: "private-servers", Key: "c0ffee"})
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
}

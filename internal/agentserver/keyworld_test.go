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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/worldsync"
)

func roundsGroup() *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "rounds", Namespace: "minecraft", UID: "rounds-uid"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:         spawneryv1alpha1.ServerGroupEphemeral,
			MaxInstances: ptr.To[int32](1),
			Storage:      &spawneryv1alpha1.StorageSpec{Backend: spawneryv1alpha1.StorageBackendObjectStore, Keep: []string{"world"}},
		},
	}
}

func roundMember(name string, mutate func(*spawneryv1alpha1.Server)) *spawneryv1alpha1.Server {
	s := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "rounds"}},
		Status:     spawneryv1alpha1.ServerStatus{Phase: string(phase.Ready), PodUID: name + "-uid"},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func caller(name string) Caller {
	return Caller{Namespace: "minecraft", Pod: name, PodUID: name + "-uid"}
}

func keyWriter(t *testing.T, worlds *fakeWorlds, objs ...client.Object) KubeWriter {
	c := fakeClient(t, objs...)
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now}
	if worlds != nil {
		w.Worlds = worlds
	}
	return w
}

func getServer(t *testing.T, w KubeWriter, name string) *spawneryv1alpha1.Server {
	t.Helper()
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(context.Background(), client.ObjectKey{Namespace: "minecraft", Name: name}, &srv); err != nil {
		return nil
	}
	return &srv
}

func TestKeyWorldSetsTheKeyThenBindsAndIsIdempotent(t *testing.T) {
	worlds := &fakeWorlds{}
	w := keyWriter(t, worlds, roundsGroup(), roundMember("rounds-x7k2", nil))
	world, err := w.KeyWorld(context.Background(), caller("rounds-x7k2"), "round-7")
	if err != nil || world != "minecraft/rounds/round-7" {
		t.Fatalf("KeyWorld = %q, %v", world, err)
	}
	if got := getServer(t, w, "rounds-x7k2").Spec.WorldKey; got != "round-7" {
		t.Errorf("spec.worldKey = %q", got)
	}
	want := worldsync.Binding{Server: "rounds-x7k2", Pod: "minecraft/rounds-x7k2"}
	if got := worlds.bindings[world]; got != want {
		t.Errorf("binding = %+v, want %+v", got, want)
	}

	// The world now exists (the node took its lease); the same key again
	// still answers.
	worlds.exists = true
	if again, err := w.KeyWorld(context.Background(), caller("rounds-x7k2"), "round-7"); err != nil || again != world {
		t.Fatalf("again: %q, %v", again, err)
	}
	if _, err := w.KeyWorld(context.Background(), caller("rounds-x7k2"), "round-8"); !errors.Is(err, ErrKeySet) {
		t.Fatalf("another key: err = %v, want ErrKeySet", err)
	}
}

func TestKeyWorldFinishesABindingThatFailed(t *testing.T) {
	worlds := &fakeWorlds{bindFail: errors.New("store down")}
	w := keyWriter(t, worlds, roundsGroup(), roundMember("rounds-x7k2", nil))
	if _, err := w.KeyWorld(context.Background(), caller("rounds-x7k2"), "round-7"); !errors.Is(err, ErrBindingFailed) {
		t.Fatalf("err = %v, want ErrBindingFailed", err)
	}
	if got := getServer(t, w, "rounds-x7k2").Spec.WorldKey; got != "round-7" {
		t.Fatalf("spec.worldKey = %q: the key comes before the binding", got)
	}
	worlds.bindFail = nil
	if _, err := w.KeyWorld(context.Background(), caller("rounds-x7k2"), "round-7"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if _, ok := worlds.bindings["minecraft/rounds/round-7"]; !ok {
		t.Error("the retry wrote no binding")
	}
}

func TestKeyWorldRefusals(t *testing.T) {
	plain := roundsGroup()
	plain.Spec.Storage, plain.Spec.MaxInstances = nil, nil
	onDemand := roundsGroup()
	onDemand.Spec.Type = spawneryv1alpha1.ServerGroupOnDemand
	for name, tc := range map[string]struct {
		group  *spawneryv1alpha1.ServerGroup
		others []client.Object
		worlds *fakeWorlds
		key    string
		uid    string
		want   error
	}{
		"group without storage": {group: plain, worlds: &fakeWorlds{}, key: "round-7", want: ErrNotKeepable},
		"on-demand group":       {group: onDemand, worlds: &fakeWorlds{}, key: "round-7", want: ErrNotKeepable},
		"world sync off":        {group: roundsGroup(), key: "round-7", want: ErrWorldSyncOff},
		"suffix-shaped key":     {group: roundsGroup(), worlds: &fakeWorlds{}, key: "abcd", want: instance.ErrBadKey},
		"world exists":          {group: roundsGroup(), worlds: &fakeWorlds{exists: true}, key: "round-7", want: ErrKeyTaken},
		"deletion pending":      {group: roundsGroup(), worlds: &fakeWorlds{pending: true}, key: "round-7", want: ErrWorldDeleting},
		"another pod":           {group: roundsGroup(), worlds: &fakeWorlds{}, key: "round-7", uid: "old-uid", want: ErrNoSuchServer},
		"carried by another": {group: roundsGroup(), worlds: &fakeWorlds{}, key: "round-7", want: ErrKeyTaken,
			others: []client.Object{roundMember("rounds-wxyz", func(s *spawneryv1alpha1.Server) { s.Spec.WorldKey = "round-7" })}},
		"its name is taken": {group: roundsGroup(), worlds: &fakeWorlds{}, key: "round-7", want: ErrKeyTaken,
			others: []client.Object{roundMember("rounds-round-7", nil)}},
	} {
		t.Run(name, func(t *testing.T) {
			objs := append([]client.Object{tc.group, roundMember("rounds-x7k2", nil)}, tc.others...)
			w := keyWriter(t, tc.worlds, objs...)
			c := caller("rounds-x7k2")
			if tc.uid != "" {
				c.PodUID = tc.uid
			}
			_, err := w.KeyWorld(context.Background(), c, tc.key)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got := getServer(t, w, "rounds-x7k2").Spec.WorldKey; got != "" {
				t.Errorf("a refused key was set: %q", got)
			}
		})
	}
}

func TestKeyWorldIsRefusedFromAProxy(t *testing.T) {
	w := keyWriter(t, &fakeWorlds{}, roundsGroup())
	s := &Server{opts: Options{Writer: w}, requestRate: newRequestLimiter(time.Now)}
	resp := s.answerCloudRequest(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "minecraft", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy},
		&agentpb.CloudRequest{Id: 3, Request: &agentpb.CloudRequest_KeyWorld{KeyWorld: &agentpb.KeyWorldRequest{Key: "round-7"}}})
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("response = %+v, want REFUSED", resp)
	}
	if !answersOffTheLoop(&agentpb.CloudRequest{Request: &agentpb.CloudRequest_KeyWorld{KeyWorld: &agentpb.KeyWorldRequest{}}}) {
		t.Error("KeyWorld waits on the object store and must answer off the session loop")
	}
}

func TestStartOfAKeptWorldAnswersTheMemberThatKeyedIt(t *testing.T) {
	adopted := roundMember("rounds-x7k2", func(s *spawneryv1alpha1.Server) { s.Spec.WorldKey = "round-7" })
	w := keyWriter(t, &fakeWorlds{exists: true}, roundsGroup(), adopted)
	got, err := w.StartServer(context.Background(), "minecraft", "rounds", "round-7", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AlreadyRunning || got.Name != "rounds-x7k2" {
		t.Fatalf("got %+v, want the running member that keyed the world", got)
	}
	if getServer(t, w, "rounds-round-7") != nil {
		t.Error("a second member was created for a world that is running")
	}
}

func TestStartOfAKeptWorldResumesIt(t *testing.T) {
	finished := roundMember("rounds-x7k2", func(s *spawneryv1alpha1.Server) {
		s.Spec.WorldKey = "round-7"
		s.Status.Phase = string(phase.Finished)
	})
	w := keyWriter(t, &fakeWorlds{exists: true}, roundsGroup(), finished)
	got, err := w.StartServer(context.Background(), "minecraft", "rounds", "round-7", nil)
	if err != nil || got.Name != "rounds-round-7" || got.AlreadyRunning {
		t.Fatalf("got %+v, %v", got, err)
	}
	srv := getServer(t, w, "rounds-round-7")
	if srv == nil || srv.Spec.WorldKey != "round-7" || !srv.Spec.Resumed || srv.Spec.Key != "" {
		t.Fatalf("resumed member = %+v", srv)
	}
	again, err := w.StartServer(context.Background(), "minecraft", "rounds", "round-7", nil)
	if err != nil || !again.AlreadyRunning || again.Name != "rounds-round-7" {
		t.Fatalf("second start: %+v, %v", again, err)
	}
}

func TestStartOfAKeyWithoutAWorldIsNotFound(t *testing.T) {
	w := keyWriter(t, &fakeWorlds{}, roundsGroup())
	if _, err := w.StartServer(context.Background(), "minecraft", "rounds", "round-7", nil); !errors.Is(err, ErrNoWorld) {
		t.Fatalf("err = %v, want ErrNoWorld", err)
	}
	s := &Server{opts: Options{Writer: w}, requestRate: newRequestLimiter(time.Now)}
	resp := s.answerStartServer(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "minecraft", PodName: "gateway-0", Role: agent.RoleProxy},
		1, &agentpb.StartServerRequest{Group: "rounds", Key: "round-7"})
	if resp.GetError().GetReason() != agentpb.RequestError_NOT_FOUND {
		t.Fatalf("reason = %v, want NOT_FOUND", resp.GetError().GetReason())
	}
}

func TestOnlyResumedMembersCountAgainstMaxInstances(t *testing.T) {
	ownMembers := []client.Object{
		roundsGroup(),
		roundMember("rounds-aaaa", nil),
		roundMember("rounds-bbbb", func(s *spawneryv1alpha1.Server) { s.Spec.WorldKey = "round-1" }),
	}
	w := keyWriter(t, &fakeWorlds{exists: true}, ownMembers...)
	if _, err := w.StartServer(context.Background(), "minecraft", "rounds", "round-7", nil); err != nil {
		t.Fatalf("the group's own members were counted against maxInstances: %v", err)
	}
	if _, err := w.StartServer(context.Background(), "minecraft", "rounds", "round-8", nil); !errors.Is(err, ErrTooManyInstances) {
		t.Fatalf("second resume: err = %v, want ErrTooManyInstances", err)
	}
}

func TestDeleteOfAKeptWorldFindsItsCarrierByKey(t *testing.T) {
	adopted := roundMember("rounds-x7k2", func(s *spawneryv1alpha1.Server) { s.Spec.WorldKey = "round-7" })
	worlds := &fakeWorlds{exists: true}
	w := keyWriter(t, worlds, roundsGroup(), adopted, roundMember("rounds-wxyz", nil))
	got, err := w.DeleteServer(context.Background(), "minecraft", "rounds", "round-7")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "rounds-x7k2" || !got.World {
		t.Fatalf("got %+v", got)
	}
	if len(worlds.deleted) != 1 || worlds.deleted[0] != "minecraft/rounds/round-7" {
		t.Fatalf("marked %v", worlds.deleted)
	}
	if getServer(t, w, "rounds-x7k2") != nil {
		t.Error("the member that carries the world is still there")
	}
	if getServer(t, w, "rounds-wxyz") == nil {
		t.Error("an unkeyed member was deleted")
	}
}

func TestStopAcceptsAMemberThatKeyedItsWorld(t *testing.T) {
	adopted := roundMember("rounds-x7k2", func(s *spawneryv1alpha1.Server) { s.Spec.WorldKey = "round-7" })
	w := keyWriter(t, &fakeWorlds{}, roundsGroup(), adopted, roundMember("rounds-wxyz", nil))
	if err := w.StopServer(context.Background(), "minecraft", "rounds-x7k2"); err != nil {
		t.Fatalf("stop of a keyed member: %v", err)
	}
	if err := w.StopServer(context.Background(), "minecraft", "rounds-wxyz"); !errors.Is(err, ErrNotAnInstance) {
		t.Fatalf("stop of an unkeyed member: err = %v, want ErrNotAnInstance", err)
	}
}

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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/podspec"
)

var (
	commandNow   = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	proxyCaller  = grpcauth.Identity{Namespace: "ns", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy}
	serverCaller = grpcauth.Identity{Namespace: "ns", PodName: "lobby-a", PodUID: "pod-a", Role: agent.RoleServer}
)

func commandFixture(t *testing.T, objects ...client.Object) (*Server, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).WithObjects(objects...).Build()
	rec := events.NewFakeRecorder(16)
	clock := func() time.Time { return commandNow }
	s := &Server{
		opts: Options{
			Writer:      KubeWriter{Client: c, Clock: clock},
			State:       netstate.Source{Reader: c},
			Servers:     &recordingFanout{live: map[string]bool{}},
			Recorder:    rec,
			Clock:       clock,
			ExecuteWait: 300 * time.Millisecond,
		},
		requestRate: newRequestLimiter(time.Now),
	}
	return s, c, rec
}

func scalableGroup(name string, minR, maxR int32) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID("uid-" + name)},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:    spawneryv1alpha1.ServerGroupEphemeral,
			Scaling: &spawneryv1alpha1.ScalingSpec{MinReplicas: minR, MaxReplicas: maxR, SpareSlots: 10},
		},
	}
}

func askScale(s *Server, id grpcauth.Identity, group string, replicas int32, seconds int64) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
		Id: 1, Request: &agentpb.CloudRequest_Scale{Scale: &agentpb.ScaleRequest{
			Group: group, Replicas: replicas, DurationSeconds: seconds,
		}},
	})
}

func TestAScaleCreatesAnExactBoostOwnedByTheGroup(t *testing.T) {
	s, c, _ := commandFixture(t, scalableGroup("lobby", 1, 5))

	resp := askScale(s, serverCaller, "lobby", 0, 0)

	if resp.GetScale().GetReplicas() != 0 || resp.GetScale().GetExpiresAtUnix() != commandNow.Add(time.Hour).Unix() {
		t.Fatalf("answer = %+v, want a pin of 0 for the default hour", resp.GetResult())
	}
	var list spawneryv1alpha1.ScaleBoostList
	if err := c.List(context.Background(), &list, client.InNamespace("ns")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("boosts = %d, want 1", len(list.Items))
	}
	b := list.Items[0]
	if b.Spec.Mode != spawneryv1alpha1.ScaleBoostExact || b.Spec.Replicas != 0 || b.Spec.GroupRef.Name != "lobby" {
		t.Errorf("boost = %+v, want Exact 0 on lobby", b.Spec)
	}
	if len(b.OwnerReferences) != 1 || b.OwnerReferences[0].UID != "uid-lobby" {
		t.Errorf("owners = %+v, want the group", b.OwnerReferences)
	}
}

func TestAScaleIsRefusedWhereItCannotHold(t *testing.T) {
	persistent := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupPersistent},
	}
	s, _, _ := commandFixture(t, scalableGroup("lobby", 1, 5), persistent)

	for name, c := range map[string]struct {
		group    string
		replicas int32
		seconds  int64
		reason   agentpb.RequestError_Reason
		says     string
	}{
		"below zero":               {"lobby", -1, 0, agentpb.RequestError_REFUSED, "below zero"},
		"above maxReplicas":        {"lobby", 6, 0, agentpb.RequestError_REFUSED, "maxReplicas is 5"},
		"eight days":               {"lobby", 1, 8 * 24 * 3600, agentpb.RequestError_REFUSED, "7 days"},
		"seconds that overflow":    {"lobby", 1, 1 << 62, agentpb.RequestError_REFUSED, "7 days"},
		"a persistent group":       {"survival", 1, 0, agentpb.RequestError_REFUSED, "ephemeral"},
		"a group that is not here": {"nowhere", 1, 0, agentpb.RequestError_NOT_FOUND, "no group"},
	} {
		got := askScale(s, serverCaller, c.group, c.replicas, c.seconds).GetError()
		if got.GetReason() != c.reason || !strings.Contains(got.GetMessage(), c.says) {
			t.Errorf("%s: error = %v, want %s mentioning %q", name, got, c.reason, c.says)
		}
	}
}

func TestSevenDaysExactlyIsAllowed(t *testing.T) {
	s, _, _ := commandFixture(t, scalableGroup("lobby", 1, 5))
	if resp := askScale(s, serverCaller, "lobby", 2, 7*24*3600); resp.GetScale() == nil {
		t.Errorf("answer = %+v, want a pin: seven days is the limit, not past it", resp.GetResult())
	}
}

func askForceStop(s *Server, id grpcauth.Identity, server string) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
		Id: 2, Request: &agentpb.CloudRequest_ForceStop{ForceStop: &agentpb.ForceStopRequest{Server: server, Issuer: "alice"}},
	})
}

func readyServer(name, group, podUID string) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: group}},
		Status:     spawneryv1alpha1.ServerStatus{Phase: "Ready", PodName: name, PodUID: podUID},
	}
}

func TestAForceStopFromAProxySetsTheFlagAndLeavesARecord(t *testing.T) {
	s, c, rec := commandFixture(t, readyServer("lobby-a", "lobby", "pod-a"))

	resp := askForceStop(s, proxyCaller, "lobby-a")

	if resp.GetForceStop().GetServer() != "lobby-a" {
		t.Fatalf("answer = %+v, want lobby-a", resp.GetResult())
	}
	var srv spawneryv1alpha1.Server
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "lobby-a"}, &srv); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !srv.Spec.ForceStop {
		t.Error("spec.forceStop was not set")
	}
	select {
	case ev := <-rec.Events:
		for _, want := range []string{"ForceStopped", "alice", "gateway-0"} {
			if !strings.Contains(ev, want) {
				t.Errorf("event %q does not name %q", ev, want)
			}
		}
	default:
		t.Error("no event was recorded")
	}

	if again := askForceStop(s, proxyCaller, "lobby-a"); again.GetForceStop() == nil {
		t.Errorf("a second force-stop = %+v, want success: it is what shortens a grace period already running", again.GetResult())
	}
}

func TestAForceStopIsRefusedFromABackendAndForAProxyOrNothing(t *testing.T) {
	gateway := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-0", Namespace: "ns",
		Labels: map[string]string{podspec.LabelRole: podspec.RoleProxy}}}
	s, c, _ := commandFixture(t, readyServer("lobby-a", "lobby", "pod-a"), gateway)

	if got := askForceStop(s, serverCaller, "lobby-a").GetError(); got.GetReason() != agentpb.RequestError_REFUSED ||
		!strings.Contains(got.GetMessage(), "proxy") {
		t.Errorf("from a backend: %v, want REFUSED naming the proxy rule", got)
	}
	var srv spawneryv1alpha1.Server
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "lobby-a"}, &srv)
	if srv.Spec.ForceStop {
		t.Error("a backend's force-stop set the flag")
	}
	if got := askForceStop(s, proxyCaller, "gateway-0").GetError(); got.GetReason() != agentpb.RequestError_REFUSED {
		t.Errorf("a proxy's name: %v, want REFUSED", got)
	}
	if got := askForceStop(s, proxyCaller, "nobody").GetError(); got.GetReason() != agentpb.RequestError_NOT_FOUND {
		t.Errorf("an unknown name: %v, want NOT_FOUND", got)
	}
}

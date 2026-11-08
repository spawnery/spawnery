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

package netstate_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/phase"
)

func withReadyReason(srv *spawneryv1alpha1.Server, reason string) *spawneryv1alpha1.Server {
	meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
		Type: spawneryv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason,
	})
	return srv
}

func stateOf(t *testing.T, state *agentpb.NetworkState, name string) *agentpb.ServerState {
	t.Helper()
	for _, s := range state.GetServers() {
		if s.GetName() == name {
			return s
		}
	}
	t.Fatalf("no server %q in %v", name, state.GetServers())
	return nil
}

func TestBuildSaysWhichServerTheSchedulerCannotPlace(t *testing.T) {
	cases := []struct {
		name   string
		phase  phase.Phase
		reason string
		want   bool
	}{
		{"pending with no room", phase.Pending, phase.ReasonPodUnschedulable, true},
		{"pending before the scheduler looked", phase.Pending, phase.ReasonPodPending, false},
		{"starting", phase.Starting, phase.ReasonPodRunning, false},
		{"failed at the deadline", phase.Failed, phase.ReasonStartupTimeout, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := withReadyReason(serverInPhase("ns", "lobby-a", "lobby", string(c.phase), 0, 100), c.reason)
			src, _ := source(t, ephemeralGroup("ns", "lobby"), srv)

			got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if s := stateOf(t, got, "lobby-a"); s.GetUnschedulable() != c.want {
				t.Errorf("unschedulable = %v, want %v", s.GetUnschedulable(), c.want)
			}
		})
	}
}

func TestAServerWithoutAReadyConditionIsNotUnschedulable(t *testing.T) {
	src, _ := source(t, ephemeralGroup("ns", "lobby"), serverInPhase("ns", "lobby-a", "lobby", "Pending", 0, 100))

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stateOf(t, got, "lobby-a").GetUnschedulable() {
		t.Error("a Pending server with no Ready condition reads as unschedulable")
	}
}

func TestAnUnschedulableServerWithANodeIsNotReportedUnschedulable(t *testing.T) {
	srv := withReadyReason(serverInPhase("ns", "lobby-a", "lobby", "Pending", 0, 100), phase.ReasonPodUnschedulable)
	srv.Status.PodName = "lobby-a"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "ns",
			Labels: map[string]string{"spawnery.cloud/role": "server"}},
		Spec: corev1.PodSpec{NodeName: "node-2"},
	}
	src, _ := source(t, ephemeralGroup("ns", "lobby"), srv, pod)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := stateOf(t, got, "lobby-a")
	if s.GetNode() != "node-2" || s.GetUnschedulable() {
		t.Errorf("node = %q, unschedulable = %v, want node-2 and placeable: the status lags the bind by a "+
			"reconcile and the two fields never both say yes", s.GetNode(), s.GetUnschedulable())
	}
}

func TestAnUnschedulableOnDemandMemberReachesProxiesAndNotBackends(t *testing.T) {
	member := withReadyReason(onDemandMember("ns", "private-servers", "c0ffee"), phase.ReasonPodUnschedulable)
	member.Status.Phase = string(phase.Pending)
	src, _ := source(t, onDemandGroup("ns", "private-servers"), member)

	proxies, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !stateOf(t, proxies, "private-servers-c0ffee").GetUnschedulable() {
		t.Error("the proxies are not told the member cannot be placed")
	}
	backends, err := src.Build(context.Background(), "ns", netstate.ForServers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if hasServer(backends, "private-servers-c0ffee") {
		t.Error("a backend is shown an on-demand member")
	}
}

// Everything a report moves, which a fresh picture is not worth a push for.
func TestVisibleIgnoresWhatReportsMove(t *testing.T) {
	base := readyServer("ns", "lobby-a", "lobby", 0, 100)
	want := netstate.Visible(base, "node-1", "")

	moved := base.DeepCopy()
	moved.Status.Players = 40
	moved.Status.Slots = 120
	moved.Status.PlayableSlots = 80
	if got := netstate.Visible(moved, "node-1", ""); !proto.Equal(got, want) {
		t.Errorf("players, slots and playable slots moved the visible state: %v vs %v", got, want)
	}
}

func TestVisibleFollowsWhatALifecycleChangeMoves(t *testing.T) {
	base := readyServer("ns", "lobby-a", "lobby", 0, 100)
	base.Status.PodUID = "pod-1"
	want := netstate.Visible(base, "node-1", "")

	changes := map[string]func(*spawneryv1alpha1.Server) string{
		"phase":      func(s *spawneryv1alpha1.Server) string { s.Status.Phase = "Retiring"; return "node-1" },
		"registered": func(s *spawneryv1alpha1.Server) string { s.Status.Registered = false; return "node-1" },
		"pod":        func(s *spawneryv1alpha1.Server) string { s.Status.PodUID = "pod-2"; return "node-1" },
		"held":       func(s *spawneryv1alpha1.Server) string { s.Spec.Hold = true; return "node-1" },
		"node":       func(*spawneryv1alpha1.Server) string { return "node-2" },
		"unschedulable": func(s *spawneryv1alpha1.Server) string {
			s.Status.Phase = string(phase.Pending)
			withReadyReason(s, phase.ReasonPodUnschedulable)
			return ""
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			srv := base.DeepCopy()
			node := change(srv)
			if proto.Equal(netstate.Visible(srv, node, ""), want) {
				t.Errorf("a change of %s left the visible state as it was", name)
			}
		})
	}
}

// Visible is built by the same function as a picture's entry, so a field added
// to one cannot be missed by the other.
func TestVisibleAgreesWithBuildOnceTheReportedFieldsAreBlanked(t *testing.T) {
	srv := withReadyReason(readyServer("ns", "lobby-a", "lobby", 12, 100), phase.ReasonPodRunning)
	srv.Status.PodName = "lobby-a"
	srv.Status.PodUID = "pod-a"
	srv.Status.PlayableSlots = 50
	srv.Spec.Number = 3
	srv.Spec.Hold = true
	heap := resource.MustParse("5Gi")
	srv.Spec.Heap = &heap
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "ns",
			Labels: map[string]string{"spawnery.cloud/role": "server"}},
		Spec: corev1.PodSpec{NodeName: "node-2"},
	}
	src, reg := source(t, ephemeralGroup("ns", "lobby"), srv, pod)
	reg.Connect("pod-a", agent.RoleServer)
	if err := reg.ReportAnnouncement("pod-a", "ns", "lobby-a", agent.Announcement{State: "running"}); err != nil {
		t.Fatalf("ReportAnnouncement: %v", err)
	}
	if _, err := reg.ReportAcceptJoins("pod-a", "ns", false, false); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	built := proto.Clone(stateOf(t, got, "lobby-a")).(*agentpb.ServerState)
	built.Players, built.Slots, built.PlayableSlots = 0, 0, 0
	built.State, built.Attributes, built.JoinsClosed = "", nil, false
	if want := netstate.Visible(srv, "node-2", ""); !proto.Equal(built, want) {
		t.Errorf("Build with the reported fields blanked = %v, Visible = %v", built, want)
	}
}

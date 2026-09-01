/*
Copyright The Spawnery Authors.

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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
)

func source(t *testing.T, objects ...client.Object) (netstate.Source, *agent.Registry) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	start := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	reg := agent.New(func() time.Time { return start }, 5*time.Second, start)
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(objects...).Build()
	return netstate.Source{Reader: reader, Agents: reg}, reg
}

func ephemeralGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "example/paper:1",
			MaxPlayers: 100,
		},
		Status: spawneryv1alpha1.ServerGroupStatus{
			Replicas: 2, ReadyReplicas: 1, OnlinePlayers: 12, FreeSlots: 88,
		},
	}
}

func proxyGroupNamed(ns, name string) *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   1,
			Image:      "example/velocity:1",
			Expose:     spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort},
		},
		Status: spawneryv1alpha1.ProxyGroupStatus{ReadyReplicas: 1},
	}
}

func serverInPhase(ns, name, group, phase string, players, slots int32) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: group},
		},
		Status: spawneryv1alpha1.ServerStatus{
			Phase: phase, Players: players, Slots: slots, Registered: true,
		},
	}
}

func readyServer(ns, name, group string, players, slots int32) *spawneryv1alpha1.Server {
	return serverInPhase(ns, name, group, "Ready", players, slots)
}

func TestBuildDescribesEveryGroupAndServerInTheNamespace(t *testing.T) {
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		proxyGroupNamed("ns", "gateway"),
		readyServer("ns", "lobby-a", "lobby", 12, 100),
		readyServer("ns", "lobby-b", "lobby", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(got.GetGroups()) != 2 {
		t.Fatalf("groups = %v, want the ServerGroup and the ProxyGroup", got.GetGroups())
	}
	// Sorted, so this can be asserted as a list rather than a set.
	if got.GetGroups()[0].GetName() != "gateway" ||
		got.GetGroups()[0].GetKind() != agentpb.GroupState_PROXY {
		t.Errorf("groups[0] = %+v, want gateway as a PROXY group", got.GetGroups()[0])
	}
	if got.GetGroups()[1].GetKind() != agentpb.GroupState_EPHEMERAL {
		t.Errorf("lobby kind = %v, want EPHEMERAL", got.GetGroups()[1].GetKind())
	}
	if got.GetGroups()[1].GetFreeSlots() != 88 {
		t.Errorf("lobby freeSlots = %d, want the operator's own figure 88",
			got.GetGroups()[1].GetFreeSlots())
	}
	if len(got.GetServers()) != 2 {
		t.Fatalf("servers = %v, want both", got.GetServers())
	}
	if got.GetServers()[0].GetPlayers() != 12 {
		t.Errorf("lobby-a players = %d, want 12", got.GetServers()[0].GetPlayers())
	}
}

func TestBuildIsScopedToOneNamespace(t *testing.T) {
	// The property the whole API rests on -- a plugin sees its own network and
	// nothing else -- and it is one List option away from being wrong.
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-a", "lobby", 0, 100),
		ephemeralGroup("other", "secret"),
		readyServer("other", "secret-a", "secret", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, g := range got.GetGroups() {
		if g.GetName() == "secret" {
			t.Fatal("another namespace's group reached this network's state")
		}
	}
	for _, srv := range got.GetServers() {
		if srv.GetName() == "secret-a" {
			t.Fatal("another namespace's server reached this network's state")
		}
	}
}

func TestBuildCarriesTheRoster(t *testing.T) {
	// Players come from the registry and not the cache: the operator holds
	// them in memory and puts them in no object.
	src, reg := source(t, ephemeralGroup("ns", "lobby"))
	reg.Connect("proxy-a", agent.RoleProxy)
	if err := reg.ReportRoster("proxy-a", "ns", []agent.RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-a"},
	}); err != nil {
		t.Fatalf("ReportRoster: %v", err)
	}

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetPlayers()) != 1 || got.GetPlayers()[0].GetUuid() != "u-alice" {
		t.Fatalf("players = %v, want alice", got.GetPlayers())
	}
}

func TestBuildSurvivesANetworkWithNoProxyReports(t *testing.T) {
	// An empty roster is a state and not an error. A namespace whose proxies
	// have not reported has no player list, and the API documents that as
	// ordinary rather than as a failure a plugin has to handle.
	src, _ := source(t, ephemeralGroup("ns", "lobby"))

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build with no proxy reports: %v", err)
	}
	if len(got.GetPlayers()) != 0 {
		t.Errorf("players = %v, want none", got.GetPlayers())
	}
}

func TestAServersPhaseTravelsAsTheOperatorSpellsIt(t *testing.T) {
	// Not mapped to an enum here. A phase this proto predates has to reach an
	// agent as itself, so the agent can decide it does not know it -- which is
	// what ServerPhase.fromWire in the plugin API is for.
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		serverInPhase("ns", "lobby-a", "lobby", "Retiring", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetPhase() != "Retiring" {
		t.Errorf("phase = %q, want the operator's own spelling", got.GetServers()[0].GetPhase())
	}
}

func TestBuildCarriesWhatAServerSaysAboutItself(t *testing.T) {
	// From the registry and from no object, for a different reason than the
	// roster is: this is the server's own word, the operator never acts on it,
	// and a status field would mean an etcd write every time a game changed
	// what it was doing.
	src, reg := source(t,
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-a", "lobby", 0, 100),
		readyServer("ns", "lobby-b", "lobby", 0, 100),
	)
	reg.Connect("pod-a", agent.RoleServer)
	if err := reg.ReportAnnouncement("pod-a", "ns", "lobby-a", agent.Announcement{
		State:      "running",
		Attributes: map[string]string{"map": "arena"},
	}); err != nil {
		t.Fatalf("ReportAnnouncement: %v", err)
	}

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetServers()) != 2 {
		t.Fatalf("servers = %v, want both", got.GetServers())
	}
	if got.GetServers()[0].GetState() != "running" ||
		got.GetServers()[0].GetAttributes()["map"] != "arena" {
		t.Errorf("lobby-a = %+v, want what it announced", got.GetServers()[0])
	}
	// The one that announced nothing is described as nothing, not as its
	// neighbour: an announcement is attached by name, and a mapping that lost
	// the name would show every server the last one's description.
	if got.GetServers()[1].GetState() != "" || len(got.GetServers()[1].GetAttributes()) != 0 {
		t.Errorf("lobby-b = %+v, want an empty description", got.GetServers()[1])
	}
}

func TestAServerThatAnnouncedNothingIsDescribedAsNothing(t *testing.T) {
	// The picture a network has before anything on it announces, and the
	// picture every network has while its agents predate the verb. They are
	// the same picture on purpose -- a plugin that had to tell them apart
	// would be asking about the agent rather than about the game.
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-a", "lobby", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetState() != "" {
		t.Errorf("state = %q, want empty", got.GetServers()[0].GetState())
	}
}

func TestBuildCarriesWhatSomebodyWroteDownAboutAGroup(t *testing.T) {
	// From the spec and not the status: nobody derived this, a person wrote it
	// in the group's own definition, and the operator's whole part in it is to
	// carry it to the agents.
	group := ephemeralGroup("ns", "lobby")
	group.Spec.Attributes = map[string]string{"permission": "task.build"}
	proxy := proxyGroupNamed("ns", "gateway")
	proxy.Spec.Attributes = map[string]string{"region": "eu"}
	src, _ := source(t, group, proxy)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Sorted, so gateway is first and lobby second.
	if got.GetGroups()[0].GetAttributes()["region"] != "eu" {
		t.Errorf("gateway = %+v, want the proxy group's own attributes", got.GetGroups()[0])
	}
	// Both kinds, because a plugin reading one list should not find that half
	// of it can be described and half cannot.
	if got.GetGroups()[1].GetAttributes()["permission"] != "task.build" {
		t.Errorf("lobby = %+v, want the server group's own attributes", got.GetGroups()[1])
	}
}

func TestBuildSaysWhichRunOfAServerThisIs(t *testing.T) {
	// A persistent server keeps its name across every restart -- that name is
	// the identity of its world -- so anything asking "is this still the one I
	// meant" has to compare something else.
	srv := readyServer("ns", "survival-0", "survival", 0, 100)
	srv.Status.PodUID = "pod-7c3f"
	src, _ := source(t, ephemeralGroup("ns", "survival"), srv)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetIncarnation() != "pod-7c3f" {
		t.Errorf("incarnation = %q, want the pod the operator recorded",
			got.GetServers()[0].GetIncarnation())
	}
}

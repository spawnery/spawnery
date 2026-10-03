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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/podspec"
)

func source(t *testing.T, objects ...client.Object) (netstate.Source, *agent.Registry) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
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

func onDemandGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	maxInstances := int32(300)
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:   spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:         spawneryv1alpha1.ServerGroupOnDemand,
			Image:        "example/paper:1",
			MaxPlayers:   4,
			MaxInstances: &maxInstances,
		},
	}
}

func onDemandMember(ns, group, key string) *spawneryv1alpha1.Server {
	srv := readyServer(ns, group+"-"+key, group, 0, 4)
	srv.Spec.Key = key
	return srv
}

func hasServer(state *agentpb.NetworkState, name string) bool {
	for _, srv := range state.GetServers() {
		if srv.GetName() == name {
			return true
		}
	}
	return false
}

func hasGroup(state *agentpb.NetworkState, name string) bool {
	for _, g := range state.GetGroups() {
		if g.GetName() == name {
			return true
		}
	}
	return false
}

func TestBuildDescribesEveryGroupAndServerInTheNamespace(t *testing.T) {
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		proxyGroupNamed("ns", "gateway"),
		readyServer("ns", "lobby-a", "lobby", 12, 100),
		readyServer("ns", "lobby-b", "lobby", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(got.GetGroups()) != 2 {
		t.Fatalf("groups = %v, want the ServerGroup and the ProxyGroup", got.GetGroups())
	}
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
	// One List option away from showing a plugin another network.
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-a", "lobby", 0, 100),
		ephemeralGroup("other", "secret"),
		readyServer("other", "secret-a", "secret", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
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
	src, reg := source(t, ephemeralGroup("ns", "lobby"))
	reg.Connect("proxy-a", agent.RoleProxy)
	if err := reg.ReportRoster("proxy-a", "ns", []agent.RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-a"},
	}); err != nil {
		t.Fatalf("ReportRoster: %v", err)
	}

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetPlayers()) != 1 || got.GetPlayers()[0].GetUuid() != "u-alice" {
		t.Fatalf("players = %v, want alice", got.GetPlayers())
	}
}

func TestBuildSurvivesANetworkWithNoProxyReports(t *testing.T) {
	src, _ := source(t, ephemeralGroup("ns", "lobby"))

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build with no proxy reports: %v", err)
	}
	if len(got.GetPlayers()) != 0 {
		t.Errorf("players = %v, want none", got.GetPlayers())
	}
}

func TestAServersPhaseTravelsAsTheOperatorSpellsIt(t *testing.T) {
	// Unmapped: ServerPhase.fromWire in the plugin API decides what it knows.
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		serverInPhase("ns", "lobby-a", "lobby", "Retiring", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetPhase() != "Retiring" {
		t.Errorf("phase = %q, want the operator's own spelling", got.GetServers()[0].GetPhase())
	}
}

func TestBuildCarriesWhatAServerSaysAboutItself(t *testing.T) {
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

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
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
	if got.GetServers()[1].GetState() != "" || len(got.GetServers()[1].GetAttributes()) != 0 {
		t.Errorf("lobby-b = %+v, want an empty description", got.GetServers()[1])
	}
}

func TestAServerThatAnnouncedNothingIsDescribedAsNothing(t *testing.T) {
	src, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-a", "lobby", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetState() != "" {
		t.Errorf("state = %q, want empty", got.GetServers()[0].GetState())
	}
}

func TestBuildCarriesWhatSomebodyWroteDownAboutAGroup(t *testing.T) {
	group := ephemeralGroup("ns", "lobby")
	group.Spec.Attributes = map[string]string{"permission": "task.build"}
	proxy := proxyGroupNamed("ns", "gateway")
	proxy.Spec.Attributes = map[string]string{"region": "eu"}
	src, _ := source(t, group, proxy)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Sorted, so gateway is first and lobby second.
	if got.GetGroups()[0].GetAttributes()["region"] != "eu" {
		t.Errorf("gateway = %+v, want the proxy group's own attributes", got.GetGroups()[0])
	}
	if got.GetGroups()[1].GetAttributes()["permission"] != "task.build" {
		t.Errorf("lobby = %+v, want the server group's own attributes", got.GetGroups()[1])
	}
}

func TestBuildSaysWhichRunOfAServerThisIs(t *testing.T) {
	srv := readyServer("ns", "survival-0", "survival", 0, 100)
	srv.Status.PodUID = "pod-7c3f"
	src, _ := source(t, ephemeralGroup("ns", "survival"), srv)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetIncarnation() != "pod-7c3f" {
		t.Errorf("incarnation = %q, want the pod the operator recorded",
			got.GetServers()[0].GetIncarnation())
	}
}

func TestBuildCarriesAGroupsDisplayName(t *testing.T) {
	group := ephemeralGroup("ns", "bingo-team")
	group.Spec.DisplayName = "Bingo-Team"
	proxy := proxyGroupNamed("ns", "gateway")
	proxy.Spec.DisplayName = "Gateway"
	src, _ := source(t, group, proxy)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Sorted, so bingo-team is first and gateway second.
	if got.GetGroups()[0].GetDisplayName() != "Bingo-Team" {
		t.Errorf("bingo-team = %+v, want its display name", got.GetGroups()[0])
	}
	if got.GetGroups()[1].GetDisplayName() != "Gateway" {
		t.Errorf("gateway = %+v, want the proxy group's display name", got.GetGroups()[1])
	}
}

func TestBuildCarriesAGroupsAdmission(t *testing.T) {
	seats := int32(12)
	enforced := ephemeralGroup("ns", "duels")
	enforced.Spec.PlayableSlots = &seats
	enforced.Spec.EnforcePlayableSlots = true
	src, _ := source(t, enforced, ephemeralGroup("ns", "lobby"))

	got, err := src.Build(context.Background(), "ns", netstate.ForServers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Sorted: duels, then lobby.
	if g := got.GetGroups()[0]; g.GetPlayableSlots() != 12 || !g.GetEnforcePlayableSlots() {
		t.Errorf("duels = %+v, want playable slots 12 and enforcement", g)
	}
	if g := got.GetGroups()[1]; g.GetPlayableSlots() != 0 || g.GetEnforcePlayableSlots() {
		t.Errorf("lobby = %+v, want neither", g)
	}
}

func TestBuildCarriesAGroupsJoinRule(t *testing.T) {
	required := ephemeralGroup("ns", "build")
	required.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{}
	denyOnly := ephemeralGroup("ns", "hub")
	denyOnly.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{
		Node: "network.banned", Mode: spawneryv1alpha1.JoinPermissionDenyOnly,
	}
	src, _ := source(t, required, denyOnly, ephemeralGroup("ns", "lobby"))

	got, err := src.Build(context.Background(), "ns", netstate.ForServers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Sorted: build, hub, lobby.
	if g := got.GetGroups()[0]; g.GetJoinPermission() != "spawnery.join.build" || g.GetJoinPermissionDenyOnly() {
		t.Errorf("build = %+v, want spawnery.join.build, required", g)
	}
	if g := got.GetGroups()[1]; g.GetJoinPermission() != "network.banned" || !g.GetJoinPermissionDenyOnly() {
		t.Errorf("hub = %+v, want network.banned, deny-only", g)
	}
	if g := got.GetGroups()[2]; g.GetJoinPermission() != "" {
		t.Errorf("lobby = %+v, want no rule", g)
	}
}

func TestAnOnDemandGroupsJoinRuleReachesTheProxies(t *testing.T) {
	private := onDemandGroup("ns", "realms")
	private.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{}
	src, _ := source(t, private)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetGroups()) != 1 || got.GetGroups()[0].GetJoinPermission() != "spawnery.join.realms" {
		t.Errorf("groups = %+v, want realms with spawnery.join.realms", got.GetGroups())
	}
}

func TestAGroupWithoutADisplayNameTravelsWithAnEmptyOne(t *testing.T) {
	// Which name stands in for a missing display name is the reader's decision.
	src, _ := source(t, ephemeralGroup("ns", "lobby"))

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetGroups()[0].GetDisplayName() != "" {
		t.Errorf("display name = %q, want empty", got.GetGroups()[0].GetDisplayName())
	}
}

func TestBuildCarriesAServersNumber(t *testing.T) {
	numbered := readyServer("ns", "hub-dvjk", "hub", 3, 100)
	numbered.Spec.Number = 1
	// A server from before the field existed.
	old := readyServer("ns", "hub-old1", "hub", 0, 100)
	src, _ := source(t, ephemeralGroup("ns", "hub"), numbered, old)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	numbers := map[string]int32{}
	for _, s := range got.GetServers() {
		numbers[s.GetName()] = s.GetNumber()
	}
	if numbers["hub-dvjk"] != 1 {
		t.Errorf("hub-dvjk = %d, want 1", numbers["hub-dvjk"])
	}
	if numbers["hub-old1"] != 0 {
		t.Errorf("hub-old1 = %d, want 0", numbers["hub-old1"])
	}
}

func TestOnDemandMembersReachProxiesOnly(t *testing.T) {
	src, _ := source(t,
		onDemandGroup("ns", "private-servers"),
		onDemandMember("ns", "private-servers", "c0ffee"),
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-abc", "lobby", 0, 100),
	)

	forProxies, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !hasServer(forProxies, "private-servers-c0ffee") {
		t.Error("a proxy cannot route to a private server it cannot see")
	}
	if !hasGroup(forProxies, "private-servers") {
		t.Fatal("the on-demand group is missing from the proxies' picture")
	}
	for _, g := range forProxies.GetGroups() {
		if g.GetName() == "private-servers" && g.GetKind() != agentpb.GroupState_ON_DEMAND {
			t.Errorf("kind = %v, want ON_DEMAND: an agent cannot read a group as unspecified "+
				"when this build knows the type", g.GetKind())
		}
	}

	forServers, err := src.Build(context.Background(), "ns", netstate.ForServers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if hasServer(forServers, "private-servers-c0ffee") {
		t.Error("every lobby is carrying an entry for a server nobody will be sent to")
	}
	if hasGroup(forServers, "private-servers") {
		t.Error("the on-demand group itself reached a backend's picture")
	}
	if !hasServer(forServers, "lobby-abc") || !hasGroup(forServers, "lobby") {
		t.Error("an ordinary group or server fell out of the backends' picture")
	}
}

func TestABackendIsToldAPlayerIsOnlineAndNotThatTheyAreOnAPrivateServer(t *testing.T) {
	src, reg := source(t,
		onDemandGroup("ns", "private-servers"),
		onDemandMember("ns", "private-servers", "c0ffee"),
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-abc", "lobby", 0, 100),
	)
	reg.Connect("proxy-a", agent.RoleProxy)
	if err := reg.ReportRoster("proxy-a", "ns", []agent.RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "private-servers-c0ffee"},
		{UUID: "u-bob", Name: "bob", Server: "lobby-abc"},
	}); err != nil {
		t.Fatalf("ReportRoster: %v", err)
	}

	forServers, err := src.Build(context.Background(), "ns", netstate.ForServers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	where := map[string]string{}
	for _, p := range forServers.GetPlayers() {
		where[p.GetUuid()] = p.GetServer()
	}
	if len(where) != 2 {
		t.Fatalf("players = %v, want both: a player on a private server is still on the network",
			forServers.GetPlayers())
	}
	if where["u-alice"] != "" {
		t.Errorf("alice is on %q in a backend's picture, which its own servers() does not list -- "+
			"and stopServer takes that name", where["u-alice"])
	}
	if where["u-bob"] != "lobby-abc" {
		t.Errorf("bob is on %q, want lobby-abc", where["u-bob"])
	}

	forProxies, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, p := range forProxies.GetPlayers() {
		if p.GetUuid() == "u-alice" && p.GetServer() != "private-servers-c0ffee" {
			t.Errorf("alice is on %q in the proxies' picture, which is the one that routes",
				p.GetServer())
		}
	}
}

func TestOnlyAMemberOfAnOnDemandGroupIsAPrivateServer(t *testing.T) {
	if !netstate.IsPrivateServer(onDemandMember("ns", "private-servers", "c0ffee")) {
		t.Error("a member carrying a key was not a private server")
	}
	if netstate.IsPrivateServer(readyServer("ns", "lobby-a", "lobby", 0, 100)) {
		t.Error("an ordinary server was a private server")
	}
}

func TestAudienceOfSendsOnlyProxiesTheWholePicture(t *testing.T) {
	if netstate.AudienceOf(agent.RoleProxy) != netstate.ForProxies {
		t.Error("a proxy was given the narrower picture, and cannot route to a private server")
	}
	if netstate.AudienceOf(agent.RoleServer) != netstate.ForServers {
		t.Error("a backend was given the whole picture")
	}
	if netstate.AudienceOf(agent.Role("something-new")) != netstate.ForServers {
		t.Error("an unknown role was given the whole picture")
	}
}

func proxyPodIn(ns, name string, ready, draining bool) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, UID: types.UID(name + "-uid"),
		Labels: podspec.ProxyLabels("production", "gateway"),
	}}
	if draining {
		pod.Annotations = map[string]string{podspec.AnnotationProxyDrainingSince: "2026-09-26T12:00:00Z"}
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return pod
}

func TestBuildListsEveryProxy(t *testing.T) {
	src, reg := source(t,
		proxyGroupNamed("ns", "gateway"),
		proxyPodIn("ns", "gateway-a", true, false),
		proxyPodIn("ns", "gateway-b", true, true),
		proxyPodIn("other", "gateway-x", true, false),
	)
	reg.Connect("gateway-a-uid", agent.RoleProxy)
	if err := reg.ReportPlayers("gateway-a-uid", 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetProxies()) != 2 {
		t.Fatalf("proxies = %v, want the two in this namespace", got.GetProxies())
	}
	a, b := got.GetProxies()[0], got.GetProxies()[1]
	if a.GetName() != "gateway-a" || !a.GetReady() || a.GetDraining() || a.GetPlayers() != 3 || a.GetGroup() != "gateway" {
		t.Errorf("gateway-a = %+v", a)
	}
	if b.GetName() != "gateway-b" || !b.GetDraining() {
		t.Errorf("gateway-b = %+v, want it draining", b)
	}
}

func TestBuildSaysWhichProxiesAcceptTransfers(t *testing.T) {
	transferring := proxyPodIn("ns", "gateway-a", true, false)
	transferring.Spec.Containers = []corev1.Container{{
		Name: podspec.ProxyContainerName,
		Env:  []corev1.EnvVar{{Name: podspec.EnvTransferForceAfterSeconds, Value: "120"}},
	}}
	refusing := proxyPodIn("ns", "gateway-b", true, false)
	refusing.Spec.Containers = []corev1.Container{{Name: podspec.ProxyContainerName}}
	src, _ := source(t, proxyGroupNamed("ns", "gateway"), transferring, refusing)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a, b := got.GetProxies()[0], got.GetProxies()[1]
	if !a.GetAcceptsTransfers() {
		t.Errorf("gateway-a = %+v, want accepts_transfers", a)
	}
	if b.GetAcceptsTransfers() {
		t.Errorf("gateway-b = %+v, want transfers refused", b)
	}
}

func TestBuildNamesTheNodeAServerAndAProxyRunOn(t *testing.T) {
	srv := readyServer("ns", "lobby-a", "lobby", 0, 100)
	srv.Status.PodName = "lobby-a"
	unplaced := readyServer("ns", "lobby-b", "lobby", 0, 100)
	unplaced.Status.PodName = "lobby-b"
	serverPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "ns",
			Labels: map[string]string{podspec.LabelRole: podspec.RoleServer, podspec.LabelGroup: "lobby"}},
		Spec: corev1.PodSpec{NodeName: "node-2"},
	}
	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-b", Namespace: "ns",
			Labels: map[string]string{podspec.LabelRole: podspec.RoleServer, podspec.LabelGroup: "lobby"}},
	}
	proxyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-a", Namespace: "ns",
			Labels: map[string]string{podspec.LabelRole: podspec.RoleProxy, podspec.LabelGroup: "gateway"}},
		Spec: corev1.PodSpec{NodeName: "node-3"},
	}
	src, _ := source(t, ephemeralGroup("ns", "lobby"), srv, unplaced, serverPod, pendingPod, proxyPod)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	nodes := map[string]string{}
	for _, s := range got.GetServers() {
		nodes[s.GetName()] = s.GetNode()
	}
	if nodes["lobby-a"] != "node-2" || nodes["lobby-b"] != "" {
		t.Errorf("server nodes = %v, want lobby-a on node-2 and lobby-b unscheduled", nodes)
	}
	if p := got.GetProxies(); len(p) != 1 || p[0].GetNode() != "node-3" {
		t.Errorf("proxies = %v, want gateway-a on node-3", p)
	}
}

func TestBuildCarriesThePlayableFigure(t *testing.T) {
	srv := readyServer("ns", "duels-a", "lobby", 14, 100)
	srv.Status.PlayableSlots = 12
	src, _ := source(t, ephemeralGroup("ns", "lobby"), srv)

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n := got.GetServers()[0].GetPlayableSlots(); n != 12 {
		t.Errorf("playable_slots = %d, want 12", n)
	}
}

func TestBuildCarriesAClosedDoor(t *testing.T) {
	lobbyA := readyServer("ns", "lobby-a", "lobby", 0, 100)
	lobbyA.Status.PodUID = "pod-a"
	lobbyB := readyServer("ns", "lobby-b", "lobby", 0, 100)
	lobbyB.Status.PodUID = "pod-b"
	src, reg := source(t, ephemeralGroup("ns", "lobby"), lobbyA, lobbyB)
	reg.Connect("pod-a", agent.RoleServer)
	if _, err := reg.ReportAcceptJoins("pod-a", "ns", false, false); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !got.GetServers()[0].GetJoinsClosed() {
		t.Errorf("lobby-a = %+v, want joins_closed", got.GetServers()[0])
	}
	if got.GetServers()[1].GetJoinsClosed() {
		t.Errorf("lobby-b = %+v, want joins open", got.GetServers()[1])
	}
}

func TestAClosedDoorFollowsTheCurrentPodNotTheServerName(t *testing.T) {
	srv := readyServer("ns", "survival-0", "survival", 0, 100)
	srv.Status.PodUID = "new-pod"
	src, reg := source(t, ephemeralGroup("ns", "survival"), srv)

	reg.Connect("old-pod", agent.RoleServer)
	if _, err := reg.ReportAcceptJoins("old-pod", "ns", false, false); err != nil {
		t.Fatalf("ReportAcceptJoins(old-pod): %v", err)
	}
	reg.Connect("new-pod", agent.RoleServer)
	if _, err := reg.ReportAcceptJoins("new-pod", "ns", true, false); err != nil {
		t.Fatalf("ReportAcceptJoins(new-pod): %v", err)
	}

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetJoinsClosed() {
		t.Errorf("survival-0 = %+v, want the current pod's open door", got.GetServers()[0])
	}
}

func TestAClosedDoorOnTheCurrentPodIsCarried(t *testing.T) {
	srv := readyServer("ns", "survival-0", "survival", 0, 100)
	srv.Status.PodUID = "pod-a"
	src, reg := source(t, ephemeralGroup("ns", "survival"), srv)

	reg.Connect("pod-a", agent.RoleServer)
	if _, err := reg.ReportAcceptJoins("pod-a", "ns", false, false); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !got.GetServers()[0].GetJoinsClosed() {
		t.Errorf("survival-0 = %+v, want the current pod's closed door", got.GetServers()[0])
	}
}

func TestAClosedDoorOnTheCurrentPodSurvivesADisconnect(t *testing.T) {
	srv := readyServer("ns", "survival-0", "survival", 0, 100)
	srv.Status.PodUID = "pod-a"
	src, reg := source(t, ephemeralGroup("ns", "survival"), srv)

	reg.Connect("pod-a", agent.RoleServer)
	if _, err := reg.ReportAcceptJoins("pod-a", "ns", false, false); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}
	reg.Disconnect("pod-a")

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !got.GetServers()[0].GetJoinsClosed() {
		t.Errorf("survival-0 = %+v, want the closed door to survive a disconnect", got.GetServers()[0])
	}
}

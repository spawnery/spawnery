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
	"fmt"
	"strings"
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
	"github.com/spawnery/spawnery/internal/netstate"
)

func networkWith(players []*agentpb.RosterEntry, servers []*agentpb.ServerState) *agentpb.NetworkState {
	return &agentpb.NetworkState{Players: players, Servers: servers}
}

func TestATargetThisNetworkDoesNotHaveIsNotFound(t *testing.T) {
	state := networkWith(
		[]*agentpb.RosterEntry{{Uuid: "u-alice", Name: "alice", Server: "lobby-a"}},
		[]*agentpb.ServerState{{Name: "lobby-a", Group: "lobby", Registered: true}},
	)

	if _, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Server{Server: "somebody-elses-server"},
	}); ok {
		t.Error("a server this network does not have resolved anyway")
	}
}

// connectFixture is a namespace with an on-demand group and three of its
// members -- one running, one still starting, one in another namespace -- next
// to an ordinary group, and a player on a proxy's roster.
func connectFixture(t *testing.T) (
	ask func(grpcauth.Identity, *agentpb.ConnectRequest) *agentpb.CloudResponse,
	named func(string) *agentpb.ConnectRequest,
	proxy, backend grpcauth.Identity,
) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	maxInstances := int32(10)
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers", Namespace: "ns"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type: spawneryv1alpha1.ServerGroupOnDemand, MaxInstances: &maxInstances,
		},
	}
	member := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-c0ffee", Namespace: "ns"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"},
			Key:      "c0ffee",
		},
		Status: spawneryv1alpha1.ServerStatus{Phase: "Ready", Slots: 4, Registered: true},
	}
	elsewhere := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "other-secret", Namespace: "other"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"},
			Key:      "secret",
		},
		Status: spawneryv1alpha1.ServerStatus{Phase: "Ready", Slots: 4, Registered: true},
	}
	starting := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-d00d", Namespace: "ns"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"},
			Key:      "d00d",
		},
		Status: spawneryv1alpha1.ServerStatus{Phase: "Starting", Slots: 4, Registered: false},
	}
	lobby := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupEphemeral},
	}
	lobbyC := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-c", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
		Status:     spawneryv1alpha1.ServerStatus{Phase: "Ready", Slots: 100, Registered: true},
	}
	ordinary := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-b", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
		Status:     spawneryv1alpha1.ServerStatus{Phase: "Ready", Slots: 100, Registered: false},
	}
	registry := agent.New(time.Now, time.Minute, time.Now())
	registry.Connect("proxy-a", agent.RoleProxy)
	if err := registry.ReportRoster("proxy-a", "ns", []agent.RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-a"},
	}); err != nil {
		t.Fatalf("ReportRoster: %v", err)
	}
	s := &Server{
		opts: Options{
			Agents:  registry,
			Proxies: stubFleet{},
			State: netstate.Source{
				Reader: fake.NewClientBuilder().WithScheme(scheme).
					WithStatusSubresource(&spawneryv1alpha1.Server{}).
					WithObjects(group, member, starting, elsewhere, lobby, lobbyC, ordinary).Build(),
				Agents: registry,
			},
		},
		requestRate: newRequestLimiter(time.Now),
	}
	ask = func(id grpcauth.Identity, target *agentpb.ConnectRequest) *agentpb.CloudResponse {
		target.PlayerUuid = "u-alice"
		return s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
			Id:      1,
			Request: &agentpb.CloudRequest_Connect{Connect: target},
		})
	}
	named = func(server string) *agentpb.ConnectRequest {
		return &agentpb.ConnectRequest{Target: &agentpb.ConnectRequest_Server{Server: server}}
	}
	proxy = grpcauth.Identity{Namespace: "ns", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy}
	backend = grpcauth.Identity{Namespace: "ns", PodName: "lobby-a", PodUID: "pod-a", Role: agent.RoleServer}
	return
}

func TestAConnectResolvesAgainstThePictureOfWhoAsked(t *testing.T) {
	ask, named, proxy, backend := connectFixture(t)

	if got := ask(proxy, named("private-servers-c0ffee")); !got.GetConnect().GetOrdered() {
		t.Errorf("proxy's response = %+v, want the move ordered", got)
	}

	got := ask(backend, named("private-servers-c0ffee"))
	if got.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Errorf("backend naming a private server: response = %+v, want REFUSED", got)
	}
	if !strings.Contains(got.GetError().GetMessage(), "proxy") {
		t.Errorf("message = %q, want it to say a private server is addressed through a proxy",
			got.GetError().GetMessage())
	}

	for name, target := range map[string]*agentpb.ConnectRequest{
		"a server that does not exist":        named("private-servers-nobody"),
		"a group that does not exist":         {Target: &agentpb.ConnectRequest_Group{Group: "nobody"}},
		"a private server of another network": named("other-secret"),
		"an ordinary server, unregistered":    named("lobby-b"),
	} {
		got := ask(backend, target)
		if got.GetError().GetReason() != agentpb.RequestError_NOT_FOUND {
			t.Errorf("backend naming %s: response = %+v, want NOT_FOUND", name, got)
		}
	}
}

func TestAPrivateServerNotYetRunningIsStillAddressedThroughAProxy(t *testing.T) {
	ask, named, proxy, backend := connectFixture(t)

	if got := ask(proxy, named("private-servers-d00d")); got.GetError().GetReason() != agentpb.RequestError_NOT_FOUND {
		t.Errorf("proxy naming a private server not yet registered: response = %+v, want NOT_FOUND", got)
	}

	got := ask(backend, named("private-servers-d00d"))
	if got.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("backend naming a private server not yet registered: response = %+v, want REFUSED", got)
	}
	for _, want := range []string{"proxy", "running"} {
		if !strings.Contains(got.GetError().GetMessage(), want) {
			t.Errorf("message = %q, want it to mention %q", got.GetError().GetMessage(), want)
		}
	}
}

func TestAGroupTargetNeverOpensAPrivateServer(t *testing.T) {
	ask, _, proxy, backend := connectFixture(t)
	onDemand := &agentpb.ConnectRequest{Target: &agentpb.ConnectRequest_Group{Group: "private-servers"}}

	for name, id := range map[string]grpcauth.Identity{"proxy": proxy, "backend": backend} {
		got := ask(id, onDemand)
		if got.GetError().GetReason() != agentpb.RequestError_REFUSED {
			t.Errorf("%s naming an on-demand group: response = %+v, want REFUSED", name, got)
			continue
		}
		if !strings.Contains(got.GetError().GetMessage(), "by name") {
			t.Errorf("%s: message = %q, want it to say the members are addressed by name",
				name, got.GetError().GetMessage())
		}
	}

	got := ask(proxy, &agentpb.ConnectRequest{Target: &agentpb.ConnectRequest_Group{Group: "lobby"}})
	if !got.GetConnect().GetOrdered() || got.GetConnect().GetTarget() != "lobby-c" {
		t.Errorf("proxy naming an ordinary group: response = %+v, want a move to lobby-c", got)
	}
}

func TestAnUnregisteredTargetIsRefusedEvenThoughItExists(t *testing.T) {
	state := networkWith(nil,
		[]*agentpb.ServerState{{Name: "lobby-a", Group: "lobby", Registered: false}},
	)

	if _, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Server{Server: "lobby-a"},
	}); ok {
		t.Error("an unregistered server was accepted as a move target")
	}
}

func TestAGroupTargetPicksTheServerWithTheMostRoom(t *testing.T) {
	state := networkWith(nil, []*agentpb.ServerState{
		{Name: "lobby-a", Group: "lobby", Players: 90, Slots: 100, Registered: true},
		{Name: "lobby-b", Group: "lobby", Players: 10, Slots: 100, Registered: true},
		// Another group's emptier server must not win.
		{Name: "arena-a", Group: "arena", Players: 0, Slots: 100, Registered: true},
	})

	got, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Group{Group: "lobby"},
	})

	if !ok || got != "lobby-b" {
		t.Errorf("target = %q ok=%v, want lobby-b: a group means wherever that group has room", got, ok)
	}
}

func TestAGroupWithNoRegisteredServerResolvesToNothing(t *testing.T) {
	state := networkWith(nil, []*agentpb.ServerState{
		{Name: "lobby-a", Group: "lobby", Registered: false},
	})

	if _, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Group{Group: "lobby"},
	}); ok {
		t.Error("a group whose every server is unroutable resolved to one anyway")
	}
}

func TestRequestsPastTheBurstAreRefused(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRequestLimiter(func() time.Time { return now })

	for i := 0; i < RequestBurst; i++ {
		if !l.allow("pod-a") {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	if l.allow("pod-a") {
		t.Error("a request past the burst was allowed")
	}
}

func TestOnePodsBurstIsNotAnothersOnesBudget(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRequestLimiter(func() time.Time { return now })

	for i := 0; i < RequestBurst+2; i++ {
		l.allow("noisy")
	}

	if !l.allow("quiet") {
		t.Error("one pod exhausting its budget refused another pod's first request")
	}
}

func TestTheBucketRefills(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRequestLimiter(func() time.Time { return now })
	for i := 0; i < RequestBurst; i++ {
		l.allow("pod-a")
	}
	if l.allow("pod-a") {
		t.Fatal("the bucket was not empty")
	}

	now = now.Add(RequestRefill)

	if !l.allow("pod-a") {
		t.Error("a token did not come back after the refill interval")
	}
}

func TestRefilledBucketsAreSweptOnceTheMapIsFull(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newRequestLimiter(func() time.Time { return now })
	l.maxBuckets = 4
	for i := 0; i < 4; i++ {
		l.allow(fmt.Sprintf("pod-%d", i))
	}
	// Refill everything, then a fifth pod arrives.
	now = now.Add(time.Duration(RequestBurst) * RequestRefill)
	l.allow("pod-4")

	if got := len(l.buckets); got != 1 {
		t.Errorf("%d buckets after the sweep, want 1: the refilled ones are indistinguishable "+
			"from pods that never asked", got)
	}
	if _, ok := l.buckets["pod-4"]; !ok {
		t.Error("the pod that triggered the sweep lost its own bucket")
	}
}

func announcement(state string, attributes map[string]string) *agentpb.AnnounceRequest {
	return &agentpb.AnnounceRequest{State: state, Attributes: attributes}
}

func TestAnAnnouncementWithinItsBoundsIsAccepted(t *testing.T) {
	if message, ok := announcementRefusal(announcement("running",
		map[string]string{"map": "arena"})); !ok {
		t.Errorf("an ordinary announcement was refused: %s", message)
	}
	if _, ok := announcementRefusal(announcement("", nil)); !ok {
		t.Error("clearing a description was refused")
	}
}

func TestAStateLongerThanTheOperatorCarriesIsRefused(t *testing.T) {
	long := strings.Repeat("x", AnnounceMaxStateLength+1)

	message, ok := announcementRefusal(announcement(long, nil))
	if ok {
		t.Fatal("an oversized state was accepted")
	}
	if !strings.Contains(message, "64") {
		t.Errorf("refusal = %q, want it to name the bound", message)
	}
}

func TestMoreAttributesThanTheOperatorCarriesAreRefused(t *testing.T) {
	attributes := make(map[string]string)
	for i := 0; i <= AnnounceMaxAttributes; i++ {
		attributes[fmt.Sprintf("key-%d", i)] = "v"
	}

	if message, ok := announcementRefusal(announcement("", attributes)); ok {
		t.Error("too many attributes were accepted")
	} else if !strings.Contains(message, "attributes") {
		t.Errorf("refusal = %q, want it to name what was too many", message)
	}
}

func TestAnAttributeNameOrValueBeyondTheBoundIsRefused(t *testing.T) {
	if _, ok := announcementRefusal(announcement("",
		map[string]string{strings.Repeat("k", AnnounceMaxKeyLength+1): "v"})); ok {
		t.Error("an oversized attribute name was accepted")
	}
	message, ok := announcementRefusal(announcement("",
		map[string]string{"map": strings.Repeat("v", AnnounceMaxValueLength+1)}))
	if ok {
		t.Fatal("an oversized attribute value was accepted")
	}
	if !strings.Contains(message, `"map"`) {
		t.Errorf("refusal = %q, want it to name the attribute", message)
	}
}

func TestAnAttributeWithNoNameIsRefused(t *testing.T) {
	if _, ok := announcementRefusal(announcement("", map[string]string{"": "v"})); ok {
		t.Error("a nameless attribute was accepted")
	}
}

func TestAnAnnouncementIsStoredUnderTheIdentitysOwnName(t *testing.T) {
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("pod-a", agent.RoleServer)
	// Not New, which needs the fleets and certificates this verb never reaches.
	s := &Server{opts: Options{Agents: registry, Proxies: stubFleet{}}, requestRate: newRequestLimiter(time.Now)}

	response := s.answerCloudRequest(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "lobby-a", PodUID: "pod-a", Role: agent.RoleServer},
		&agentpb.CloudRequest{
			Id:      7,
			Request: &agentpb.CloudRequest_Announce{Announce: announcement("running", nil)},
		})

	if response.GetAnnounce() == nil {
		t.Fatalf("response = %+v, want an accepted announcement", response)
	}
	if response.GetId() != 7 {
		t.Errorf("id = %d, want the request's own", response.GetId())
	}
	if got := registry.Announcements("ns")["lobby-a"].State; got != "running" {
		t.Errorf("stored state = %q, want it under the identity's name", got)
	}
}

func TestAProxyAnnouncementIsRefusedRatherThanDropped(t *testing.T) {
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("proxy-a", agent.RoleProxy)
	s := &Server{opts: Options{Agents: registry, Proxies: stubFleet{}}, requestRate: newRequestLimiter(time.Now)}

	response := s.answerCloudRequest(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy},
		&agentpb.CloudRequest{
			Id:      1,
			Request: &agentpb.CloudRequest_Announce{Announce: announcement("running", nil)},
		})

	if response.GetError() == nil {
		t.Fatalf("response = %+v, want a refusal", response)
	}
	if response.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Errorf("reason = %v, want REFUSED", response.GetError().GetReason())
	}
}

func TestAServerClosesItsOwnDoorAndNobodyElses(t *testing.T) {
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("pod-a", agent.RoleServer)
	registry.Connect("pod-b", agent.RoleServer)
	s := &Server{opts: Options{Agents: registry, Proxies: stubFleet{}}, requestRate: newRequestLimiter(time.Now)}

	response := s.answerCloudRequest(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "lobby-a", PodUID: "pod-a", Role: agent.RoleServer},
		&agentpb.CloudRequest{
			Id: 3,
			Request: &agentpb.CloudRequest_AcceptJoins{
				AcceptJoins: &agentpb.AcceptJoinsRequest{Accept: false},
			},
		})

	if response.GetAcceptJoins() == nil {
		t.Fatalf("response = %+v, want the door closed", response)
	}
	if registry.Lookup("pod-a").AcceptingJoins {
		t.Error("the door of the server that asked is still open")
	}
	if !registry.Lookup("pod-b").AcceptingJoins {
		t.Error("one server's request closed another server's door")
	}
}

type stateRecorder struct {
	stubFleet
	namespaces []string
}

func (r *stateRecorder) SendState(_ context.Context, namespace string) {
	r.namespaces = append(r.namespaces, namespace)
}

func TestADoorThatMovesReachesTheProxiesAtOnce(t *testing.T) {
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("pod-a", agent.RoleServer)
	proxies := &stateRecorder{}
	s := &Server{opts: Options{Agents: registry, Proxies: proxies}, requestRate: newRequestLimiter(time.Now)}
	id := grpcauth.Identity{Namespace: "ns", PodName: "lobby-a", PodUID: "pod-a", Role: agent.RoleServer}
	door := func(reqID uint64, accept bool) {
		response := s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
			Id:      reqID,
			Request: &agentpb.CloudRequest_AcceptJoins{AcceptJoins: &agentpb.AcceptJoinsRequest{Accept: accept}},
		})
		if response.GetAcceptJoins() == nil {
			t.Fatalf("response = %+v, want accepted", response)
		}
	}

	door(1, false)
	if len(proxies.namespaces) != 1 || proxies.namespaces[0] != "ns" {
		t.Fatalf("after closing: state pushes = %v, want one to ns", proxies.namespaces)
	}
	door(2, false)
	if len(proxies.namespaces) != 1 {
		t.Fatalf("a repeated close pushed again: %v", proxies.namespaces)
	}
	door(3, true)
	if len(proxies.namespaces) != 2 {
		t.Fatalf("after opening: state pushes = %v, want a second one", proxies.namespaces)
	}
}

func TestAServerSaysItsRoundIsOver(t *testing.T) {
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("pod-a", agent.RoleServer)
	s := &Server{opts: Options{Agents: registry, Proxies: stubFleet{}}, requestRate: newRequestLimiter(time.Now)}

	response := s.answerCloudRequest(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "arena-a", PodUID: "pod-a", Role: agent.RoleServer},
		&agentpb.CloudRequest{
			Id: 5,
			Request: &agentpb.CloudRequest_AcceptJoins{
				AcceptJoins: &agentpb.AcceptJoinsRequest{Accept: false, RoundEnded: true},
			},
		})

	if response.GetAcceptJoins() == nil {
		t.Fatalf("response = %+v, want the round recorded", response)
	}
	if !registry.Lookup("pod-a").RoundEnded {
		t.Error("the server said its round was over and the registry never heard it")
	}
}

func TestAProxyIsRefusedADoor(t *testing.T) {
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("proxy-a", agent.RoleProxy)
	s := &Server{opts: Options{Agents: registry, Proxies: stubFleet{}}, requestRate: newRequestLimiter(time.Now)}

	response := s.answerCloudRequest(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy},
		&agentpb.CloudRequest{
			Id: 4,
			Request: &agentpb.CloudRequest_AcceptJoins{
				AcceptJoins: &agentpb.AcceptJoinsRequest{Accept: false},
			},
		})

	if response.GetError() == nil ||
		response.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("response = %+v, want a refusal with a reason", response)
	}
}

func TestStartRefusesAnOnDemandGroupWithNoCeiling(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	unbounded := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupOnDemand},
	}
	w := KubeWriter{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(unbounded).Build(),
		Clock:  time.Now,
	}

	_, err := w.StartServer(context.Background(), "ns", "private-servers", "c0ffee")
	if !errors.Is(err, ErrNoCeiling) {
		t.Fatalf("err = %v, want ErrNoCeiling: an unbounded group started a member", err)
	}
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: "private-servers-c0ffee"}, &srv); err == nil {
		t.Fatal("the member of an unbounded group was created anyway")
	}

	s := &Server{opts: Options{Writer: w}, requestRate: newRequestLimiter(time.Now)}
	resp := s.answerStartServer(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy},
		7, &agentpb.StartServerRequest{Group: "private-servers", Key: "c0ffee"})
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
	if !strings.Contains(resp.GetError().GetMessage(), "no spec.maxInstances") {
		t.Errorf("message = %q, which does not say the group has no ceiling",
			resp.GetError().GetMessage())
	}
}

func TestAGroupTargetPicksByPlayableSeats(t *testing.T) {
	state := networkWith(nil, []*agentpb.ServerState{
		{Name: "duels-a", Group: "duels", Players: 12, Slots: 100, PlayableSlots: 12, Registered: true},
		{Name: "duels-b", Group: "duels", Players: 20, Slots: 100, PlayableSlots: 24, Registered: true},
	})
	got, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Group{Group: "duels"},
	})
	if !ok || got != "duels-b" {
		t.Errorf("target = %q ok=%v, want duels-b: duels-a is full at its playable seats", got, ok)
	}
}

func TestAPlayableFigureOfZeroReadsAsEverySeat(t *testing.T) {
	state := networkWith(nil, []*agentpb.ServerState{
		{Name: "lobby-a", Group: "lobby", Players: 90, Slots: 100, PlayableSlots: 12, Registered: true},
		{Name: "lobby-b", Group: "lobby", Players: 50, Slots: 100, Registered: true},
	})
	got, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Group{Group: "lobby"},
	})
	if !ok || got != "lobby-b" {
		t.Errorf("target = %q ok=%v, want lobby-b: an older status's 0 is all 100 seats", got, ok)
	}
}

func TestAGroupWhoseServersAreAllFullStillResolves(t *testing.T) {
	state := networkWith(nil, []*agentpb.ServerState{
		{Name: "duels-a", Group: "duels", Players: 14, Slots: 100, PlayableSlots: 12, Registered: true},
	})
	if got, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Group{Group: "duels"},
	}); !ok || got != "duels-a" {
		t.Errorf("target = %q ok=%v, want duels-a: full is not unroutable", got, ok)
	}
}

func TestAmongFullRoundsAGroupTargetPicksOneWithRoomLeft(t *testing.T) {
	state := networkWith(nil, []*agentpb.ServerState{
		{Name: "duels-a", Group: "duels", Players: 16, Slots: 16, PlayableSlots: 12, Registered: true},
		{Name: "duels-b", Group: "duels", Players: 12, Slots: 16, PlayableSlots: 12, Registered: true},
	})
	got, ok := resolveTarget(state, &agentpb.ConnectRequest{
		Target: &agentpb.ConnectRequest_Group{Group: "duels"},
	})
	if !ok || got != "duels-b" {
		t.Errorf("target = %q ok=%v, want duels-b: duels-a is at its hard limit", got, ok)
	}
}

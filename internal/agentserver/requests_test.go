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

package agentserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

// One test per bound, and each asserting the reason rather than merely that
// something was refused. A single "it was refused" test passes when the wrong
// bound fired, and a bound that cannot be shown to fire on its own might be
// dead behind another.

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

func TestAnUnregisteredTargetIsRefusedEvenThoughItExists(t *testing.T) {
	// A server the proxies cannot route to is a server a move would put the
	// player nowhere. Registered and not the phase, for the reason
	// ServerState's own comment gives.
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
	// The bound is per pod. Sharing one bucket would let a compromised pod
	// silence every other agent in the fleet, which is the failure milestone
	// 2a's promise is about.
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

// One test per bound here too, and each names the bound it broke. A single
// "it was refused" test passes when the wrong bound fired.

func announcement(state string, attributes map[string]string) *agentpb.AnnounceRequest {
	return &agentpb.AnnounceRequest{State: state, Attributes: attributes}
}

func TestAnAnnouncementWithinItsBoundsIsAccepted(t *testing.T) {
	if message, ok := announcementRefusal(announcement("running",
		map[string]string{"map": "arena"})); !ok {
		t.Errorf("an ordinary announcement was refused: %s", message)
	}
	// And the empty one, which is how a server takes its description back.
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
	// The number is in the message because the caller is a plugin author
	// reading a log line, and a bound they have to go and look up is one they
	// will guess at instead.
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
	// Named, because a plugin publishing eight attributes needs to know which.
	if !strings.Contains(message, `"map"`) {
		t.Errorf("refusal = %q, want it to name the attribute", message)
	}
}

func TestAnAttributeWithNoNameIsRefused(t *testing.T) {
	// Not a bound but a shape: nothing can ask for an attribute that has no
	// name, so storing one would cost a reader nothing but confusion.
	if _, ok := announcementRefusal(announcement("", map[string]string{"": "v"})); ok {
		t.Error("a nameless attribute was accepted")
	}
}

func TestAnAnnouncementIsStoredUnderTheIdentitysOwnName(t *testing.T) {
	// The name comes from the pod's authenticated identity and the message has
	// no field for one, which is what keeps this the only verb here that
	// cannot describe somebody else.
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("pod-a", agent.RoleServer)
	// Built by hand rather than by New, which insists on the fleets and the
	// certificates a real listener needs. This verb reaches none of them: it
	// reads the identity, checks the bounds and writes to the registry.
	s := &Server{opts: Options{Agents: registry}, requestRate: newRequestLimiter(time.Now)}

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
	// A network's picture has a record per server and none per proxy. Storing
	// it silently would leave a plugin author watching for a description that
	// was never going to appear.
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("proxy-a", agent.RoleProxy)
	s := &Server{opts: Options{Agents: registry}, requestRate: newRequestLimiter(time.Now)}

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
	// The narrowest verb on this channel: it names nothing, so the only server
	// it can reach is the one that asked. Retire is bounded by a namespace;
	// this is bounded by a pod.
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("pod-a", agent.RoleServer)
	registry.Connect("pod-b", agent.RoleServer)
	s := &Server{opts: Options{Agents: registry}, requestRate: newRequestLimiter(time.Now)}

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

func TestAProxyIsRefusedADoor(t *testing.T) {
	registry := agent.New(time.Now, time.Second, time.Now())
	registry.Connect("proxy-a", agent.RoleProxy)
	s := &Server{opts: Options{Agents: registry}, requestRate: newRequestLimiter(time.Now)}

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

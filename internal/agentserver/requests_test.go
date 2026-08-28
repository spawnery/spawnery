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
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/agentpb"
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

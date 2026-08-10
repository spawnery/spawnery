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

package agentserver_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/proxyreg"
)

// joinContextRecordingFleet delegates to a real *proxyreg.Fleet — so the
// session behaves exactly like it does in production, FullSync and all — but
// on a second Join call, it also records whether the *first* call's context
// was already cancelled by that point.
//
// That check has to happen here, inside Join itself, rather than later from
// the test goroutine. sessions.enter cancels the displaced handler's
// enter-derived context before that handler's successor ever reaches Join —
// happens-before within one goroutine, no scheduling luck required — so if
// ProxySession hands Join the enter-derived context, this is true on every
// run. But the cascade that eventually cancels stream.Context() too (the
// first handler noticing its own ctx.Done(), returning, and gRPC tearing its
// stream down as a result) is a real concurrent race with no ordering
// guarantee against this call. Give it any wall-clock time at all — a poll
// loop, a few more Recv round trips — and that race resolves the same way
// regardless of which context Join actually received, and the test stops
// discriminating. Checking synchronously, in the same call, is what keeps it
// honest.
type joinContextRecordingFleet struct {
	*proxyreg.Fleet

	mu                         sync.Mutex
	joins                      int
	firstCtx                   context.Context
	firstCancelledBySecondJoin bool
}

func (f *joinContextRecordingFleet) Join(ctx context.Context, namespace, group, podUID string) (
	<-chan *agentpb.OperatorToProxy, func(), error) {
	f.mu.Lock()
	f.joins++
	switch f.joins {
	case 1:
		if ctx.Err() != nil {
			f.mu.Unlock()
			panic("the first Join's context was already cancelled before a successor existed")
		}
		f.firstCtx = ctx
	case 2:
		f.firstCancelledBySecondJoin = f.firstCtx.Err() != nil
	}
	f.mu.Unlock()
	return f.Fleet.Join(ctx, namespace, group, podUID)
}

// firstWasCancelledBySecond blocks until a second Join call has happened, then
// reports what it observed about the first call's context at that instant.
func (f *joinContextRecordingFleet) firstWasCancelledBySecond(t *testing.T) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		joins := f.joins
		observed := f.firstCancelledBySecondJoin
		f.mu.Unlock()
		if joins >= 2 {
			return observed
		}
		if time.Now().After(deadline) {
			t.Fatal("the second Join never happened")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The guard in Fleet.Join (internal/proxyreg/fleet.go) only closes the
// zombie-handler window because ProxySession passes it the enter-derived
// context, the one sessions.enter returns and a successor's enter cancels —
// not stream.Context(), which a supersede never touches. Swap the two in
// server.go and this guard goes quietly inert: Join would never see a
// cancelled context from a displaced handler, because stream.Context() of the
// first stream is still live at the point its successor calls Join (a
// supersede does not close the client's own stream), and no other test would
// notice.
//
// This pins the invariant directly. TestASecondProxyStreamSupersedesTheFirstWithoutMisreportingWhy
// is the model for driving the supersede itself; this test adds the one
// assertion that test cannot make, because it has no way to see the context
// object Join was actually given.
func TestProxySessionJoinsWithTheEnterDerivedContext(t *testing.T) {
	recording := &joinContextRecordingFleet{}
	f := newFixtureWithProxies(t, 8*time.Minute, 10*time.Minute, 0, func(real *proxyreg.Fleet) agentserver.ProxyFleet {
		recording.Fleet = real
		return recording
	})
	pod := f.proxyPod("gateway-hhhh")

	first, closeFirst := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeFirst()
	for i := 0; i < 3; i++ {
		if _, err := first.Recv(); err != nil {
			t.Fatalf("first Recv %d: %v", i, err)
		}
	}

	second, closeSecond := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeSecond()
	for i := 0; i < 3; i++ {
		if _, err := second.Recv(); err != nil {
			t.Fatalf("second Recv %d: %v", i, err)
		}
	}

	if !recording.firstWasCancelledBySecond(t) {
		t.Fatal("the first stream's Join context was not cancelled by the time its successor joined; " +
			"ProxySession must be passing Join the enter-derived context, not stream.Context()")
	}
}

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

// joinContextRecordingFleet records, on the second Join, whether the first Join's context
// was already cancelled. The check must run inside Join: stream.Context() is cancelled
// later by a concurrent race, and any delay lets that race hide which context Join got.
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

// Fleet.Join's zombie-handler guard only works if ProxySession passes it the
// enter-derived context: a supersede never cancels the first stream's stream.Context().
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

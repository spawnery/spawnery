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
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// Without a fleet, ProxySession would panic on its first stream — in a gRPC
// handler goroutine, minutes after start. New refuses that up front instead,
// the same way controller.SetupAll refuses a nil Bootstrapper. This is the
// symmetric test to TestSetupAllRefusesWithoutABootstrapper.
func TestNewRefusesWithoutAProxyFleet(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a nil Proxies fleet instead of panicking")
		}
	}()
	New(Options{})
}

// The same guard on the other half of the fleet, and it needed its own test
// for a reason worth writing down: this milestone tried to mutation-check the
// operator binary's wiring instead -- delete `Servers:` from its options and
// watch something fail -- and nothing did. `go vet` does not run main, and no
// unit test in this repository constructs the operator's own Options. The
// binary's wiring is guarded by this panic and by `make e2e`, which starts the
// real process, and by nothing in between.
//
// So the panic is what a test can see, and this is that test.
func TestNewRefusesWithoutAServerFanout(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a nil Servers fanout instead of panicking")
		}
	}()
	New(Options{Proxies: stubFleet{}})
}

// And the third, for the source a request is resolved against. Same guard,
// same reason: without it answerConnect would panic inside a session.
func TestNewRefusesWithoutANetworkStateSource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a zero netstate.Source instead of panicking")
		}
	}()
	New(Options{Proxies: stubFleet{}, Servers: stubFanout{}})
}

// stubFanout satisfies ServerFanout so the test above reaches the State check.
type stubFanout struct{}

func (stubFanout) Join(context.Context, string, string) (<-chan *agentpb.OperatorToServer, func(), error) {
	return nil, func() {}, nil
}

// stubFleet satisfies ProxyFleet so the test above reaches the Servers check
// rather than stopping at the one before it.
type stubFleet struct{}

func (stubFleet) Join(context.Context, string, string, string) (<-chan *agentpb.OperatorToProxy, func(), error) {
	return nil, func() {}, nil
}

func (stubFleet) Move(string, string, string) {}

// The opening sends are the one part of a session nothing could end.
//
// stream.Send blocks on the client's flow-control window and observes no
// context, so an agent that opens a stream and then stops reading held this
// goroutine for good: the hard deadline's timer is armed after the sends, and
// arming it before would not have helped either, because sessions.cancel
// cancels a context that only the handler's own loop selects on — and this is
// before the loop. Returning is the only thing that ends a blocked Send.
func TestTheOpeningSendsAreBounded(t *testing.T) {
	t.Run("a send that finishes hands its result straight back", func(t *testing.T) {
		want := errors.New("the stream broke")
		if got := sendBounded(time.Minute, func() error { return want }); !errors.Is(got, want) {
			t.Errorf("err = %v, want %v — a real send error must not be reported as a timeout", got, want)
		}
		if got := sendBounded(time.Minute, func() error { return nil }); got != nil {
			t.Errorf("err = %v, want nil", got)
		}
	})

	t.Run("a send that never finishes gives up", func(t *testing.T) {
		// Released at the end so this test leaves nothing running, and
		// checked, because a send that is still blocked when the deadline
		// fires must still be able to finish afterwards — that is what the
		// real one does when the handler's return closes the stream.
		//
		// What this does not prove is the buffering of the result channel.
		// `sent <- send()` evaluates send fully, defers included, before it
		// touches the channel, so nothing inside send can observe the send
		// that follows it. Making the channel unbuffered parks that goroutine
		// for good and no assertion here goes red. The alternative is a
		// runtime.NumGoroutine poll in a package that also runs envtest and
		// two gRPC servers, which would buy this one detail at the price of a
		// flaky suite. The buffer is reasoned in sendBounded's comment
		// instead.
		release := make(chan struct{})
		finished := make(chan struct{})
		defer func() {
			close(release)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Error("the send never finished after it was released")
			}
		}()

		start := time.Now()
		err := sendBounded(20*time.Millisecond, func() error {
			<-release
			close(finished)
			return nil
		})
		if err == nil {
			t.Fatal("a send that never returns was reported as successful")
		}
		if code := status.Code(err); code != codes.DeadlineExceeded {
			t.Errorf("code = %s, want %s: an agent that stops reading is not an authentication "+
				"problem and must not read as one", code, codes.DeadlineExceeded)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("it took %s to give up on a 20ms bound", elapsed)
		}
	})
}

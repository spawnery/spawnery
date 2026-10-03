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
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/spawnery/spawnery/internal/agentpb"
)

func TestNewRefusesWithoutAProxyFleet(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a nil Proxies fleet instead of panicking")
		}
	}()
	New(Options{})
}

// The operator binary's Options are built by no unit test, so this panic and
// `make e2e` are the only guards on its wiring.
func TestNewRefusesWithoutAServerFanout(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a nil Servers fanout instead of panicking")
		}
	}()
	New(Options{Proxies: stubFleet{}})
}

func TestNewRefusesWithoutANetworkStateSource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a zero netstate.Source instead of panicking")
		}
	}()
	New(Options{Proxies: stubFleet{}, Servers: stubFanout{}})
}

type stubFanout struct{}

func (stubFanout) Join(context.Context, string, string) (<-chan *agentpb.OperatorToServer, func(), error) {
	return nil, func() {}, nil
}

func (stubFanout) SetInterest(string, bool) {}

type stubFleet struct{}

func (stubFleet) Join(context.Context, string, string, string) (<-chan *agentpb.OperatorToProxy, func(), error) {
	return nil, func() {}, nil
}

func (stubFleet) Move(string, string, string) {}

func (stubFleet) SetInterest(string, bool) {}

func (stubFleet) SendState(context.Context, string) {}

// stream.Send observes no context and the hard deadline cannot reach a handler
// before its loop, so returning is the only thing that ends a blocked Send.
func TestTheOpeningSendsAreBounded(t *testing.T) {
	t.Run("a send that finishes hands its result straight back", func(t *testing.T) {
		want := errors.New("the stream broke")
		if got := sendBounded(time.Minute, "a message", func() error { return want }); !errors.Is(got, want) {
			t.Errorf("err = %v, want %v — a real send error must not be reported as a timeout", got, want)
		}
		if got := sendBounded(time.Minute, "a message", func() error { return nil }); got != nil {
			t.Errorf("err = %v, want nil", got)
		}
	})

	t.Run("a send that never finishes gives up", func(t *testing.T) {
		// Released at the end so this test leaves nothing running. The
		// buffering of sendBounded's result channel is not observable here
		// without a flaky goroutine count.
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
		err := sendBounded(20*time.Millisecond, "a message", func() error {
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

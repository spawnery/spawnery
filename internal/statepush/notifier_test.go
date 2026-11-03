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

package statepush

import (
	"context"
	"sync"
	"testing"
	"time"
)

const testInterval = 60 * time.Millisecond

type recordingTarget struct {
	mu    sync.Mutex
	sends []string
}

func (r *recordingTarget) SendState(_ context.Context, namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, namespace)
}

func (r *recordingTarget) count(namespace string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.sends {
		if s == namespace {
			n++
		}
	}
	return n
}

func (r *recordingTarget) waitFor(t *testing.T, namespace string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.count(namespace) < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d pushes, want %d", namespace, r.count(namespace), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheFirstChangeInAQuietNamespaceGoesOutAtOnce(t *testing.T) {
	target := &recordingTarget{}
	n := New(Options{Proxies: target, Interval: time.Hour})

	n.Changed("a")

	if got := target.count("a"); got != 1 {
		t.Errorf("pushes = %d right after the first change, want 1", got)
	}
}

func TestABurstSharesOnePushAtTheEndOfTheInterval(t *testing.T) {
	target := &recordingTarget{}
	n := New(Options{Proxies: target, Interval: testInterval})

	for i := 0; i < 50; i++ {
		n.Changed("a")
	}
	if got := target.count("a"); got != 1 {
		t.Fatalf("pushes = %d right after the burst, want the first one only", got)
	}

	target.waitFor(t, "a", 2)
	time.Sleep(2 * testInterval)
	if got := target.count("a"); got != 2 {
		t.Errorf("pushes = %d after the interval, want the first and one for the rest of the burst", got)
	}
}

func TestANamespaceQuietForAnIntervalPushesAtOnceAgain(t *testing.T) {
	target := &recordingTarget{}
	n := New(Options{Proxies: target, Interval: testInterval})

	n.Changed("a")
	time.Sleep(2 * testInterval)
	n.Changed("a")

	if got := target.count("a"); got != 2 {
		t.Errorf("pushes = %d, want 2: a change after a quiet interval waits for nothing", got)
	}
}

func TestNamespacesAreThrottledSeparately(t *testing.T) {
	target := &recordingTarget{}
	n := New(Options{Proxies: target, Interval: time.Hour})

	n.Changed("a")
	n.Changed("b")

	if target.count("a") != 1 || target.count("b") != 1 {
		t.Errorf("pushes a=%d b=%d, want 1 each", target.count("a"), target.count("b"))
	}
}

func TestStartReturnsWhenItsContextEndsAndDropsWhatWaits(t *testing.T) {
	target := &recordingTarget{}
	n := New(Options{Proxies: target, Interval: testInterval})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()

	n.Changed("a")
	n.Changed("a")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after its context ended")
	}

	time.Sleep(2 * testInterval)
	n.Changed("a")
	if got := target.count("a"); got != 1 {
		t.Errorf("pushes = %d, want only the one sent before the context ended", got)
	}
}

func TestTheNotifierRunsWithTheLeader(t *testing.T) {
	if !New(Options{}).NeedLeaderElection() {
		t.Error("NeedLeaderElection = false")
	}
}

func TestAnUnsetIntervalIsTheDefault(t *testing.T) {
	if got := New(Options{}).interval; got != DefaultInterval {
		t.Errorf("interval = %v, want %v", got, DefaultInterval)
	}
}

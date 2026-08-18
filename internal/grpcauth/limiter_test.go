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

package grpcauth

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc/peer"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

func TestPeerLimiterAllowsABurstThenRefills(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewPeerLimiter(func() time.Time { return now })

	for i := 0; i < PeerBurst; i++ {
		if !l.allow("10.244.0.7") {
			t.Fatalf("attempt %d of the permitted burst %d was refused", i, PeerBurst)
		}
	}
	if l.allow("10.244.0.7") {
		t.Fatalf("attempt %d was allowed; the burst is %d", PeerBurst+1, PeerBurst)
	}

	now = now.Add(PeerRefill)
	if !l.allow("10.244.0.7") {
		t.Errorf("one token did not refill after %s", PeerRefill)
	}
	if l.allow("10.244.0.7") {
		t.Errorf("two tokens refilled after %s; the rate is one per interval", PeerRefill)
	}
}

// The key is what makes a rollout safe. Every agent reconnects from its own pod
// IP, so a fleet coming back after an operator restart spends one bucket each
// rather than sharing one. A limiter keyed on anything the whole fleet has in
// common would throttle exactly the case that must not be throttled.
//
// This exercises the limiter's own keying with bare IP strings, which is not
// what the production path produces — peerAddr does, from a context.
// TestTheRateLimitKeysOnThePodRatherThanTheConnection below is what ties the
// two together.
func TestPeerLimiterBucketsArePerPeer(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewPeerLimiter(func() time.Time { return now })

	for i := 0; i < PeerBurst; i++ {
		l.allow("10.244.0.7")
	}
	if !l.allow("10.244.0.8") {
		t.Error("a second peer was refused because the first had spent its burst")
	}
}

// A bucket never fills past its burst, or a peer that was quiet for an hour
// could spend an hour's worth of budget at once.
func TestPeerLimiterDoesNotAccumulatePastItsBurst(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewPeerLimiter(func() time.Time { return now })

	l.allow("10.244.0.7")
	now = now.Add(100 * PeerRefill)

	for i := 0; i < PeerBurst; i++ {
		if !l.allow("10.244.0.7") {
			t.Fatalf("attempt %d was refused after a long quiet period", i)
		}
	}
	if l.allow("10.244.0.7") {
		t.Error("the bucket accumulated past its burst")
	}
}

// The limit sits behind the cache, and that ordering is the design rather than
// an implementation detail: a pod replaying one token must never reach it.
//
// This proves only the hit side: every replay after the first is served from
// the cache and so never enters the branch that consults the limiter at all,
// which means this assertion (reviews.calls == 1) would hold even if the
// limiter check were deleted from Authenticate entirely, with no limiter left
// to reach. TestDistinctTokensFromOnePeerAreRateLimited below is what proves
// the limiter is wired in and reached on the miss side.
func TestARepeatedTokenNeverReachesTheLimiter(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	reviews := &countingReviewer{} // stub: counts Create calls, returns an accepted review
	a := &Authenticator{
		Reviews:  reviews,
		Pods:     alwaysFoundPods{}, // stub: LookupPod returns ("lobby", true, nil)
		Audience: "spawnery-operator",
		Cache:    NewReviewCache(clock),
		Limiter:  NewPeerLimiter(clock),
	}

	for i := 0; i < PeerBurst*3; i++ {
		if _, err := a.Authenticate(context.Background(), "one-token", agent.RoleServer); err != nil {
			t.Fatalf("attempt %d was refused: %v", i, err)
		}
	}
	if reviews.calls != 1 {
		t.Errorf("the API server was asked %d times for one token, want 1", reviews.calls)
	}
}

// The replay test above cannot tell "the limiter is wired in and never
// reached on a hit" apart from "there is no limiter at all" -- both leave it
// green. This closes that gap with tokens the cache has never seen: every
// call below is a genuine cache miss, so each one reaches a.Limiter, and a
// peer presenting more than PeerBurst of them must eventually be refused by
// the limiter itself. isExhausted pins the refusal to the limiter, not to
// the reviewer or the pod checker, both of which are stubbed to always
// succeed.
func TestDistinctTokensFromOnePeerAreRateLimited(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	reviews := &countingReviewer{}
	a := &Authenticator{
		Reviews:  reviews,
		Pods:     alwaysFoundPods{},
		Audience: "spawnery-operator",
		Cache:    NewReviewCache(clock),
		Limiter:  NewPeerLimiter(clock),
	}

	for i := 0; i < PeerBurst+2; i++ {
		token := fmt.Sprintf("distinct-token-%d", i)
		_, err := a.Authenticate(context.Background(), token, agent.RoleServer)
		if err == nil {
			continue
		}
		if !isExhausted(err) {
			t.Fatalf("distinct token %d was refused for a reason other than the rate limit: %v", i, err)
		}
		return // the limiter refused a genuinely new token; the test is satisfied
	}
	t.Fatalf("%d distinct tokens from one peer were all accepted; the limiter never engaged", PeerBurst+2)
}

// tcpPeerContext is what a real gRPC server hands the interceptor: a
// peer.Peer whose Addr is a *net.TCPAddr, so Addr.String() is
// IP:ephemeral-port. Constructing it by hand rather than standing up a server
// keeps the test a unit test while still exercising the exact type the
// transport installs.
func tcpPeerContext(ip string, port int) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: port},
	})
}

// The limiter's key must name a POD, not a connection, and nothing else in
// this package ever looks at the key the production path actually produces:
// every other test calls Authenticate with context.Background(), where
// peerAddr returns the constant "unknown", so replacing peerAddr's body with a
// constant left both this package and internal/agentserver green.
//
// Both directions are here because each catches a different defect.
//
//   - Same IP, different ephemeral ports must share one bucket. A gRPC peer
//     address is IP:port and the port is fresh on every TCP connection, so
//     keying on the whole address gives a pod in a reconnect loop a fresh
//     PeerBurst per connection — the attack docs/known-issues.md documents,
//     bounded only by how fast it can complete handshakes.
//   - Different IPs must not share one bucket. That is the mass-reconnect
//     safety the design leans on, and it is also what catches a peerAddr that
//     returns a constant: with one key for everybody, the second pod inherits
//     the first pod's spent bucket.
//
// Every token below is distinct, so every call is a genuine cache miss and
// therefore genuinely reaches the limiter; isExhausted pins the refusal to the
// limiter rather than to the reviewer or the pod checker, both stubbed to
// succeed.
func TestTheRateLimitKeysOnThePodRatherThanTheConnection(t *testing.T) {
	newAuth := func(now func() time.Time) *Authenticator {
		return &Authenticator{
			Reviews:  &countingReviewer{},
			Pods:     alwaysFoundPods{},
			Audience: "spawnery-operator",
			Cache:    NewReviewCache(now),
			Limiter:  NewPeerLimiter(now),
		}
	}

	t.Run("one pod reconnecting from a new port shares its bucket", func(t *testing.T) {
		now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
		a := newAuth(func() time.Time { return now })

		first := tcpPeerContext("10.244.0.7", 42662)
		for i := 0; i < PeerBurst; i++ {
			token := fmt.Sprintf("first-connection-token-%d", i)
			if _, err := a.Authenticate(first, token, agent.RoleServer); err != nil {
				t.Fatalf("call %d of the permitted burst %d was refused: %v", i, PeerBurst, err)
			}
		}

		// The same pod, a new TCP connection: a different ephemeral port and
		// therefore a different peer.Addr.String(), but the same host.
		second := tcpPeerContext("10.244.0.7", 42674)
		_, err := a.Authenticate(second, "second-connection-token", agent.RoleServer)
		if err == nil {
			t.Fatalf("a %d+1st token check from 10.244.0.7 was allowed because it "+
				"arrived on a new connection: the bucket is keyed on IP:port, so "+
				"every reconnect starts a fresh burst and the limit bounds nothing",
				PeerBurst)
		}
		if !isExhausted(err) {
			t.Fatalf("refused for a reason other than the rate limit: %v", err)
		}
	})

	t.Run("two pods do not share a bucket", func(t *testing.T) {
		now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
		a := newAuth(func() time.Time { return now })

		spender := tcpPeerContext("10.244.0.7", 42662)
		for i := 0; i < PeerBurst; i++ {
			token := fmt.Sprintf("busy-pod-token-%d", i)
			if _, err := a.Authenticate(spender, token, agent.RoleServer); err != nil {
				t.Fatalf("call %d of the permitted burst %d was refused: %v", i, PeerBurst, err)
			}
		}

		other := tcpPeerContext("10.244.0.8", 51000)
		if _, err := a.Authenticate(other, "quiet-pod-token", agent.RoleServer); err != nil {
			t.Fatalf("a second pod was refused because the first had spent its burst: %v — "+
				"the limiter is not keyed on the peer at all, and a fleet reconnecting "+
				"after an operator restart would throttle itself", err)
		}
	})
}

// countingReviewer answers every review the same way and counts the asking.
// The count is the whole point: it is what shows the API server was spared.
type countingReviewer struct{ calls int }

func (c *countingReviewer) Create(
	_ context.Context, tr *authnv1.TokenReview, _ metav1.CreateOptions,
) (*authnv1.TokenReview, error) {
	c.calls++
	return &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     tr.Spec.Audiences,
		User: authnv1.UserInfo{
			Username: "system:serviceaccount:minecraft:" + podspec.ServerServiceAccountName,
			Extra: map[string]authnv1.ExtraValue{
				claimPodName: {"lobby-abcd"},
				claimPodUID:  {"5f3a9c1e-0000-4000-8000-000000000002"},
			},
		},
	}}, nil
}

// alwaysFoundPods is the half Authenticate must NOT cache, stubbed to succeed
// so this test measures only the half it must.
type alwaysFoundPods struct{}

func (alwaysFoundPods) LookupPod(
	_ context.Context, _, _, _ string, _ agent.Role,
) (string, bool, error) {
	return "lobby", true, nil
}

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
	"testing"
	"time"

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

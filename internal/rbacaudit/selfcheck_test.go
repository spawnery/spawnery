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

package rbacaudit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeReviewer answers reviews from a set of allowed keys, and records what it
// was asked. The recording is half the point: the attributes this builds are
// what the authorizer matches on, and a check that asked about the wrong verb
// or dropped the subresource would answer "allowed" for a permission nobody
// holds.
type fakeReviewer struct {
	allowed map[string]bool
	asked   []authorizationv1.ResourceAttributes
	err     error
}

func (f *fakeReviewer) Create(_ context.Context, review *authorizationv1.SelfSubjectAccessReview,
	_ metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error) {
	if f.err != nil {
		return nil, f.err
	}
	attr := *review.Spec.ResourceAttributes
	f.asked = append(f.asked, attr)
	key := Permission{
		Group: attr.Group, Resource: attr.Resource,
		Subresource: attr.Subresource, Verb: attr.Verb,
	}.Key()
	out := review.DeepCopy()
	out.Status.Allowed = f.allowed[key]
	return out, nil
}

func perms() []Permission {
	return []Permission{
		{Group: "", Resource: "pods", Verb: "list", Why: "the manager's cache"},
		{Group: "", Resource: "pods", Subresource: "status", Verb: "get", Why: "readiness"},
		{Group: "spawnery.cloud", Resource: "networks", Verb: "watch", Why: "the Network controller"},
	}
}

// TestVerifyReturnsOnlyWhatIsDenied is the whole function: it asks the
// authorizer directly rather than waiting for a request to be refused, which
// is what lets it see a cache-backed verb at all.
func TestVerifyReturnsOnlyWhatIsDenied(t *testing.T) {
	// The keys are Permission.Key()'s own rendering, taken from it rather
	// than spelled by hand: a key format this test guessed wrong would make
	// every permission read as denied and the assertion below fail for a
	// reason that has nothing to do with Verify.
	allowed := map[string]bool{}
	for _, p := range perms() {
		if p.Subresource == "" {
			allowed[p.Key()] = true
		}
	}
	reviewer := &fakeReviewer{allowed: allowed}

	denied, err := Verify(context.Background(), reviewer, perms(), "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(denied) != 1 || denied[0].Subresource != "status" {
		t.Fatalf("denied = %+v, want only pods/status get", denied)
	}
	// Every permission is asked about, not just up to the first denial: an
	// administrator fixing one missing verb at a time, restarting between
	// each, is the worst possible way to learn about four.
	if len(reviewer.asked) != len(perms()) {
		t.Errorf("asked %d reviews, want %d", len(reviewer.asked), len(perms()))
	}
}

// TestVerifyAsksAboutTheSubresourceRatherThanTheResource is the attribute most
// easily dropped, and dropping it is silent in the permissive direction:
// pods/status is a different resource to the authorizer, and asking about
// "pods" would answer allowed for an operator that cannot write a status.
func TestVerifyAsksAboutTheSubresourceRatherThanTheResource(t *testing.T) {
	reviewer := &fakeReviewer{allowed: map[string]bool{}}

	if _, err := Verify(context.Background(), reviewer, perms(), "spawnery-system"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var found bool
	for _, a := range reviewer.asked {
		if a.Resource == "pods" && a.Subresource == "status" && a.Verb == "get" {
			found = true
		}
		// And the namespace reaches every review, or a namespaced grant would
		// be checked at cluster scope and read as missing.
		if a.Namespace != "spawnery-system" {
			t.Errorf("review for %s/%s asked namespace %q", a.Resource, a.Verb, a.Namespace)
		}
	}
	if !found {
		t.Error("no review carried the subresource")
	}
}

// TestVerifyFailsWholeRatherThanReportingPhantomDenials pins the error path.
// An API server that cannot answer a review says nothing about what this
// operator may do, and counting every unanswered review as a denial would bury
// the real ones on the day there are any.
func TestVerifyFailsWholeRatherThanReportingPhantomDenials(t *testing.T) {
	reviewer := &fakeReviewer{err: errors.New("apiserver is having a moment")}

	denied, err := Verify(context.Background(), reviewer, perms(), "")
	if err == nil {
		t.Fatal("Verify succeeded against an API server that answered nothing")
	}
	if denied != nil {
		t.Errorf("denied = %+v, want none: an unanswered review is not a denial", denied)
	}
}

// TestDeniedMessageNamesTheCallSite is why Permission.Why is worth carrying
// into the log. "networks: list is missing" leaves an administrator to go and
// find out what breaks; the table already knows.
func TestDeniedMessageNamesTheCallSite(t *testing.T) {
	msg := DeniedMessage([]Permission{
		{Group: "", Resource: "pods", Verb: "create", Why: "the Server controller creates pods"},
		{Group: "", Resource: "events", Verb: "create", Why: "every controller"},
	})
	for _, want := range []string{"pods", "create", "the Server controller creates pods", "events", ";"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

// TestTheRealTablesAreCheckable is the guard that keeps this useful. Verify
// builds ResourceAttributes from each Permission, and a table entry with no
// resource or no verb produces a review the authorizer answers about nothing
// -- allowed, every time, for a permission that means nothing.
func TestTheRealTablesAreCheckable(t *testing.T) {
	for name, table := range map[string][]Permission{
		"RequiredCluster":    RequiredCluster,
		"RequiredNamespaced": RequiredNamespaced,
	} {
		for _, p := range table {
			if p.Resource == "" || p.Verb == "" {
				t.Errorf("%s carries %+v, which cannot be checked against the authorizer", name, p)
			}
		}
	}
}

// recordingLogger captures what the checker logs, which is most of what it
// does: the gauge says how many permissions are missing and the log is the
// only thing that says which, and when.
func recordingLogger(lines *[]string) logr.Logger {
	return funcr.New(func(prefix, args string) {
		*lines = append(*lines, args)
	}, funcr.Options{})
}

func checkerOver(reviewer Reviewer, interval time.Duration) *Checker {
	return &Checker{
		Reviewer: reviewer,
		Scopes:   []Scope{{What: "cluster-scoped", Required: perms()}},
		Interval: interval,
	}
}

// countingReviewer wraps fakeReviewer and stops the checker's context once it
// has answered enough rounds, so a repeating test does not depend on a sleep.
type countingReviewer struct {
	*fakeReviewer
	after int
	stop  context.CancelFunc
	n     int
}

func (c *countingReviewer) Create(ctx context.Context, review *authorizationv1.SelfSubjectAccessReview,
	opts metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error) {
	out, err := c.fakeReviewer.Create(ctx, review, opts)
	if len(c.asked)%len(perms()) == 0 {
		c.n++
		if c.n >= c.after {
			c.stop()
		}
	}
	return out, err
}

// TestTheCheckerAsksAgain is the whole change. Before it, a permission revoked
// while the operator ran was invisible: milestone 6a measured seven and three
// quarter minutes with pods:list gone and not one log line anywhere.
func TestTheCheckerAsksAgain(t *testing.T) {
	fake := &fakeReviewer{allowed: allowAll()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reviewer := &countingReviewer{fakeReviewer: fake, after: 3, stop: cancel}

	checker := checkerOver(reviewer, time.Millisecond)
	if err := checker.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if rounds := len(fake.asked) / len(perms()); rounds < 3 {
		t.Errorf("the checker ran %d rounds, want at least 3", rounds)
	}
}

// TestANegativeIntervalChecksOnce keeps the old behaviour reachable. An
// operator whose administrator would rather pay nothing after startup can have
// exactly what 0.2.3 did.
func TestANegativeIntervalChecksOnce(t *testing.T) {
	fake := &fakeReviewer{allowed: allowAll()}
	checker := checkerOver(fake, -1)
	if err := checker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(fake.asked) != len(perms()) {
		t.Errorf("a negative interval asked %d reviews, want the %d of one round",
			len(fake.asked), len(perms()))
	}
}

// TestADenialRepeatsAndAGrantDoesNot pins the asymmetry. A broken installation
// has to stay legible in a log somebody tails hours later; a healthy one
// saying so every ten minutes for a year is how a log stops being read.
func TestADenialRepeatsAndAGrantDoesNot(t *testing.T) {
	allowed := allowAll()
	fake := &fakeReviewer{allowed: allowed}
	checker := checkerOver(fake, 0)
	var lines []string
	logger := recordingLogger(&lines)
	ctx := context.Background()

	checker.reported = map[string]string{}
	checker.checkAll(ctx, logger)
	checker.checkAll(ctx, logger)
	if got := countContaining(lines, "every permission the operator needs is granted"); got != 1 {
		t.Errorf("two granted rounds logged %d lines, want 1", got)
	}

	// Revoke one, and check twice more.
	allowed[perms()[0].Key()] = false
	checker.checkAll(ctx, logger)
	checker.checkAll(ctx, logger)
	if got := countContaining(lines, "is missing permissions"); got != 2 {
		t.Errorf("two denied rounds logged %d lines, want 2", got)
	}

	// Repair it: the recovery is news and says what was missing.
	allowed[perms()[0].Key()] = true
	checker.checkAll(ctx, logger)
	if got := countContaining(lines, "are granted again"); got != 1 {
		t.Errorf("the repair logged %d lines, want 1", got)
	}
	if got := countContaining(lines, "the manager's cache"); got == 0 {
		t.Error("no line named the call site of the permission that was missing")
	}
}

// TestTheGaugeCountsWhatIsMissingAndSurvivesAFailedRound covers both readings
// the gauge has to carry. Zero means asked and nothing is missing, which is
// what an alert acts on; a round that could not ask leaves the last answer
// alone, because an API server that cannot answer says nothing about what the
// operator may do.
func TestTheGaugeCountsWhatIsMissingAndSurvivesAFailedRound(t *testing.T) {
	const scope = "gauge-test"
	t.Cleanup(func() { PermissionsMissing.DeleteLabelValues(scope) })

	allowed := allowAll()
	allowed[perms()[0].Key()] = false
	fake := &fakeReviewer{allowed: allowed}
	checker := &Checker{
		Reviewer: fake,
		Scopes:   []Scope{{What: scope, Required: perms()}},
	}
	checker.reported = map[string]string{}
	var lines []string
	logger := recordingLogger(&lines)
	checker.checkAll(context.Background(), logger)
	if got := testutil.ToFloat64(PermissionsMissing.WithLabelValues(scope)); got != 1 {
		t.Errorf("the gauge reads %v, want the 1 permission that is denied", got)
	}

	fake.err = errors.New("the API server is unreachable")
	checker.checkAll(context.Background(), logger)
	if got := testutil.ToFloat64(PermissionsMissing.WithLabelValues(scope)); got != 1 {
		t.Errorf("a failed round moved the gauge to %v; it should have left it at 1", got)
	}

	fake.err = nil
	allowed[perms()[0].Key()] = true
	checker.checkAll(context.Background(), logger)
	if got := testutil.ToFloat64(PermissionsMissing.WithLabelValues(scope)); got != 0 {
		t.Errorf("the gauge reads %v after the repair, want 0", got)
	}
}

// TestDefaultScopesAskTheRealTables guards the wiring main.go no longer does
// itself: the cluster table at cluster scope, the namespaced one in the
// operator's own namespace. Asking either in the other's scope would answer
// denied for permissions the operator holds.
func TestDefaultScopesAskTheRealTables(t *testing.T) {
	scopes := DefaultScopes("spawnery-system")
	if len(scopes) != 2 {
		t.Fatalf("DefaultScopes returned %d scopes, want 2", len(scopes))
	}
	if scopes[0].Namespace != "" {
		t.Errorf("the cluster scope asks in namespace %q, want cluster scope", scopes[0].Namespace)
	}
	if len(scopes[0].Required) != len(RequiredCluster) {
		t.Error("the cluster scope does not carry RequiredCluster")
	}
	if scopes[1].Namespace != "spawnery-system" {
		t.Errorf("the namespaced scope asks in %q, want the operator's namespace", scopes[1].Namespace)
	}
	if len(scopes[1].Required) != len(RequiredNamespaced) {
		t.Error("the namespaced scope does not carry RequiredNamespaced")
	}
}

func allowAll() map[string]bool {
	allowed := map[string]bool{}
	for _, p := range perms() {
		allowed[p.Key()] = true
	}
	return allowed
}

func countContaining(lines []string, want string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, want) {
			n++
		}
	}
	return n
}

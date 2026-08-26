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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Reviewer creates SelfSubjectAccessReviews. It is
// kubernetes.Interface.AuthorizationV1().SelfSubjectAccessReviews(), narrowed
// so a test can substitute one without a cluster.
type Reviewer interface {
	Create(ctx context.Context, review *authorizationv1.SelfSubjectAccessReview,
		opts metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error)
}

// Verify asks the API server, in the operator's own identity, whether each of
// these permissions is actually granted, and returns the ones that are not.
//
// # Why this exists at all, when there is already an audit
//
// The audit in this package compares the required table against the generated
// ClusterRole: two files, checked against each other at build time. It proves
// the manifest says what the code needs, and it cannot prove that the manifest
// reached the cluster, that it was applied whole, that the RoleBinding names
// the right ServiceAccount, or that nobody has edited it since.
//
// Nothing else notices when it did not, and that is the point. Measured during
// milestone 6a: removing pods:list from every marker and watching the operator
// for seven and three-quarter minutes produced not one log line, no 403 in
// rest_client_requests_total across 24 samples, and no restart. A permission
// reached only through the manager's cache is claimed by a *watch*, and a
// watch that cannot start is retried silently forever -- so the request the
// API server would have denied is never made in a form anything reports. The
// operator sits there looking healthy and reconciling nothing.
//
// A SelfSubjectAccessReview asks the authorizer the question directly rather
// than waiting for a request to be refused, so it sees the cache-backed verbs
// exactly as well as the others. It needs no permission of its own: the
// system:basic-user ClusterRole grants it to every authenticated identity, and
// that binding is part of every conforming cluster.
//
// # What it does not answer
//
// Whether the operator needs a permission at all -- that is the required
// table's job, maintained by hand.
//
// It also answers only for the moment it is asked, which is why Checker below
// asks again on an interval rather than once at startup.
//
// namespace scopes the review. Pass the operator's own namespace for
// RequiredNamespaced and the empty string for RequiredCluster, where an empty
// namespace means "at cluster scope" to the authorizer.
func Verify(ctx context.Context, reviewer Reviewer, required []Permission, namespace string) ([]Permission, error) {
	var denied []Permission
	for _, p := range required {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace:   namespace,
					Group:       p.Group,
					Resource:    p.Resource,
					Subresource: p.Subresource,
					Verb:        p.Verb,
				},
			},
		}
		answer, err := reviewer.Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			// The whole check fails rather than the one permission being
			// counted as denied. An API server that cannot answer a review is
			// not evidence that a verb is missing, and reporting 74 phantom
			// denials would bury the real ones on the day there are any.
			return nil, fmt.Errorf("self subject access review for %s: %w", p.Key(), err)
		}
		if !answer.Status.Allowed {
			denied = append(denied, p)
		}
	}
	return denied, nil
}

// DeniedMessage renders what Verify returned for a log line, naming each
// permission and the call site that needs it.
//
// The call sites are the reason this is worth more than a list of verbs: an
// administrator looking at "networks: list is missing" has to go and find out
// what breaks, and Permission.Why already says.
func DeniedMessage(denied []Permission) string {
	out := ""
	for i, p := range denied {
		if i > 0 {
			out += "; "
		}
		out += p.Key()
		if p.Why != "" {
			out += " (" + p.Why + ")"
		}
	}
	return out
}

// DefaultCheckInterval is how often Checker asks again.
//
// The cost is what decided it, and it was measured rather than feared. The
// whole check is 73 SelfSubjectAccessReviews -- the two required tables -- and
// against an envtest API server on 2026-08-26 they took 54 ms end to end with
// the client-side rate limiter off, which is what this operator runs with:
// controller-runtime v0.24 sets QPS to -1 in GetConfig and leaves the pacing
// to the API server's own priority and fairness. The same 73 take 3.4 seconds
// at a client QPS of 20 and 13.4 at the client-go default of 5, so anyone who
// turns the limiter back on is buying a different trade and should know it.
//
// Ten minutes against 54 ms is roughly one part in ten thousand of one
// connection's time, and a review is an in-memory authorizer lookup with no
// etcd behind it. What made this look expensive before it was measured was the
// count, not the cost.
//
// What sets the interval is not the cost but what is being watched: a
// permission changes when a person changes it, so a check that notices within
// ten minutes notices as well as one that notices within ten seconds, and the
// alert on spawnery_permissions_missing needs the gauge no fresher than that.
const DefaultCheckInterval = 10 * time.Minute

// Scope is one required table and the namespace to ask about it in.
type Scope struct {
	// What names the scope in the log and in the metric's label.
	What string
	// Required is the table to check.
	Required []Permission
	// Namespace scopes the review. Empty means cluster scope, which is what
	// the authorizer reads an empty namespace as.
	Namespace string
}

// DefaultScopes is the pair the operator checks: everything RequiredCluster
// asks for at cluster scope, and everything RequiredNamespaced asks for in the
// operator's own namespace.
func DefaultScopes(operatorNamespace string) []Scope {
	return []Scope{
		{What: "cluster-scoped", Required: RequiredCluster},
		{What: "in its own namespace", Required: RequiredNamespaced, Namespace: operatorNamespace},
	}
}

// Checker runs Verify on an interval and reports what changed.
//
// Startup was where this began, and startup catches the commonest failure by
// far: an installation that was never right. What it could not catch is a
// permission revoked while the operator runs -- an administrator tightening a
// ClusterRole, a GitOps sync reverting a hand-applied binding, an aggregated
// role losing a rule -- which lands the operator in exactly the state
// milestone 6a measured: reconciling nothing on the paths that need the verb,
// logging nothing, restarting never, and reporting healthy throughout.
//
// The reason it took a measurement to decide was that "73 reviews per check"
// reads as expensive and is not; see DefaultCheckInterval.
type Checker struct {
	// Reviewer creates the reviews. The operator's own clientset.
	Reviewer Reviewer
	// Scopes is what to check, usually DefaultScopes.
	Scopes []Scope
	// Interval is how often to check again. Zero means DefaultCheckInterval;
	// negative means check once and stop, which is what this did before it
	// could do anything else.
	Interval time.Duration

	// reported is the last message logged per scope, so a state that has not
	// changed is not logged again. Only Start touches it.
	reported map[string]string
}

// NeedLeaderElection is false on purpose. A non-leader answering these
// questions gets the same answers -- the identity is the process's, not the
// lease's -- and an installation whose RBAC is wrong should say so on every
// replica, because the replica that says nothing is the one somebody is
// looking at.
func (c *Checker) NeedLeaderElection() bool { return false }

// Start checks now and then every Interval until ctx ends.
//
// It never returns an error, which is a decision rather than an omission: a
// missing permission may be one this cluster's paths never take, and stopping
// the manager over a table maintained by hand would turn a degradation into an
// outage. Loud is the point, not fatal.
func (c *Checker) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("permissions")
	c.reported = make(map[string]string, len(c.Scopes))

	c.checkAll(ctx, logger)
	if c.Interval < 0 {
		return nil
	}
	interval := c.Interval
	if interval == 0 {
		interval = DefaultCheckInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			c.checkAll(ctx, logger)
		}
	}
}

// checkAll runs one round over every scope.
func (c *Checker) checkAll(ctx context.Context, logger logr.Logger) {
	for _, scope := range c.Scopes {
		denied, err := Verify(ctx, c.Reviewer, scope.Required, scope.Namespace)
		if err != nil {
			// Reported and carried, and the gauge is left where it was. An API
			// server that cannot answer a review says nothing about what this
			// operator may do, so neither zeroing the gauge nor filling it
			// with 73 phantom denials would be true.
			logger.Error(err, "could not check the operator's own permissions", "scope", scope.What)
			continue
		}
		PermissionsMissing.WithLabelValues(scope.What).Set(float64(len(denied)))

		// Asymmetric on purpose. A denial repeats at every check, because a
		// broken installation should be noisy in a log somebody tails hours
		// later and six lines an hour is not a flood. A grant is logged only
		// when it is news -- at the first check, and again when a denial has
		// been repaired -- because a healthy operator saying so every ten
		// minutes for a year is how a log stops being read.
		message := DeniedMessage(denied)
		if len(denied) > 0 {
			logger.Error(nil,
				"the operator is missing permissions it needs; it will run and reconcile nothing on the paths that use them",
				"scope", scope.What, "count", len(denied), "missing", message)
			c.reported[scope.What] = message
			continue
		}
		previous, checked := c.reported[scope.What]
		switch {
		case checked && previous == "":
			// Granted last time and granted now. Nothing is news.
		case checked:
			logger.Info("the permissions the operator was missing are granted again",
				"scope", scope.What, "were", previous)
		default:
			logger.Info("every permission the operator needs is granted", "scope", scope.What)
		}
		c.reported[scope.What] = ""
	}
}

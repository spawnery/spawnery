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

// Reviewer is the SelfSubjectAccessReviews client, narrowed so a test can
// substitute one.
type Reviewer interface {
	Create(ctx context.Context, review *authorizationv1.SelfSubjectAccessReview,
		opts metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error)
}

// Verify asks the API server, in the operator's own identity, which of these
// permissions are not granted. The file audit cannot prove the manifest
// reached the cluster intact, and a cache-backed watch that is forbidden
// retries silently forever while the operator looks healthy. Every
// authenticated identity may create these reviews (system:basic-user). An
// empty namespace means cluster scope.
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
			// Fail the whole check: an API server that cannot answer is no evidence of a
			// missing verb, and dozens of phantom denials would bury real ones.
			return nil, fmt.Errorf("self subject access review for %s: %w", p.Key(), err)
		}
		if !answer.Status.Allowed {
			denied = append(denied, p)
		}
	}
	return denied, nil
}

// DeniedMessage names each permission with its Why, so an administrator
// sees what breaks.
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

// DefaultCheckInterval is set by how fast permissions change (by hand), not by
// cost: a review is an in-memory authorizer lookup, and the client-side rate
// limiter is off (controller-runtime sets QPS to -1).
const DefaultCheckInterval = 10 * time.Minute

type Scope struct {
	// What names the scope in the log and in the metric's label.
	What     string
	Required []Permission
	// Namespace is empty for cluster scope.
	Namespace string
}

func DefaultScopes(operatorNamespace string) []Scope {
	return []Scope{
		{What: "cluster-scoped", Required: RequiredCluster},
		{What: "in its own namespace", Required: RequiredNamespaced, Namespace: operatorNamespace},
	}
}

// Checker runs Verify on an interval, to catch a permission revoked while the
// operator runs, which otherwise fails silently.
type Checker struct {
	Reviewer Reviewer
	// Scopes is what to check, usually DefaultScopes.
	Scopes []Scope
	// Interval: zero means DefaultCheckInterval; negative means check once.
	Interval time.Duration

	// reported is the last message logged per scope. Only Start touches it.
	reported map[string]string
}

// Every replica checks: the identity is the process's, not the lease's, and
// the replica somebody is looking at should say so.
func (c *Checker) NeedLeaderElection() bool { return false }

// Start never returns an error: a missing permission may be on a path this
// cluster never takes, and stopping the manager would turn a degradation into
// an outage.
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

func (c *Checker) checkAll(ctx context.Context, logger logr.Logger) {
	for _, scope := range c.Scopes {
		denied, err := Verify(ctx, c.Reviewer, scope.Required, scope.Namespace)
		if err != nil {
			// The gauge is left where it was: a failed review says nothing about what the
			// operator may do.
			logger.Error(err, "could not check the operator's own permissions", "scope", scope.What)
			continue
		}
		PermissionsMissing.WithLabelValues(scope.What).Set(float64(len(denied)))

		// A denial is logged at every check; a grant only when it is news, so a
		// healthy operator does not flood the log.
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
		case checked:
			logger.Info("the permissions the operator was missing are granted again",
				"scope", scope.What, "were", previous)
		default:
			logger.Info("every permission the operator needs is granted", "scope", scope.What)
		}
		c.reported[scope.What] = ""
	}
}

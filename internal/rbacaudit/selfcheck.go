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

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// table's job, maintained by hand. And whether a permission granted here is
// still granted an hour from now: this runs once, at startup, because the
// alternative is 74 reviews on every resync for a state that changes when a
// person changes it. A revocation after startup is the case the audit cannot
// see and this cannot either; what it catches is the far commoner one, an
// installation that was never right.
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

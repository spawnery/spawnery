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

// Package rbacaudit states, independently of the generated manifests, which
// Kubernetes permissions the operator actually needs — and checks the
// generated ClusterRole against that statement in both directions.
//
// The table is maintained by hand on purpose. Deriving it from the kubebuilder
// markers would only prove that the role grants what the role grants.
//
// Two independent checks stand on that table, and the redundancy is
// deliberate: the file-based comparison in this package against the rendered
// ClusterRole and Role, and the envtest suite beside it, which asks the real
// authorizer the same questions through SubjectAccessReview. A rule this
// package expands wrongly and a rule the authorizer reads differently are
// separate mistakes, and neither check can see the other's.
package rbacaudit

import (
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

// Permission is one thing the operator may do: a verb on a resource, possibly
// on one of its subresources.
type Permission struct {
	// Group is the API group. The core group is the empty string.
	Group string
	// Resource is the plural resource name, without any subresource.
	Resource string
	// Subresource is empty for the resource itself.
	Subresource string
	// Verb is the RBAC verb.
	Verb string
	// Why names the call site that needs this. It is documentation, not
	// identity — Key ignores it — and it is what makes an obsolete entry
	// noticeable when its call site disappears.
	Why string
}

// Key is the identity of a permission, ignoring Why.
func (p Permission) Key() string {
	resource := p.Resource
	if p.Subresource != "" {
		resource += "/" + p.Subresource
	}
	return fmt.Sprintf("%s/%s:%s", p.Group, resource, p.Verb)
}

// String renders a permission for a failure message, including its reason.
func (p Permission) String() string {
	if p.Why == "" {
		return p.Key()
	}
	return p.Key() + " (" + p.Why + ")"
}

// ExpandRules flattens PolicyRules into individual permissions.
//
// A wildcard in any position is an error rather than an expansion: it grants
// everything in that position, so it can never be reconciled against a finite
// table, and an operator that needs a wildcard has outgrown this audit.
//
// A rule using NonResourceURLs is an error for the same reason this package
// exists in the first place: it grants access this audit has no way to
// represent, so letting it fall through the group/resource loops and expand
// to nothing would make the audit silently ignore what the role grants.
func ExpandRules(rules []rbacv1.PolicyRule) ([]Permission, error) {
	var out []Permission
	for i, rule := range rules {
		if len(rule.NonResourceURLs) > 0 {
			return nil, fmt.Errorf("rule %d uses non-resource URLs, which this audit cannot model", i)
		}
		// Refused rather than ignored, which is what this used to do.
		//
		// A Permission has no name field, so a rule restricted to particular
		// object names would expand into one that reads as unrestricted --
		// and it would read that way in the *permissive* direction: Compare
		// would find the required permission granted when in truth it is
		// granted for one name only. That is the wrong way for an audit to be
		// wrong.
		//
		// controller-gen emits no resourceNames, so nothing generated reaches
		// this. What does reach it is hand-written RBAC, and there is a live
		// invitation to write some: docs/known-issues.md records that the
		// master design asks for resourceNames on the forwarding-secret
		// reader Role. Whoever takes that up gets this error rather than an
		// audit that quietly stops meaning what it says.
		if len(rule.ResourceNames) > 0 {
			return nil, fmt.Errorf(
				"rule %d is restricted to resourceNames %v, which this audit cannot model: "+
					"a Permission carries no name, so this rule would expand into one that "+
					"reads as unrestricted and Compare would report it satisfied for every "+
					"object rather than for these", i, rule.ResourceNames)
		}
		for _, group := range rule.APIGroups {
			if group == rbacv1.APIGroupAll {
				return nil, fmt.Errorf("rule %d grants every API group", i)
			}
			for _, resource := range rule.Resources {
				name, sub, hasSub := strings.Cut(resource, "/")
				if name == rbacv1.ResourceAll {
					return nil, fmt.Errorf("rule %d grants every resource in group %q", i, group)
				}
				if hasSub && sub == rbacv1.ResourceAll {
					return nil, fmt.Errorf("rule %d grants every subresource of %q", i, name)
				}
				for _, verb := range rule.Verbs {
					if verb == rbacv1.VerbAll {
						return nil, fmt.Errorf("rule %d grants every verb on %q", i, resource)
					}
					out = append(out, Permission{
						Group:       group,
						Resource:    name,
						Subresource: sub,
						Verb:        verb,
					})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// Diff is the two-way comparison between what the code needs and what the
// role grants.
type Diff struct {
	// Missing is required but not granted: the operator will hit Forbidden.
	Missing []Permission
	// Extra is granted but not required: the role is wider than it needs to be.
	Extra []Permission
}

// Compare reports both directions. Duplicates on either side are collapsed.
func Compare(required, granted []Permission) Diff {
	requiredByKey := make(map[string]Permission, len(required))
	for _, p := range required {
		requiredByKey[p.Key()] = p
	}
	grantedByKey := make(map[string]Permission, len(granted))
	for _, p := range granted {
		grantedByKey[p.Key()] = p
	}

	var d Diff
	for key, p := range requiredByKey {
		if _, ok := grantedByKey[key]; !ok {
			d.Missing = append(d.Missing, p)
		}
	}
	for key, p := range grantedByKey {
		if _, ok := requiredByKey[key]; !ok {
			d.Extra = append(d.Extra, p)
		}
	}
	sort.Slice(d.Missing, func(i, j int) bool { return d.Missing[i].Key() < d.Missing[j].Key() })
	sort.Slice(d.Extra, func(i, j int) bool { return d.Extra[i].Key() < d.Extra[j].Key() })
	return d
}

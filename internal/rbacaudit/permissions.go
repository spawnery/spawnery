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
	// ResourceNames, if set, is the exact set of objects this permission
	// covers. Empty means every object of the resource, which is what almost
	// every grant here is.
	//
	// It is part of the identity, so a named permission and an unnamed one are
	// different permissions and neither satisfies the other. That is the whole
	// point: an audit that let a named grant stand in for an unrestricted
	// requirement would report the operator able to read every Secret when it
	// can read one, and an audit that let an unrestricted grant stand in for a
	// named requirement would miss a role that had quietly widened.
	ResourceNames []string
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
	key := fmt.Sprintf("%s/%s:%s", p.Group, resource, p.Verb)
	if len(p.ResourceNames) == 0 {
		return key
	}
	// Sorted, so a role that lists the same names in a different order is the
	// same permission. RBAC does not care about the order and neither should
	// this; a copy, because sorting the caller's slice underneath them would
	// be a surprising thing for an identity function to do.
	names := append([]string(nil), p.ResourceNames...)
	sort.Strings(names)
	return key + " on [" + strings.Join(names, " ") + "]"
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
		// resourceNames is carried through rather than refused, which is what
		// this did until the chart began rendering a narrowed forwarding-secret
		// reader Role.
		//
		// The refusal was right for as long as Permission had nowhere to put a
		// name: such a rule would have expanded into one that reads as
		// unrestricted, and it would have read that way in the *permissive*
		// direction, with Compare reporting the requirement satisfied for
		// every object when it was satisfied for one. Now that the names are
		// part of the identity, a named rule and an unrestricted one are
		// simply different permissions and neither is mistaken for the other.
		//
		// controller-gen still emits no resourceNames, so nothing generated
		// reaches this; what does is hand-written and chart-rendered RBAC.
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
						// A copy per permission: one rule expands into many,
						// and handing them all the rule's own slice would make
						// a later sort of one reorder every other.
						ResourceNames: append([]string(nil), rule.ResourceNames...),
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

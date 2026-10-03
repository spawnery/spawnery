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

// Package rbacaudit checks the generated RBAC against a hand-maintained table
// of what the operator needs, in both directions. The file comparison here
// and the SubjectAccessReview envtest suite are deliberately redundant: they
// catch different mistakes.
package rbacaudit

import (
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

type Permission struct {
	// Group is the API group. The core group is the empty string.
	Group string
	// Resource is the plural resource name, without any subresource.
	Resource    string
	Subresource string
	Verb        string
	// ResourceNames is part of the identity: a named grant must not satisfy an
	// unrestricted requirement, nor the reverse. Empty means every object.
	ResourceNames []string
	// Why names the call site. Key ignores it.
	Why string
}

func (p Permission) Key() string {
	resource := p.Resource
	if p.Subresource != "" {
		resource += "/" + p.Subresource
	}
	key := fmt.Sprintf("%s/%s:%s", p.Group, resource, p.Verb)
	if len(p.ResourceNames) == 0 {
		return key
	}
	// A sorted copy: RBAC ignores order, and the caller's slice is not ours to sort.
	names := append([]string(nil), p.ResourceNames...)
	sort.Strings(names)
	return key + " on [" + strings.Join(names, " ") + "]"
}

func (p Permission) String() string {
	if p.Why == "" {
		return p.Key()
	}
	return p.Key() + " (" + p.Why + ")"
}

// ExpandRules refuses wildcards and NonResourceURLs, which this audit cannot
// reconcile against a finite table.
func ExpandRules(rules []rbacv1.PolicyRule) ([]Permission, error) {
	var out []Permission
	for i, rule := range rules {
		if len(rule.NonResourceURLs) > 0 {
			return nil, fmt.Errorf("rule %d uses non-resource URLs, which this audit cannot model", i)
		}
		// resourceNames is carried through; a named rule and an unrestricted one are
		// different permissions. Only hand-written and chart-rendered RBAC has them.
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
						// A copy per permission, so sorting one cannot reorder the others.
						ResourceNames: append([]string(nil), rule.ResourceNames...),
					})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

type Diff struct {
	// Missing is required but not granted: the operator will hit Forbidden.
	Missing []Permission
	// Extra is granted but not required: the role is wider than it needs to be.
	Extra []Permission
}

// Compare collapses duplicates on either side.
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

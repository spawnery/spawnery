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
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func perm(group, resource, sub, verb string) Permission {
	return Permission{Group: group, Resource: resource, Subresource: sub, Verb: verb}
}

func keys(perms []Permission) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, p.Key())
	}
	return out
}

// sameKeys compares two key lists element by element, so callers can assert
// exact order rather than just set membership.
func sameKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestKeyIgnoresWhy(t *testing.T) {
	a := Permission{Group: "", Resource: "pods", Verb: "get", Why: "one place"}
	b := Permission{Group: "", Resource: "pods", Verb: "get", Why: "another"}
	if a.Key() != b.Key() {
		t.Errorf("Key differs on Why alone: %q vs %q", a.Key(), b.Key())
	}
}

func TestKeyDistinguishesSubresource(t *testing.T) {
	bare := perm("spawnery.cloud", "servers", "", "update")
	status := perm("spawnery.cloud", "servers", "status", "update")
	if bare.Key() == status.Key() {
		t.Fatalf("bare and subresource share a key: %q", bare.Key())
	}
}

func TestExpandRules(t *testing.T) {
	cases := []struct {
		name  string
		rules []rbacv1.PolicyRule
		want  []string
	}{
		{
			name: "cross product of groups, resources and verbs",
			rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"spawnery.cloud"},
				Resources: []string{"networks", "servergroups"},
				Verbs:     []string{"get", "list"},
			}},
			want: []string{
				"spawnery.cloud/networks:get",
				"spawnery.cloud/networks:list",
				"spawnery.cloud/servergroups:get",
				"spawnery.cloud/servergroups:list",
			},
		},
		{
			name: "the core group is the empty string",
			rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"delete"},
			}},
			want: []string{"/pods:delete"},
		},
		{
			name: "a subresource is split off the resource",
			rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"spawnery.cloud"},
				Resources: []string{"servers/status"},
				Verbs:     []string{"update"},
			}},
			want: []string{"spawnery.cloud/servers/status:update"},
		},
		{
			name:  "no rules yield no permissions",
			rules: nil,
			want:  nil,
		},
		{
			// The input order here is deliberately not alphabetical, so that
			// this case only passes if the result is actually sorted rather
			// than merely reflecting iteration order by coincidence.
			name: "results come back sorted by key regardless of input order",
			rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"spawnery.cloud", ""},
				Resources: []string{"servers"},
				Verbs:     []string{"update", "get"},
			}},
			want: []string{
				"/servers:get",
				"/servers:update",
				"spawnery.cloud/servers:get",
				"spawnery.cloud/servers:update",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandRules(tc.rules)
			if err != nil {
				t.Fatalf("ExpandRules: %v", err)
			}
			gotKeys := keys(got)
			if len(gotKeys) != len(tc.want) {
				t.Fatalf("got %v, want %v", gotKeys, tc.want)
			}
			for i := range gotKeys {
				if gotKeys[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", gotKeys, tc.want)
				}
			}
		})
	}
}

// TestExpandRulesSplitsSubresourceFields checks the Resource and Subresource
// fields directly. Key() renders an unsplit "servers/status" identically to a
// split Resource="servers"/Subresource="status", so a test that only checks
// Key() (as TestExpandRules's own subresource case does) cannot tell the
// split apart from a no-op strings.Cut.
func TestExpandRulesSplitsSubresourceFields(t *testing.T) {
	got, err := ExpandRules([]rbacv1.PolicyRule{{
		APIGroups: []string{"spawnery.cloud"},
		Resources: []string{"servers/status"},
		Verbs:     []string{"update"},
	}})
	if err != nil {
		t.Fatalf("ExpandRules: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d permissions, want 1: %v", len(got), got)
	}
	if got[0].Resource != "servers" || got[0].Subresource != "status" {
		t.Fatalf("got Resource=%q Subresource=%q, want Resource=%q Subresource=%q",
			got[0].Resource, got[0].Subresource, "servers", "status")
	}
}

// TestExpandRulesRejectsWildcards is the point of this function: a wildcard
// grants everything in its position, so it can never be matched against a
// finite table. Treating it as an over-grant is the only honest answer.
func TestExpandRulesRejectsWildcards(t *testing.T) {
	cases := []struct {
		name string
		rule rbacv1.PolicyRule
	}{
		{"wildcard group", rbacv1.PolicyRule{
			APIGroups: []string{"*"}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
		{"wildcard resource", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"*"}, Verbs: []string{"get"}}},
		{"wildcard verb", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"*"}}},
		{"wildcard subresource", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"pods/*"}, Verbs: []string{"get"}}},
		{"wildcard resource name with a concrete subresource", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"*/status"}, Verbs: []string{"get"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExpandRules([]rbacv1.PolicyRule{tc.rule}); err == nil {
				t.Fatal("wildcard accepted, want an error")
			}
		})
	}
}

// TestExpandRulesRejectsNonResourceURLs guards against the same failure mode
// as the wildcard rejection above, from a different angle: a rule using
// NonResourceURLs has empty APIGroups and Resources, so the group/resource
// loops in ExpandRules simply do not run for it. Without an explicit check,
// such a rule would silently expand to zero permissions instead of erroring,
// and the audit would report a role granting non-resource access as if it
// granted nothing at all.
func TestExpandRulesRejectsNonResourceURLs(t *testing.T) {
	rule := rbacv1.PolicyRule{
		NonResourceURLs: []string{"/healthz"},
		Verbs:           []string{"get"},
	}
	if _, err := ExpandRules([]rbacv1.PolicyRule{rule}); err == nil {
		t.Fatal("non-resource URL rule accepted, want an error")
	}
}

func TestCompare(t *testing.T) {
	required := []Permission{
		perm("", "pods", "", "get"),
		perm("", "pods", "", "delete"),
		perm("spawnery.cloud", "servers", "status", "update"),
	}

	t.Run("exact match yields nothing", func(t *testing.T) {
		d := Compare(required, required)
		if len(d.Missing) != 0 || len(d.Extra) != 0 {
			t.Errorf("diff = %+v, want empty", d)
		}
	})

	t.Run("a granted verb the table does not list is extra", func(t *testing.T) {
		granted := append(append([]Permission{}, required...), perm("", "pods", "", "update"))
		d := Compare(required, granted)
		if len(d.Extra) != 1 || d.Extra[0].Key() != "/pods:update" {
			t.Errorf("extra = %v, want exactly /pods:update", keys(d.Extra))
		}
		if len(d.Missing) != 0 {
			t.Errorf("missing = %v, want none", keys(d.Missing))
		}
	})

	t.Run("a required verb the role does not grant is missing", func(t *testing.T) {
		granted := required[:2]
		d := Compare(required, granted)
		if len(d.Missing) != 1 || d.Missing[0].Key() != "spawnery.cloud/servers/status:update" {
			t.Errorf("missing = %v, want exactly spawnery.cloud/servers/status:update", keys(d.Missing))
		}
		if len(d.Extra) != 0 {
			t.Errorf("extra = %v, want none", keys(d.Extra))
		}
	})

	t.Run("both directions at once", func(t *testing.T) {
		granted := []Permission{
			perm("", "pods", "", "get"),
			perm("", "secrets", "", "list"),
		}
		d := Compare(required, granted)
		if len(d.Missing) != 2 {
			t.Errorf("missing = %v, want two entries", keys(d.Missing))
		}
		if len(d.Extra) != 1 || d.Extra[0].Key() != "/secrets:list" {
			t.Errorf("extra = %v, want exactly /secrets:list", keys(d.Extra))
		}
	})

	t.Run("a duplicate in the table is not reported as extra", func(t *testing.T) {
		granted := []Permission{perm("", "pods", "", "get"), perm("", "pods", "", "get")}
		d := Compare([]Permission{perm("", "pods", "", "get")}, granted)
		if len(d.Extra) != 0 {
			t.Errorf("extra = %v, want none", keys(d.Extra))
		}
	})

	// In real use, Required entries always carry a Why and ExpandRules output
	// never does — so if Compare ever matched on the whole struct instead of
	// Key(), every single permission would come back both missing and extra
	// at once, no matter how well the role matches the table.
	t.Run("Why does not affect matching", func(t *testing.T) {
		withWhy := []Permission{
			{Group: "", Resource: "pods", Verb: "get", Why: "watch loop"},
			{Group: "", Resource: "pods", Verb: "delete", Why: "cleanup on scale-down"},
			{Group: "spawnery.cloud", Resource: "servers", Subresource: "status", Verb: "update", Why: "status writer"},
		}
		withoutWhy := []Permission{
			perm("", "pods", "", "get"),
			perm("", "pods", "", "delete"),
			perm("spawnery.cloud", "servers", "status", "update"),
		}
		d := Compare(withWhy, withoutWhy)
		if len(d.Missing) != 0 || len(d.Extra) != 0 {
			t.Errorf("diff = %+v, want empty — Why must not affect matching", d)
		}
	})

	// Compare's output feeds directly into failure messages; a random order
	// makes those messages jump around between runs. Enough entries here
	// that a would-be regression to unsorted output has only a 1-in-120
	// chance of coincidentally landing in sorted order via map iteration.
	t.Run("missing and extra are each returned in sorted order", func(t *testing.T) {
		manyRequired := []Permission{
			perm("", "secrets", "", "get"),
			perm("", "configmaps", "", "get"),
			perm("", "pods", "", "get"),
			perm("", "namespaces", "", "get"),
			perm("", "events", "", "get"),
		}
		manyGranted := []Permission{
			perm("", "zzz", "", "get"),
			perm("", "aaa", "", "get"),
			perm("", "mmm", "", "get"),
			perm("", "bbb", "", "get"),
			perm("", "ccc", "", "get"),
		}
		d := Compare(manyRequired, manyGranted)
		wantMissing := []string{"/configmaps:get", "/events:get", "/namespaces:get", "/pods:get", "/secrets:get"}
		wantExtra := []string{"/aaa:get", "/bbb:get", "/ccc:get", "/mmm:get", "/zzz:get"}
		if !sameKeys(keys(d.Missing), wantMissing) {
			t.Errorf("missing = %v, want %v in that exact order", keys(d.Missing), wantMissing)
		}
		if !sameKeys(keys(d.Extra), wantExtra) {
			t.Errorf("extra = %v, want %v in that exact order", keys(d.Extra), wantExtra)
		}
	})
}

// TestExpandRulesRejectsResourceNames guards the same failure mode as the
// wildcard rejection, from the opposite direction. A wildcard grants more than
// the table can express and is refused as an over-grant. A resourceNames
// restriction grants *less* than the table can express, and expanding it
// anyway is the more dangerous of the two: the Permission carries no name, so
// the rule reads as unrestricted and Compare reports the requirement satisfied
// for every object when it holds for one.
//
// controller-gen emits no resourceNames, which is why this went unnoticed. But
// docs/known-issues.md records that the master design asks for resourceNames on
// the forwarding-secret reader Role, so the case is not hypothetical — it is
// waiting for somebody to follow that advice.
func TestExpandRulesRejectsResourceNames(t *testing.T) {
	rule := rbacv1.PolicyRule{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		Verbs:         []string{"get"},
		ResourceNames: []string{"one-particular-secret"},
	}
	got, err := ExpandRules([]rbacv1.PolicyRule{rule})
	if err == nil {
		t.Fatalf("a resourceNames restriction was accepted and expanded to %v; it has to be "+
			"refused, because that expansion claims a grant on every secret", got)
	}
	// The message has to name the restriction, or whoever meets it cannot tell
	// which rule to look at in a file with a dozen.
	if !strings.Contains(err.Error(), "one-particular-secret") {
		t.Errorf("error = %q, want it to name the resourceNames it refused", err)
	}
}

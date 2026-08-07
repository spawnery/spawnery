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

func TestKeyIgnoresWhy(t *testing.T) {
	a := Permission{Group: "", Resource: "pods", Verb: "get", Why: "eine Stelle"}
	b := Permission{Group: "", Resource: "pods", Verb: "get", Why: "eine andere"}
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExpandRules([]rbacv1.PolicyRule{tc.rule}); err == nil {
				t.Fatal("wildcard accepted, want an error")
			}
		})
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
}

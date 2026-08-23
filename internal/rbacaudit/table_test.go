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
)

// tables is every hand-maintained permission table in this package, named so a
// failure says which one. A table added later and not listed here is outside
// both checks below, which is the one way these can be quietly bypassed --
// there is no reflection over package-level vars that would catch it, and
// inventing one would be a worse trade than this comment.
func tables() map[string][]Permission {
	return map[string][]Permission{
		"RequiredCluster":          RequiredCluster,
		"RequiredNamespaced":       RequiredNamespaced,
		"RequiredNetworkNamespace": RequiredNetworkNamespace,
	}
}

// TestEveryRequiredPermissionSaysWhyItIsThere is the check docs/known-issues.md
// records as missing: "Nothing enforces that Why is filled in and Required is
// free of duplicates."
//
// Why is documentation rather than identity -- Permission.Key ignores it -- and
// that is exactly what makes it rot unwatched. Two entries had gone stale
// before anybody read them against the code: the pods:patch grant named one of
// its two call sites, and the configmaps grant named one of its three. Both
// were found by hand, months apart, and neither could have been found by any
// test in the tree. An empty Why is the same defect at its starting point: a
// permission nobody has to justify is a permission nobody can later argue
// away, and the table's whole purpose is to be the argument.
func TestEveryRequiredPermissionSaysWhyItIsThere(t *testing.T) {
	for name, table := range tables() {
		for _, p := range table {
			if strings.TrimSpace(p.Why) == "" {
				t.Errorf("%s: %s has no Why. The table is the argument for granting it — "+
					"name the call site that needs it, the way its neighbours do",
					name, p.Key())
			}
		}
	}
}

// TestNoRequiredPermissionIsListedTwice guards the other half, and it matters
// for a reason the duplicate itself hides: Compare builds a map keyed by
// Permission.Key, so a second entry for one key silently replaces the first.
// Both are granted identically -- the audit still passes -- but only the last
// one's Why survives into any message the audit prints, so a duplicate pair
// whose two Whys disagree resolves to whichever happens to be written lower in
// the file. That is a coin toss deciding which explanation a future reader
// gets.
func TestNoRequiredPermissionIsListedTwice(t *testing.T) {
	for name, table := range tables() {
		seen := make(map[string]Permission, len(table))
		for _, p := range table {
			if first, ok := seen[p.Key()]; ok {
				t.Errorf("%s lists %s twice.\n  first:  %s\n  second: %s\n"+
					"Compare keys on the permission and keeps the last, so only the second "+
					"Why would ever be printed. Delete one, or merge them into a single "+
					"entry naming both call sites.",
					name, p.Key(), first.Why, p.Why)
				continue
			}
			seen[p.Key()] = p
		}
	}
}

// TestTheTablesDoNotOverlapEachOther is the cross-table case the two above
// cannot see. The three tables become three different Kubernetes objects — a
// ClusterRole and two Roles in different namespaces — so the same permission
// appearing in two of them is not automatically wrong. What is wrong is the
// silent kind: a namespaced entry duplicating a cluster-scoped one is already
// granted everywhere by the ClusterRole, and the Role that repeats it is dead
// weight nobody will dare remove later without this test to say what it found.
func TestTheTablesDoNotOverlapEachOther(t *testing.T) {
	cluster := make(map[string]Permission, len(RequiredCluster))
	for _, p := range RequiredCluster {
		cluster[p.Key()] = p
	}
	for name, table := range map[string][]Permission{
		"RequiredNamespaced":       RequiredNamespaced,
		"RequiredNetworkNamespace": RequiredNetworkNamespace,
	} {
		for _, p := range table {
			if c, ok := cluster[p.Key()]; ok {
				t.Errorf("%s lists %s, which RequiredCluster already grants everywhere.\n"+
					"  cluster: %s\n  %s: %s\n"+
					"A ClusterRole grant reaches every namespace, so the namespaced entry "+
					"adds nothing. Drop whichever one is the accident.",
					name, p.Key(), c.Why, name, p.Why)
			}
		}
	}
}

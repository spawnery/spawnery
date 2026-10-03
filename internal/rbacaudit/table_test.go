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
	"strings"
	"testing"
)

// tables must list every permission table; there is no reflection over
// package-level vars to catch one left out.
func tables() map[string][]Permission {
	return map[string][]Permission{
		"RequiredCluster":          RequiredCluster,
		"RequiredNamespaced":       RequiredNamespaced,
		"RequiredNetworkNamespace": RequiredNetworkNamespace,
	}
}

// Why is not part of the identity, so nothing else notices it missing.
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

// Compare keys a map by Permission.Key, so a duplicate silently replaces the
// first and only the lower entry's Why survives.
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

// A namespaced entry duplicating a cluster-scoped one is dead weight in a
// Role the ClusterRole already covers.
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

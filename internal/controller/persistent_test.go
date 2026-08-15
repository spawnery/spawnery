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

package controller

import "testing"

func TestPersistentServerName(t *testing.T) {
	if got := PersistentServerName("survival", 0); got != "survival-0" {
		t.Errorf("PersistentServerName(survival, 0) = %q, want survival-0", got)
	}
	if got := PersistentServerName("survival", 12); got != "survival-12" {
		t.Errorf("PersistentServerName(survival, 12) = %q, want survival-12", got)
	}
}

func TestOrdinalOf(t *testing.T) {
	tests := []struct {
		name   string
		group  string
		server string
		want   int32
		wantOK bool
	}{
		{"the ordinary case", "survival", "survival-0", 0, true},
		{"more than one digit", "survival", "survival-12", 12, true},
		{"a different group's server", "survival", "creative-0", 0, false},
		{"an ephemeral name from the same group", "survival", "survival-a7kd", 0, false},
		{"the group name alone", "survival", "survival", 0, false},
		{"a negative number is not an ordinal", "survival", "survival--1", 0, false},
		{"a leading zero is not the same ordinal", "survival", "survival-01", 0, false},
		{"empty", "survival", "", 0, false},
		// A group whose own name ends in a number is the case that breaks a
		// naive suffix split: the boundary is the last hyphen, and everything
		// before it must equal the group exactly.
		{"a group name ending in a digit", "survival-2", "survival-2-3", 3, true},
		{"that group's own name is not one of its servers", "survival-2", "survival-2", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OrdinalOf(tc.group, tc.server)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("OrdinalOf(%q, %q) = (%d, %v), want (%d, %v)",
					tc.group, tc.server, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

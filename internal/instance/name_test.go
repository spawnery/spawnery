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

package instance_test

import (
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/instance"
)

func TestNameComposesGroupAndKey(t *testing.T) {
	got, err := instance.Name("private-servers", "c0ffee")
	if err != nil {
		t.Fatalf("Name: %v", err)
	}
	if got != "private-servers-c0ffee" {
		t.Fatalf("Name = %q", got)
	}
}

func TestNameRefusesWhatKubernetesWould(t *testing.T) {
	tests := map[string]struct{ group, key string }{
		"upper case": {"private-servers", "C0FFEE"},
		"underscore": {"private-servers", "c0f_fee"},
		"empty":      {"private-servers", ""},
		"too long":   {"private-servers-of-the-whole-network", strings.Repeat("a", 36)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := instance.Name(tc.group, tc.key); err == nil {
				t.Fatal("accepted, so the failure would land on a player's start instead")
			}
		})
	}
}

func TestNameAcceptsAUUID(t *testing.T) {
	if _, err := instance.Name("private-servers", "3f2b1c8a-9d4e-4f11-b2a6-77c0de1234ab"); err != nil {
		t.Fatalf("a UUID key was refused: %v", err)
	}
}

func TestKeptNameRefusesTheRandomSuffixShape(t *testing.T) {
	for _, key := range []string{"abcd", "x7k2", "2345"} {
		if _, err := instance.KeptName("rounds", key); err == nil {
			t.Errorf("key %q was accepted; it could name a server the group creates itself", key)
		}
	}
	for _, key := range []string{"abc", "abcde", "ab-d", "abci", "round-7", "3f2b1c8a-9d4e-4f11-b2a6-77c0de1234ab"} {
		if got, err := instance.KeptName("rounds", key); err != nil || got != "rounds-"+key {
			t.Errorf("key %q: %q, %v", key, got, err)
		}
	}
	if _, err := instance.KeptName("rounds", "C0FFEE"); err == nil {
		t.Error("KeptName skipped Name's checks")
	}
}

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

// Package v1alpha1, not v1alpha1_test: DrainTimeout's default is asserted
// against defaultProxyDrainTimeout itself, not a duplicated literal, so a
// future edit to the constant cannot silently drift out of step with what
// this test checks. That needs the unexported constant, which only a test in
// the same package can reach.
package v1alpha1

import (
	"testing"
	"time"
)

// TestDrainTimeoutDefaultsWhenTheFieldIsAbsent proves the accessor, not just
// the field: spec.drain is +optional, so a ProxyGroup that never set it must
// still get a bounded drain rather than an unbounded one.
func TestDrainTimeoutDefaultsWhenTheFieldIsAbsent(t *testing.T) {
	g := &ProxyGroup{}
	if got := g.DrainTimeout(); got != defaultProxyDrainTimeout {
		t.Errorf("DrainTimeout() = %v with no spec.drain, want %v", got, defaultProxyDrainTimeout)
	}
}

// TestDrainTimeoutHonorsAnExplicitValue proves the field is actually read,
// not merely present: a test that only checked the default would stay green
// even if DrainTimeout ignored spec.drain entirely.
func TestDrainTimeoutHonorsAnExplicitValue(t *testing.T) {
	g := &ProxyGroup{Spec: ProxyGroupSpec{Drain: &DrainSpec{TimeoutSeconds: 45}}}
	if got, want := g.DrainTimeout(), 45*time.Second; got != want {
		t.Errorf("DrainTimeout() = %v, want %v", got, want)
	}
}

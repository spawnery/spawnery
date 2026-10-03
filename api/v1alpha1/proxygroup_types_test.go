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

// Package v1alpha1, not v1alpha1_test, to reach defaultProxyDrainTimeout.
package v1alpha1

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"
)

func TestDrainTimeoutDefaultsWhenTheFieldIsAbsent(t *testing.T) {
	g := &ProxyGroup{}
	if got := g.DrainTimeout(); got != defaultProxyDrainTimeout {
		t.Errorf("DrainTimeout() = %v with no spec.drain, want %v", got, defaultProxyDrainTimeout)
	}
}

func TestDrainTimeoutHonorsAnExplicitValue(t *testing.T) {
	g := &ProxyGroup{Spec: ProxyGroupSpec{Drain: &DrainSpec{TimeoutSeconds: 45}}}
	if got, want := g.DrainTimeout(), 45*time.Second; got != want {
		t.Errorf("DrainTimeout() = %v, want %v", got, want)
	}
}

func TestTransferForceAfter(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    *ProxyUpdateSpec
		want time.Duration
		on   bool
	}{
		{"no update", nil, 0, false},
		{"update without transfer", &ProxyUpdateSpec{}, 0, false},
		{"transfer with defaults", &ProxyUpdateSpec{Transfer: &ProxyTransferSpec{}}, 120 * time.Second, true},
		{"transfer at once", &ProxyUpdateSpec{Transfer: &ProxyTransferSpec{ForceAfterSeconds: ptr.To[int32](0)}}, 0, true},
		{"transfer after 90", &ProxyUpdateSpec{Transfer: &ProxyTransferSpec{ForceAfterSeconds: ptr.To[int32](90)}}, 90 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &ProxyGroup{Spec: ProxyGroupSpec{Update: tc.u}}
			got, on := g.TransferForceAfter()
			if got != tc.want || on != tc.on {
				t.Errorf("TransferForceAfter() = %v, %v; want %v, %v", got, on, tc.want, tc.on)
			}
		})
	}
}

func TestProxyGroupMaxStale(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update *ProxyUpdateSpec
		want   time.Duration
	}{
		{"no update block", nil, 0},
		{"zero means never", &ProxyUpdateSpec{MaxStaleSeconds: 0}, 0},
		{"a bound", &ProxyUpdateSpec{MaxStaleSeconds: 600}, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &ProxyGroup{Spec: ProxyGroupSpec{Update: tc.update}}
			if got := g.MaxStale(); got != tc.want {
				t.Errorf("MaxStale() = %v, want %v", got, tc.want)
			}
		})
	}
}

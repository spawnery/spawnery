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

package main

import "testing"

// TestVerifyObservedTypesFitInferType exercises the check against
// hand-built observed-type maps rather than the real metrics registry: the
// function's whole input is a map[string]string, so a histogram or a
// convention violation is a map literal here, not something that would
// require registering a fake metric into sigs.k8s.io/controller-runtime's
// shared registry to produce.
func TestVerifyObservedTypesFitInferType(t *testing.T) {
	cases := []struct {
		name     string
		observed map[string]string
		wantErr  bool
	}{
		{
			name: "counter ending in _total, gauge not ending in _total",
			observed: map[string]string{
				"spawnery_agent_open_connections":          "gauge",
				"spawnery_agent_connections_refused_total": "counter",
			},
		},
		{
			name: "counter not ending in _total",
			observed: map[string]string{
				"spawnery_agent_open_connections": "counter",
			},
			wantErr: true,
		},
		{
			name: "gauge ending in _total",
			observed: map[string]string{
				"spawnery_agent_connections_refused_total": "gauge",
			},
			wantErr: true,
		},
		{
			name: "a histogram with a sample",
			observed: map[string]string{
				"spawnery_reconcile_duration_seconds": "histogram",
			},
			wantErr: true,
		},
		{
			name: "a summary with a sample",
			observed: map[string]string{
				"spawnery_reconcile_duration_seconds": "summary",
			},
			wantErr: true,
		},
		{
			name:     "nothing observed yet",
			observed: map[string]string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := verifyObservedTypesFitInferType(c.observed)
			if c.wantErr && err == nil {
				t.Fatal("want an error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

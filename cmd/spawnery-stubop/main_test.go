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

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterialiseWritesACaBundleAndTokenTheAgentCanRead(t *testing.T) {
	dir := t.TempDir()

	material, err := materialise(dir, []string{"stubop"})
	if err != nil {
		t.Fatalf("materialise: %v", err)
	}

	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("ca.crt: %v", err)
	}
	if !strings.HasPrefix(string(ca), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("ca.crt is not PEM: %q", ca[:min(40, len(ca))])
	}

	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if len(token) == 0 {
		t.Error("the token file is empty")
	}

	// The agent validates the serving certificate against the mounted bundle
	// and nothing else, so the SAN has to be the name the container dials.
	if got := material.Certificate.Leaf.DNSNames; len(got) != 1 || got[0] != "stubop" {
		t.Errorf("SANs = %v, want [stubop]", got)
	}
}

func TestEventsAreOneJSONObjectPerLine(t *testing.T) {
	var out strings.Builder
	recorder := newRecorder(&out)

	recorder.record("hello", map[string]any{"version": "26.2-0.2.0", "ready": true})

	var event struct {
		Kind    string `json:"kind"`
		Version string `json:"version"`
		Ready   bool   `json:"ready"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &event); err != nil {
		t.Fatalf("not a JSON line: %v (%q)", err, out.String())
	}
	if event.Kind != "hello" || event.Version != "26.2-0.2.0" || !event.Ready {
		t.Errorf("event = %+v", event)
	}
}

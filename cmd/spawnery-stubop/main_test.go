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
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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

// TestMaterialiseRotatedSignsWithTheSecondCAOfTheBundle is the cheap half of
// the --rotate-ca proof: that the fixture is built the way hack/agent-test.sh
// phase 6 assumes, before a container is ever involved. The expensive half --
// that a real JVM's TLS stack accepts it -- can only run there; see the
// script for why.
func TestMaterialiseRotatedSignsWithTheSecondCAOfTheBundle(t *testing.T) {
	dir := t.TempDir()

	material, err := materialiseRotated(dir, []string{"stubop"})
	if err != nil {
		t.Fatalf("materialiseRotated: %v", err)
	}

	bundle, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("ca.crt: %v", err)
	}
	var cas []*x509.Certificate
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("ca.crt holds a block that does not parse: %v", err)
		}
		cas = append(cas, cert)
	}
	if len(cas) != 2 {
		t.Fatalf("ca.crt holds %d certificates, want 2", len(cas))
	}

	if material.Certificate.Leaf == nil {
		t.Fatal("the serving certificate has no parsed Leaf to check the chain against")
	}

	second := x509.NewCertPool()
	second.AddCert(cas[1])
	if _, err := material.Certificate.Leaf.Verify(x509.VerifyOptions{
		Roots:     second,
		DNSName:   "stubop",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("the serving certificate does not chain to ca.crt's second entry: %v", err)
	}

	// The mutation hack/agent-test.sh's phase exists to fail: mounting only the
	// bundle's first PEM is the pre-rotation state, and a serving certificate
	// that also chained to it would mean this fixture never moved the
	// signature at all.
	first := x509.NewCertPool()
	first.AddCert(cas[0])
	if _, err := material.Certificate.Leaf.Verify(x509.VerifyOptions{
		Roots:     first,
		DNSName:   "stubop",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("the serving certificate also chains to ca.crt's first entry, so mounting only it would not fail")
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

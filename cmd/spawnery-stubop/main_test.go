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

package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/agentpb"
	"google.golang.org/grpc"
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

	if got := material.Certificate.Leaf.DNSNames; len(got) != 1 || got[0] != "stubop" {
		t.Errorf("SANs = %v, want [stubop]", got)
	}
}

// TestMaterialiseRotatedSignsWithTheSecondCAOfTheBundle checks the fixture
// hack/agent-test.sh phase 6 relies on; whether a JVM accepts it is that
// phase's job.
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

// TestSeqIsUniqueUnderConcurrentRecorders: every ordering assertion in
// hack/agent-test.sh compares seq values across concurrent streams.
func TestSeqIsUniqueUnderConcurrentRecorders(t *testing.T) {
	var out lockedBuffer
	recorder := newRecorder(&out)

	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				recorder.record("player_count", map[string]any{"writer": w})
			}
		}(w)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != writers*each {
		t.Fatalf("lines = %d, want %d: a concurrent write was lost or interleaved", len(lines), writers*each)
	}
	seen := make(map[int]bool, len(lines))
	for _, line := range lines {
		var event struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("not a JSON line: %v (%q)", err, line)
		}
		if seen[event.Seq] {
			t.Fatalf("seq %d appears twice", event.Seq)
		}
		seen[event.Seq] = true
	}
	// Dense as well as unique.
	for i := 0; i < writers*each; i++ {
		if !seen[i] {
			t.Errorf("seq %d is missing from a run of %d events", i, writers*each)
		}
	}
}

// lockedBuffer keeps the test itself from being the race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeStream is a grpc.BidiStreamingServer whose Recv the test drives.
type fakeStream struct {
	grpc.ServerStream
	recv chan error
	sent int
}

func (f *fakeStream) Recv() (*agentpb.ServerMessage, error) {
	if err := <-f.recv; err != nil {
		return nil, err
	}
	return &agentpb.ServerMessage{}, nil
}

func (f *fakeStream) Send(*agentpb.OperatorToServer) error {
	f.sent++
	return nil
}

func (f *fakeStream) Context() context.Context { return context.Background() }

// TestTheStubNeverClosesAStreamOfItsOwnAccord: hack/agent-test.sh reads every
// stream_closed in the trace as the agent's doing.
func TestTheStubNeverClosesAStreamOfItsOwnAccord(t *testing.T) {
	var out lockedBuffer
	served := &stub{events: newRecorder(&out), reportInterval: 1, renewAfter: 5, hardDeadline: 20}
	stream := &fakeStream{recv: make(chan error)}

	done := make(chan error, 1)
	go func() {
		done <- serveSession[agentpb.ServerMessage, agentpb.OperatorToServer](
			served, stream, nil, nil,
			func(of func(map[string]any) map[string]any, _ *agentpb.ServerMessage) *agentpb.OperatorToServer {
				served.events.record("observed", of(map[string]any{}))
				return nil
			})
	}()

	for i := 0; i < 3; i++ {
		stream.recv <- nil
	}
	select {
	case err := <-done:
		t.Fatalf("the stub ended the stream on its own after %d messages: %v", 3, err)
	case <-time.After(200 * time.Millisecond):
	}
	if strings.Contains(out.String(), "stream_closed") {
		t.Fatalf("a stream_closed was recorded while the stream was open:\n%s", out.String())
	}

	stream.recv <- io.EOF
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveSession returned %v, want nil: the agent closing a renewed stream is the expected outcome", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveSession did not return after the agent closed the stream")
	}
	if !strings.Contains(out.String(), "stream_closed") {
		t.Errorf("no stream_closed was recorded after the agent closed:\n%s", out.String())
	}
}

func TestTheMutingAndSupersedingGatesAreOffByDefault(t *testing.T) {
	// -1 is the flag's default.
	served := &stub{muteAfter: -1}
	for _, index := range []int64{0, 1, 7, 1 << 20} {
		if served.muted(index) {
			t.Errorf("muted(%d) = true with muteAfter -1", index)
		}
	}
	if served.supersede {
		t.Error("supersede is set on a zero-valued stub")
	}
	served = &stub{muteAfter: 2}
	if served.muted(1) || !served.muted(2) || !served.muted(3) {
		t.Error("muteAfter 2 does not mute from the third stream onward")
	}
}

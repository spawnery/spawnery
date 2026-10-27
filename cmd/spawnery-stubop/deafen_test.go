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
	"crypto/tls"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/agentpb"
)

func TestADeafenedStubStillStopsOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan os.Signal, 1)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- run([]string{"--dir", dir, "--listen", "127.0.0.1:0", "--deafen-after", "1s"}, stdout, stderr, stop)
	}()

	var addr string
	for deadline := time.Now().Add(10 * time.Second); addr == ""; {
		if _, rest, ok := strings.Cut(stderr.String(), "serving AgentService on "); ok {
			addr, _, _ = strings.Cut(rest, " ")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stub never started serving: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	stream := serverStream(t, dir, addr)
	if err := stream.Send(&agentpb.ServerMessage{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(stdout.String(), `"deafened"`); {
		if time.Now().After(deadline) {
			t.Fatalf("the stub never deafened: %s", stdout.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Traffic after the deafening, so a Read is parked on the deaf connection.
	for range 20 {
		_ = stream.Send(&agentpb.ServerMessage{})
		time.Sleep(10 * time.Millisecond)
	}
	// What an agent does once its keepalive gives up: a new connection,
	// whose TLS handshake the stub never answers.
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	go func() {
		_ = tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}).Handshake()
	}()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(stdout.String(), `"open":2`); {
		if time.Now().After(deadline) {
			t.Fatalf("the second connection never arrived: %s", stdout.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	stop <- syscall.SIGTERM
	select {
	case code := <-exited:
		if code != 0 {
			t.Errorf("exit %d, want 0: %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the deafened stub was still running 5s after SIGTERM")
	}
}

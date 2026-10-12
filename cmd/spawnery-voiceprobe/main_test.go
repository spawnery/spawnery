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
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

const player = "0f8fad5b-d9cb-469f-a165-70867728950e"

type printed struct {
	OK    bool   `json:"ok"`
	RTT   *int64 `json:"rtt_ms"`
	Error string `json:"error"`
}

func decode(t *testing.T, out string) printed {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one JSON line, got %d: %q", len(lines), out)
	}
	var r printed
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil {
		t.Fatalf("not a JSON line: %v (%q)", err, out)
	}
	return r
}

func echo(t *testing.T, answer func([]byte) []byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(answer(buf[:n]), from)
		}
	}()
	return conn.LocalAddr().String()
}

func TestAnEchoedFrameIsOK(t *testing.T) {
	got := make(chan []byte, 1)
	addr := echo(t, func(frame []byte) []byte {
		got <- append([]byte(nil), frame...)
		return append([]byte("echo:"), frame...)
	})

	var out, errOut bytes.Buffer
	code := run([]string{"--to", addr, "--uuid", player}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	r := decode(t, out.String())
	if !r.OK || r.RTT == nil || r.Error != "" {
		t.Errorf("result = %+v, want ok with an rtt and no error", r)
	}

	frame := <-got
	want := append([]byte{0xFF,
		0x0f, 0x8f, 0xad, 0x5b, 0xd9, 0xcb, 0x46, 0x9f,
		0xa1, 0x65, 0x70, 0x86, 0x77, 0x28, 0x95, 0x0e}, "probe"...)
	if !bytes.Equal(frame, want) {
		t.Errorf("frame = %x, want %x", frame, want)
	}
}

func TestAReplyThatIsNotTheEchoFails(t *testing.T) {
	addr := echo(t, func(frame []byte) []byte {
		return append([]byte("echo:"), frame[:len(frame)-1]...)
	})

	var out, errOut bytes.Buffer
	if code := run([]string{"--to", addr, "--uuid", player}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %q", code, out.String())
	}
	if r := decode(t, out.String()); r.OK || r.Error == "" {
		t.Errorf("result = %+v, want not ok with an error", r)
	}
}

func TestAClosedPortFailsWithinTheTimeout(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := conn.LocalAddr().String()
	_ = conn.Close()

	var out, errOut bytes.Buffer
	start := time.Now()
	code := run([]string{"--to", addr, "--uuid", player, "--timeout", "2s"}, &out, &errOut)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v against a 2s timeout", elapsed)
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout %q", code, out.String())
	}
	if r := decode(t, out.String()); r.OK || r.Error == "" {
		t.Errorf("result = %+v, want not ok with an error", r)
	}
}

func TestASilentPeerFailsAtTheTimeout(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var out, errOut bytes.Buffer
	start := time.Now()
	code := run([]string{"--to", conn.LocalAddr().String(), "--uuid", player, "--timeout", "500ms"}, &out, &errOut)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v against a 500ms timeout", elapsed)
	}
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout %q", code, out.String())
	}
	if r := decode(t, out.String()); r.OK || r.Error == "" {
		t.Errorf("result = %+v, want not ok with an error", r)
	}
}

func TestAMalformedUUIDIsRefused(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--to", "127.0.0.1:1", "--uuid", "not-a-uuid"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

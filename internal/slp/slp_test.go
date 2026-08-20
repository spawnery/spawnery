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

package slp

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/mcproto"
)

// serve starts a one-shot fake Minecraft server on a loopback port and hands
// the accepted connection to handle. It returns the address Ping should dial.
func serve(t *testing.T, handle func(net.Conn)) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handle(conn)
	}()

	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// drainRequest reads the handshake and the status request. Without it, closing
// the connection in a handler can reset the client mid-write, and the test
// would fail on the wrong error.
func drainRequest(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	_, _ = c.Read(buf)
	_ = c.SetReadDeadline(time.Time{})
}

// statusResponse frames a status document the way a server does.
func statusResponse(doc string) []byte {
	var body []byte
	body = mcproto.AppendVarInt(body, 0x00)
	body = mcproto.AppendString(body, doc)

	var frame []byte
	frame = mcproto.AppendVarInt(frame, int32(len(body)))
	return append(frame, body...)
}

func TestPingReadsTheStatusDocument(t *testing.T) {
	host, port := serve(t, func(c net.Conn) {
		drainRequest(c)
		_, _ = c.Write(statusResponse(`{"version":{"name":"Paper 26.2","protocol":776},"players":{"max":100,"online":0}}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := Ping(ctx, host, port)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if status.Version.Name != "Paper 26.2" {
		t.Errorf("version name is %q, want %q", status.Version.Name, "Paper 26.2")
	}
	if status.Version.Protocol != 776 {
		t.Errorf("protocol is %d, want 776", status.Version.Protocol)
	}
}

func TestPingVersionAnnouncesTheVersionItWasGiven(t *testing.T) {
	// A proxy answers a status request with the version it was asked about
	// when it supports it, so the caller that has to log in afterwards
	// (internal/mcjoin) depends on this field being exactly what it passed.
	announced := make(chan int32, 1)
	host, port := serve(t, func(c net.Conn) {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, err := mcproto.ReadPacket(c)
		if err != nil {
			announced <- 0
			return
		}
		protocol, err := mcproto.ReadVarInt(bytes.NewReader(payload))
		if err != nil {
			announced <- 0
			return
		}
		announced <- protocol
		drainRequest(c)
		_, _ = c.Write(statusResponse(`{"version":{"name":"fake","protocol":776}}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := PingVersion(ctx, host, port, -1); err != nil {
		t.Fatalf("PingVersion: %v", err)
	}
	if got := <-announced; got != -1 {
		t.Errorf("the handshake announced protocol %d, want -1", got)
	}
}

func TestPingAnnouncesTheDefaultVersion(t *testing.T) {
	announced := make(chan int32, 1)
	host, port := serve(t, func(c net.Conn) {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, payload, err := mcproto.ReadPacket(c)
		if err != nil {
			announced <- 0
			return
		}
		protocol, err := mcproto.ReadVarInt(bytes.NewReader(payload))
		if err != nil {
			announced <- 0
			return
		}
		announced <- protocol
		drainRequest(c)
		_, _ = c.Write(statusResponse(`{"version":{"name":"fake","protocol":776}}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := Ping(ctx, host, port); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// 771 written out rather than compared against handshakeProtocolVersion:
	// an assertion phrased in terms of the constant it is pinning passes
	// whatever that constant is changed to, and this test's whole purpose is
	// that Ping's own version does not move. The value is the one measured in
	// milestone 2b — a Paper 26.2 server answers it with 776.
	if got := <-announced; got != 771 {
		t.Errorf("the handshake announced protocol %d, want 771", got)
	}
}

func TestPingRejects(t *testing.T) {
	tests := []struct {
		name    string
		respond func(net.Conn)
		wantErr string
	}{
		{
			name:    "closes without answering",
			respond: func(c net.Conn) { drainRequest(c) },
			wantErr: "read packet length",
		},
		{
			name: "body shorter than the announced length",
			respond: func(c net.Conn) {
				drainRequest(c)
				var frame []byte
				frame = mcproto.AppendVarInt(frame, 100)
				_, _ = c.Write(append(frame, 0x00, 0x01))
			},
			wantErr: "read packet body",
		},
		{
			name: "wrong packet id",
			respond: func(c net.Conn) {
				drainRequest(c)
				var body []byte
				body = mcproto.AppendVarInt(body, 0x01)
				body = mcproto.AppendString(body, `{"version":{}}`)
				var frame []byte
				frame = mcproto.AppendVarInt(frame, int32(len(body)))
				_, _ = c.Write(append(frame, body...))
			},
			wantErr: "unexpected packet id",
		},
		{
			name:    "document is not an object",
			respond: func(c net.Conn) { drainRequest(c); _, _ = c.Write(statusResponse(`"not an object"`)) },
			wantErr: "not a JSON object",
		},
		{
			name:    "document without a version",
			respond: func(c net.Conn) { drainRequest(c); _, _ = c.Write(statusResponse(`{"players":{"max":1}}`)) },
			wantErr: "no version field",
		},
		{
			name: "garbage instead of a packet",
			respond: func(c net.Conn) {
				drainRequest(c)
				_, _ = c.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
			},
			wantErr: "varint longer than five bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port := serve(t, tt.respond)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			status, err := Ping(ctx, host, port)
			if err == nil {
				t.Fatalf("Ping succeeded with %+v, want an error", status)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error is %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestPingHonoursTheDeadline(t *testing.T) {
	// A server that accepts and then says nothing is the case the tool exists
	// for: the port is open long before the world is loaded.
	host, port := serve(t, func(c net.Conn) {
		drainRequest(c)
		_, _ = io.Copy(io.Discard, c)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := Ping(ctx, host, port); err == nil {
		t.Fatal("Ping succeeded against a silent server, want a deadline error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Ping took %v, want it bounded by the context deadline", elapsed)
	}
}

func TestPingReportsAClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := Ping(ctx, "127.0.0.1", port); err == nil {
		t.Fatal("Ping succeeded against a closed port, want an error")
	}
}

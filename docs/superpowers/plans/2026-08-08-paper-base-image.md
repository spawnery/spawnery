# Paper base image (milestone 2b) — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `Server` pod starts a real Paper process, its readiness probe turns green off a real server list ping, and the `Server` stops in phase `Starting` because no agent reports readiness yet.

**Architecture:** Two Go additions (`internal/slp`, `cmd/spawnery-slp`), one POSIX shell entrypoint, and three Nix files that pin Paper, pre-patch it at build time and pack the result into an OCI image with `dockerTools.buildLayeredImage`. Nothing in `internal/controller`, `internal/podspec` or the CRDs changes — the podspec already prescribes every path this image has to provide.

**Tech stack:** Go 1.26.5 (standard library only for the new packages), Nix flakes, `dockerTools`, Paper 26.2 build 111, `jdk25_headless`, Docker or Podman.

**Design:** `docs/superpowers/specs/2026-08-08-paper-base-image-design.md`. Section numbers below refer to it.

## Global constraints

- Every new Go file starts with the Apache-2.0 header from `hack/boilerplate.go.txt`.
- **Everything is written in English** — code, comments, documentation, commit messages. This is a change from milestone 2a; commit `5b1d97b` translated the repository. Commit messages are plain declarative sentences with no `feat:`-style prefix, matching the existing history.
- Every commit ends with a blank line and then:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  ```
- No new test libraries. The suite uses only `testing` from the standard library, table-driven, with `t.Errorf` messages that name the actual value.
- All commands run in the dev shell: `nix develop -c <command>` (allow up to 600000 ms).
- `nix develop -c make test` is green before every commit.
- **The exact strings the podspec already fixes, and which nothing here may deviate from:** the binary is `/usr/local/bin/spawnery-slp`, invoked as `spawnery-slp --host 127.0.0.1 --port 25565`; the port is `25565`; the working directory is `/data`; scratch is `/tmp`; the container's env carries `SPAWNERY_NETWORK`, `SPAWNERY_GROUP`, `SPAWNERY_SERVER`, `SPAWNERY_MAX_PLAYERS`, `SPAWNERY_OPERATOR_ENDPOINT`. They live in `internal/podspec/server.go` as constants; read them there rather than retyping them.
- Paper version `26.2`, build `111`, image version `0.1.0`, image name `ghcr.io/spawnery/paper`, tag `26.2-0.1.0`.
- The two pinned hashes, verified by download:
  - `paper-26.2-111.jar` → `sha256-PsgePqUMxgkLlKqwJEkYRqICcC6Kh0MIpddRD2s6oBI=`
  - Mojang `server.jar` → `sha256-zazfsliY3l5LSw5d3MJyL3cGfkZgVwnC2IbAAOu2PsU=`
- The Go module's `vendorHash` is `sha256-93cgbNfJURfz1mOM0nnOp9WGuMcFqkKlFGJ4tmdXeiw=`. It depends only on `go.mod` and `go.sum`, neither of which this plan changes; if a build says otherwise, take the hash from the error rather than guessing.
- Container uid/gid is `10001:10001`.
- **Tasks 4 through 7 only work on `x86_64-linux`** and need Docker or Podman. On `aarch64-darwin` they fail for want of a Linux builder; that is the documented gate from design section 3, not a defect. Tasks 1 through 3 run everywhere.
- `CONTAINER ?= docker` in the Makefile, so a Podman user can pass `CONTAINER=podman`. On this machine `docker` is a Podman alias, which means image names must be fully qualified (`docker.io/library/alpine:3`, not `alpine:3`) wherever a foreign image is referenced.

## File structure

| File | Responsibility |
|---|---|
| `internal/slp/slp.go` | The server list ping protocol. VarInt framing, handshake, status request, response parsing. No flags, no process, no logging. |
| `internal/slp/slp_test.go` | The protocol against a fake server in Go, acceptance and every rejection. |
| `cmd/spawnery-slp/main.go` | Flags, deadline, exit code. Nothing else. |
| `cmd/spawnery-slp/main_test.go` | Exit codes and flag defaults. |
| `image/entrypoint.sh` | EULA, the three enforced `server.properties` fields, `exec java`. |
| `image/entrypoint_test.go` | Drives the script against a stub `java`, in `make test`. |
| `nix/paper.nix` | The two pinned fetches and the pre-patch derivation. |
| `nix/paper-image.nix` | `buildLayeredImage`: layers, paths, image config. |
| `hack/image-test.sh` | The offline smoke test against a real container runtime. |
| `flake.nix` | New outputs `packages.spawnery-slp`, `packages.paper`, `packages.paper-image`. |
| `Makefile` | `image`, `image-load`, `image-test`. |

---

### Task 1: The server list ping protocol

Design section 7. A `tcpSocket` probe on 25565 goes green while the world is still loading; the status path answers only afterwards. This package speaks that path and nothing more.

**Files:**
- Create: `internal/slp/slp.go`
- Test: `internal/slp/slp_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `slp.Ping(ctx context.Context, host string, port int) (*slp.Status, error)`, where `Status` has the field `Version slp.Version` and `Version` has `Name string` and `Protocol int`. Task 2 is the only consumer.

- [ ] **Step 1: Write the failing test**

Create `internal/slp/slp_test.go` (Apache header first, then):

```go
package slp

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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
		defer conn.Close()
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
	body = appendVarInt(body, 0x00)
	body = appendString(body, doc)

	var frame []byte
	frame = appendVarInt(frame, int32(len(body)))
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
				frame = appendVarInt(frame, 100)
				_, _ = c.Write(append(frame, 0x00, 0x01))
			},
			wantErr: "read packet body",
		},
		{
			name: "wrong packet id",
			respond: func(c net.Conn) {
				drainRequest(c)
				var body []byte
				body = appendVarInt(body, 0x01)
				body = appendString(body, `{"version":{}}`)
				var frame []byte
				frame = appendVarInt(frame, int32(len(body)))
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

func TestVarIntRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, 127, 128, 255, 25565, 2097151, 2147483647} {
		encoded := appendVarInt(nil, v)
		decoded, err := readVarInt(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("readVarInt(%d): %v", v, err)
		}
		if decoded != v {
			t.Errorf("round trip of %d gave %d", v, decoded)
		}
	}
}
```

The import block at the top of the test file is therefore:

```go
import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `nix develop -c go test ./internal/slp/ -v`
Expected: FAIL — the package does not exist, so `go test` reports `no required module provides package github.com/spawnery/spawnery/internal/slp`. After creating the empty directory it becomes `undefined: Ping`, `undefined: appendVarInt`, `undefined: appendString`, `undefined: readVarInt`.

- [ ] **Step 3: Write the implementation**

Create `internal/slp/slp.go` (Apache header first, then):

```go
// Package slp speaks the Minecraft server list ping, the status half of the
// protocol. It exists because the readiness probe has to answer a question a
// port check cannot: Paper listens on 25565 well before the world is loaded,
// and only the status path answers once it is.
package slp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// handshakeProtocolVersion is the protocol version announced in the handshake.
// For a status request it does not have to match the server: a Paper 26.2
// server (protocol 776) answers a handshake announcing 771. This tool
// therefore never has to track Minecraft versions, which is measured, not
// hoped for — see section 7 of the design.
const handshakeProtocolVersion = 771

// maxPacketLen bounds what we are willing to allocate for one response. A
// status document is a few hundred bytes; anything near this is a broken or
// hostile peer, and reading it would be the only unbounded allocation here.
const maxPacketLen = 2 << 20

// Version is the part of the status document that proves a server answered
// rather than merely accepted.
type Version struct {
	Name     string `json:"name"`
	Protocol int    `json:"protocol"`
}

// Status is what Ping returns. It carries deliberately little: player counts
// are the agent's business from milestone 2c on, and a probe that interpreted
// them would be a second truth about the same thing.
type Status struct {
	Version Version `json:"version"`
}

// Ping performs one server list ping. It returns an error unless the peer
// answered with a status packet whose document is a JSON object carrying a
// version field. The deadline comes from ctx.
func Ping(ctx context.Context, host string, port int) (*Status, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set deadline: %w", err)
		}
	}

	if err := writeHandshake(conn, host, port); err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}
	// The status request itself is an empty packet 0x00.
	if err := writePacket(conn, 0x00, nil); err != nil {
		return nil, fmt.Errorf("write status request: %w", err)
	}

	return readStatusResponse(conn)
}

func writeHandshake(w io.Writer, host string, port int) error {
	var payload []byte
	payload = appendVarInt(payload, handshakeProtocolVersion)
	payload = appendString(payload, host)
	payload = binary.BigEndian.AppendUint16(payload, uint16(port))
	payload = appendVarInt(payload, 1) // next state: status
	return writePacket(w, 0x00, payload)
}

// writePacket frames one uncompressed packet. Compression is negotiated during
// login, which the status path never reaches, so the framing stays this simple.
func writePacket(w io.Writer, id int32, payload []byte) error {
	var body []byte
	body = appendVarInt(body, id)
	body = append(body, payload...)

	var frame []byte
	frame = appendVarInt(frame, int32(len(body)))
	frame = append(frame, body...)

	_, err := w.Write(frame)
	return err
}

func readStatusResponse(r io.Reader) (*Status, error) {
	byteReader := &singleByteReader{r: r}

	length, err := readVarInt(byteReader)
	if err != nil {
		return nil, fmt.Errorf("read packet length: %w", err)
	}
	if length <= 0 || length > maxPacketLen {
		return nil, fmt.Errorf("read packet length: implausible value %d", length)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read packet body: %w", err)
	}

	rest := bytes.NewReader(body)

	id, err := readVarInt(rest)
	if err != nil {
		return nil, fmt.Errorf("read packet id: %w", err)
	}
	if id != 0x00 {
		return nil, fmt.Errorf("unexpected packet id %d", id)
	}

	docLen, err := readVarInt(rest)
	if err != nil {
		return nil, fmt.Errorf("read document length: %w", err)
	}
	if docLen < 0 || int(docLen) > rest.Len() {
		return nil, fmt.Errorf("read document length: %d exceeds the packet", docLen)
	}

	doc := make([]byte, docLen)
	if _, err := io.ReadFull(rest, doc); err != nil {
		return nil, fmt.Errorf("read document: %w", err)
	}

	// Decoding into a map first is what makes a missing version an error.
	// Decoding straight into Status would silently accept "{}" as success, and
	// a probe that accepts anything makes every pod look ready.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc, &fields); err != nil {
		return nil, fmt.Errorf("status document is not a JSON object: %w", err)
	}
	if _, ok := fields["version"]; !ok {
		return nil, errors.New("status document has no version field")
	}

	var status Status
	if err := json.Unmarshal(doc, &status); err != nil {
		return nil, fmt.Errorf("decode status document: %w", err)
	}
	return &status, nil
}

func appendVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			return append(b, byte(u))
		}
		b = append(b, byte(u&0x7F)|0x80)
		u >>= 7
	}
}

func appendString(b []byte, s string) []byte {
	b = appendVarInt(b, int32(len(s)))
	return append(b, s...)
}

func readVarInt(r io.ByteReader) (int32, error) {
	var value uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(value), nil
		}
	}
	return 0, errors.New("varint longer than five bytes")
}

// singleByteReader adapts a plain io.Reader to io.ByteReader without buffering.
// Buffering would be wrong here: the length prefix is followed by exactly that
// many bytes, and a bufio.Reader could swallow part of them into its buffer
// where io.ReadFull on the underlying connection would not find them.
type singleByteReader struct {
	r   io.Reader
	buf [1]byte
}

func (s *singleByteReader) ReadByte() (byte, error) {
	if _, err := io.ReadFull(s.r, s.buf[:]); err != nil {
		return 0, err
	}
	return s.buf[0], nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/slp/ -v`
Expected: PASS — `TestPingReadsTheStatusDocument`, all six subtests of `TestPingRejects`, `TestPingHonoursTheDeadline`, `TestPingReportsAClosedPort`, `TestVarIntRoundTrip`.

- [ ] **Step 5: Run the whole suite and commit**

Run: `nix develop -c make test`
Expected: PASS, every package as before plus `ok github.com/spawnery/spawnery/internal/slp`.

```bash
git add internal/slp
git commit -F - <<'EOF'
The readiness probe can ask whether the world is loaded

Paper listens on 25565 before it has loaded anything, so a port check turns
green on a server no player can use. internal/slp speaks the status half of
the protocol instead, which answers only afterwards.

The tests are mostly rejections, and that is the point: a tool that always
succeeds makes every readiness probe green, and the mistake would first show
up as a player landing on a half-loaded server.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 2: The spawnery-slp binary

Design section 7. `internal/podspec/server.go` invokes this hard-wired as `spawnery-slp --host 127.0.0.1 --port 25565`, so every further flag needs a default, and the tool's own deadline has to sit below the probe's `TimeoutSeconds: 5`.

**Files:**
- Create: `cmd/spawnery-slp/main.go`
- Test: `cmd/spawnery-slp/main_test.go`
- Read (do not modify): `internal/podspec/server.go:194-208` — the probe definition this has to match.

**Interfaces:**
- Consumes: `slp.Ping(ctx, host, port) (*slp.Status, error)` from task 1.
- Produces: the binary `spawnery-slp`, built by task 5 into `/usr/local/bin/spawnery-slp`. Internally `run(args []string, stderr io.Writer) int` so the exit code is testable without spawning a process.

- [ ] **Step 1: Write the failing test**

Create `cmd/spawnery-slp/main_test.go` (Apache header first, then):

```go
package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// respondOnce serves exactly one status response. All lengths here are below
// 128, so every varint is a single byte and the frame can be built by hand —
// which keeps this test independent of internal/slp's unexported helpers.
func respondOnce(t *testing.T) int {
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
		defer conn.Close()

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 256)
		_, _ = conn.Read(buf)

		doc := `{"version":{"name":"Paper 26.2","protocol":776}}`
		body := append([]byte{0x00, byte(len(doc))}, doc...)
		frame := append([]byte{byte(len(body))}, body...)
		_, _ = conn.Write(frame)
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

func TestRunSucceedsWhenTheServerAnswers(t *testing.T) {
	port := respondOnce(t)

	var stderr bytes.Buffer
	code := run([]string{"--host", "127.0.0.1", "--port", strconv.Itoa(port)}, &stderr)

	if code != 0 {
		t.Errorf("exit code is %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunFailsWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var stderr bytes.Buffer
	code := run([]string{"--host", "127.0.0.1", "--port", strconv.Itoa(port), "--timeout", "1s"}, &stderr)

	if code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "spawnery-slp:") {
		t.Errorf("stderr is %q, want it to name the tool and the reason", stderr.String())
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--nonsense"}, &stderr)

	if code != 2 {
		t.Errorf("exit code is %d, want 2 for a usage error", code)
	}
}

// The probe in internal/podspec passes only --host and --port and gives the
// tool five seconds. Anything the probe does not pass has to have a usable
// default, and the tool's own deadline has to fire first so it exits with a
// message instead of being killed by the kubelet.
func TestDefaultsMatchTheReadinessProbe(t *testing.T) {
	if defaultTimeout >= 5*time.Second {
		t.Errorf("default timeout is %v, want it below the probe's 5s", defaultTimeout)
	}
	if defaultHost != "127.0.0.1" {
		t.Errorf("default host is %q, want %q", defaultHost, "127.0.0.1")
	}
	if defaultPort != 25565 {
		t.Errorf("default port is %d, want 25565", defaultPort)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `nix develop -c go test ./cmd/spawnery-slp/ -v`
Expected: FAIL — `undefined: run`, `undefined: defaultTimeout`, `undefined: defaultHost`, `undefined: defaultPort`.

- [ ] **Step 3: Write the implementation**

Create `cmd/spawnery-slp/main.go` (Apache header first, then):

```go
// Command spawnery-slp performs one Minecraft server list ping and turns the
// result into an exit code. It is the exec readiness probe of the Paper base
// image; internal/podspec invokes it as
//
//	spawnery-slp --host 127.0.0.1 --port 25565
//
// and passes nothing else, so every other flag needs a working default.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spawnery/spawnery/internal/slp"
)

const (
	defaultHost = "127.0.0.1"
	defaultPort = 25565

	// defaultTimeout sits below the probe's TimeoutSeconds of 5 on purpose. If
	// ours fired later, the kubelet would kill the process and the log would
	// say nothing about why the server did not answer.
	defaultTimeout = 4 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run returns the process exit code: 0 when the server answered, 1 when it did
// not, 2 on a usage error.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("spawnery-slp", flag.ContinueOnError)
	fs.SetOutput(stderr)

	host := fs.String("host", defaultHost, "host to ping")
	port := fs.Int("port", defaultPort, "TCP port to ping")
	timeout := fs.Duration("timeout", defaultTimeout, "deadline for the whole ping")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if _, err := slp.Ping(ctx, *host, *port); err != nil {
		fmt.Fprintf(stderr, "spawnery-slp: %v\n", err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./cmd/spawnery-slp/ -v`
Expected: PASS — all four tests.

- [ ] **Step 5: Run the whole suite and commit**

Run: `nix develop -c make test`
Expected: PASS.

```bash
git add cmd/spawnery-slp
git commit -F - <<'EOF'
The probe gets the binary it has been calling all along

internal/podspec has named /usr/local/bin/spawnery-slp since milestone 2a and
invoked it with a fixed argument list. This is that binary: two flags with the
defaults the probe relies on, a deadline that fires before the kubelet's, and
an exit code.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 3: The entrypoint

Design section 6. Paper does not start without an accepted EULA, and three fields in `server.properties` have to hold even against a user mount. Everything else stays Paper's default.

The test drives the real script against a stub `java`, so the enforcement rules are provable in `make test` without a container.

**Files:**
- Create: `image/entrypoint.sh`
- Test: `image/entrypoint_test.go`
- Read (do not modify): `internal/testenv/testenv.go:71` — `RepoPath(t, rel)` walks up to `go.mod` and is how the test finds the script.

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `image/entrypoint.sh`, installed by task 5 as `/usr/local/bin/spawnery-entrypoint`. It reads `SPAWNERY_MAX_PLAYERS` from the environment and `PAPER_HOME` (default `/opt/paper`), operates on the current working directory, and ends in `exec java`.

- [ ] **Step 1: Write the failing test**

Create `image/entrypoint_test.go` (Apache header first, then):

```go
// Package image holds tests for the shell parts of the Paper base image. There
// is no Go code here to build — the entrypoint is a shell script, and this is
// how its rules stay provable in make test rather than only in a container.
package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// stubJava puts a fake java on PATH that prints its arguments instead of
// starting a JVM. The entrypoint ends in exec, so whatever it prints is the
// command line the image would really run.
func stubJava(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'JAVA_ARGV: %s\\n' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(dir, "java"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub java: %v", err)
	}
	return dir
}

// runEntrypoint runs the script in workDir and returns its combined output.
func runEntrypoint(t *testing.T, workDir string, env ...string) (string, error) {
	t.Helper()
	script := testenv.RepoPath(t, "image/entrypoint.sh")

	cmd := exec.Command("sh", script)
	cmd.Dir = workDir
	cmd.Env = append([]string{
		"PATH=" + stubJava(t) + ":" + os.Getenv("PATH"),
		"PAPER_HOME=/opt/paper",
	}, env...)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestEntrypointAcceptsTheEula(t *testing.T) {
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, "SPAWNERY_MAX_PLAYERS=100"); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	eula, err := os.ReadFile(filepath.Join(dir, "eula.txt"))
	if err != nil {
		t.Fatalf("read eula.txt: %v", err)
	}
	if strings.TrimSpace(string(eula)) != "eula=true" {
		t.Errorf("eula.txt is %q, want %q", string(eula), "eula=true")
	}
}

func TestEntrypointEnforcesTheOperationalFieldsAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()

	// What a user mount might have put there: two settings of their own, and
	// two the operator has to be able to rely on, set wrongly.
	existing := "motd=Hello\nview-distance=6\nmax-players=20\nenable-status=false\n"
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write server.properties: %v", err)
	}

	if _, err := runEntrypoint(t, dir, "SPAWNERY_MAX_PLAYERS=100"); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "server.properties"))
	if err != nil {
		t.Fatalf("read server.properties: %v", err)
	}
	props := parseProperties(string(raw))

	enforced := map[string]string{
		"server-port":   "25565",
		"max-players":   "100",
		"enable-status": "true",
	}
	for key, want := range enforced {
		if got := props[key]; got != want {
			t.Errorf("%s is %q, want %q — the operator relies on this one", key, got, want)
		}
	}

	kept := map[string]string{"motd": "Hello", "view-distance": "6"}
	for key, want := range kept {
		if got := props[key]; got != want {
			t.Errorf("%s is %q, want %q — user settings must survive", key, got, want)
		}
	}

	// No key may appear twice; Paper would take the last one and the file
	// would drift further apart on every restart.
	seen := map[string]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		if key, _, ok := strings.Cut(line, "="); ok {
			seen[key]++
		}
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%s appears %d times, want once", key, count)
		}
	}
}

func TestEntrypointExecsJavaWithTheBundlerRepo(t *testing.T) {
	dir := t.TempDir()

	out, err := runEntrypoint(t, dir, "SPAWNERY_MAX_PLAYERS=100")
	if err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	for _, want := range []string{
		"JAVA_ARGV:",
		"-DbundlerRepoDir=/opt/paper/repo",
		"-jar /opt/paper/paper.jar",
		"--nogui",
		"-XX:MaxRAMPercentage=75",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("java was not invoked with %q; got: %s", want, out)
		}
	}
}

func TestEntrypointRefusesAnUnusableMaxPlayers(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{name: "unset", env: nil},
		{name: "not a number", env: []string{"SPAWNERY_MAX_PLAYERS=lots"}},
		{name: "empty", env: []string{"SPAWNERY_MAX_PLAYERS="}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runEntrypoint(t, t.TempDir(), tt.env...)
			if err == nil {
				t.Fatalf("entrypoint succeeded, want a failure; output: %s", out)
			}
			if strings.Contains(out, "JAVA_ARGV:") {
				t.Errorf("java was started anyway; output: %s", out)
			}
		})
	}
}

func parseProperties(raw string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			props[key] = value
		}
	}
	return props
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `nix develop -c go test ./image/ -v`
Expected: FAIL — the directory does not exist yet, so `go test` reports it matched no packages. After creating `image/` with only the test file, it fails at `sh: image/entrypoint.sh: No such file or directory`, surfacing as a non-nil error in every test.

- [ ] **Step 3: Write the implementation**

Create `image/entrypoint.sh` and make it executable (`chmod +x image/entrypoint.sh`):

```sh
#!/bin/sh
# Entrypoint of the Spawnery Paper base image.
#
# It does the least that lets Paper start at all, plus the three fields the
# operator has to be able to rely on. Everything else stays Paper's default and
# can be overridden through a user mount under /data/config. See section 6 of
# docs/superpowers/specs/2026-08-08-paper-base-image-design.md.
set -eu

# max-players is not cosmetic: from milestone 2c the agent reports it to the
# operator as slots, and the operator scales on that number. Starting with
# Paper's default of 20 while the group says 100 would make the operator plan
# against capacity the server can never honour, so refusing is better than
# guessing.
if [ -z "${SPAWNERY_MAX_PLAYERS:-}" ]; then
	echo "spawnery-entrypoint: SPAWNERY_MAX_PLAYERS is not set" >&2
	exit 1
fi
case "$SPAWNERY_MAX_PLAYERS" in
*[!0-9]*)
	echo "spawnery-entrypoint: SPAWNERY_MAX_PLAYERS is not a number: $SPAWNERY_MAX_PLAYERS" >&2
	exit 1
	;;
esac

PAPER_HOME="${PAPER_HOME:-/opt/paper}"

# Mojang's EULA. Running this image is accepting it, and the README says so
# rather than leaving it buried here.
printf 'eula=true\n' >eula.txt

[ -f server.properties ] || : >server.properties

# Rewrite one key, keeping every other line exactly as it was found. Dropping
# the old occurrence first is what keeps the file from growing a second
# max-players on every restart.
set_property() {
	grep -v "^$1=" server.properties >server.properties.tmp || true
	printf '%s=%s\n' "$1" "$2" >>server.properties.tmp
	mv server.properties.tmp server.properties
}

# The three the operator relies on. The port is obvious. max-players is
# explained above. enable-status would be the quietest failure of the three:
# switched off, the server answers no server list ping, the readiness probe
# stays red forever, and nothing in the log says why.
set_property server-port 25565
set_property max-players "$SPAWNERY_MAX_PLAYERS"
set_property enable-status true

# exec, so the JVM becomes PID 1 and receives SIGTERM directly. With a shell in
# between, the group's termination grace period would run out empty and every
# server would lose its last world state on every stop.
#
# MaxRAMPercentage rather than a fixed -Xmx: the memory bound comes from the
# group's resources, and the image does not know it. The remaining flags are
# the ones Paper itself recommends.
exec java \
	-XX:MaxRAMPercentage=75 \
	-XX:+UseG1GC \
	-XX:+AlwaysPreTouch \
	-XX:+ParallelRefProcEnabled \
	-XX:+UnlockExperimentalVMOptions \
	-XX:+DisableExplicitGC \
	-XX:+PerfDisableSharedMem \
	-XX:MaxGCPauseMillis=200 \
	-XX:G1NewSizePercent=30 \
	-XX:G1MaxNewSizePercent=40 \
	-XX:G1HeapRegionSize=8M \
	-XX:G1ReservePercent=20 \
	-XX:G1HeapWastePercent=5 \
	-XX:G1MixedGCCountTarget=4 \
	-XX:G1MixedGCLiveThresholdPercent=90 \
	-XX:G1RSetUpdatingPauseTimePercent=5 \
	-XX:InitiatingHeapOccupancyPercent=15 \
	-DbundlerRepoDir="$PAPER_HOME/repo" \
	-jar "$PAPER_HOME/paper.jar" --nogui
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./image/ -v`
Expected: PASS — `TestEntrypointAcceptsTheEula`, `TestEntrypointEnforcesTheOperationalFieldsAndKeepsTheRest`, `TestEntrypointExecsJavaWithTheBundlerRepo`, and all three subtests of `TestEntrypointRefusesAnUnusableMaxPlayers`.

- [ ] **Step 5: Run the whole suite and commit**

Run: `nix develop -c make test`
Expected: PASS, with `ok github.com/spawnery/spawnery/image` among the packages.

```bash
git add image
git commit -F - <<'EOF'
The image insists on three fields and leaves the rest alone

Paper does not start without an accepted EULA, so the entrypoint writes one.
Beyond that it enforces only server-port, max-players and enable-status —
the three the operator relies on — and keeps every other line of
server.properties exactly as a user mount left it.

The script is driven against a stub java in make test, so the rules are
provable without a container. Without that, the first proof that a user mount
cannot silently turn off enable-status would be a readiness probe that stays
red and a log that says nothing.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 4: Pin Paper and pre-patch it

Design sections 3.2 and 5.1. The Paper jar is a paperclip bootstrap that downloads Mojang's server jar at first start. This task moves that into the build so no pod ever downloads anything, and so `/data` holds 2 MB instead of 173 MB.

**Files:**
- Create: `nix/paper.nix`
- Modify: `flake.nix` — add `packages` alongside the existing `devShells`.

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `packages.paper`, an attribute set with `paperVersion` (`"26.2"`), `paperBuild` (`"111"`), `paperJar` (the launcher, a single file), and `repo` (a directory holding `versions/`, `libraries/` and `cache/`). Task 5 consumes all four.

- [ ] **Step 1: Write nix/paper.nix**

```nix
# The pinned Paper artifacts, and the patching that has to happen at build time
# rather than in every pod.
#
# The jar PaperMC publishes is not a server: it is a paperclip bootstrap that
# downloads Mojang's server jar on first start and patches it. Leaving that to
# runtime would break the main design's promise that nothing is downloaded at
# runtime, and would extract 166 MB into every ephemeral pod's emptyDir on
# every single start.
{ fetchurl
, jdk25_headless
, stdenvNoCC
}:

rec {
  paperVersion = "26.2";
  paperBuild = "111";

  # The launcher. Its hash was computed from a download and checked in here;
  # that does not make the source trustworthy, it makes the artifact frozen —
  # a changed upstream breaks the build instead of substituting a jar quietly.
  paperJar = fetchurl {
    url = "https://fill-data.papermc.io/v1/objects/3ec81e3ea50cc6090b94aab024491846a202702e8a874308a5d7510f6b3aa012/paper-${paperVersion}-${paperBuild}.jar";
    hash = "sha256-PsgePqUMxgkLlKqwJEkYRqICcC6Kh0MIpddRD2s6oBI=";
  };

  # Mojang's server jar. This URL and this hash both come from
  # META-INF/download-context inside paperJar, which is itself pinned above.
  # The checksum therefore does not come from the host that serves the
  # artifact — which is what the main design asks for and what no other hash in
  # this project manages.
  mojangJar = fetchurl {
    url = "https://piston-data.mojang.com/v1/objects/823e2250d24b3ddac457a60c92a6a941943fcd6a/server.jar";
    hash = "sha256-zazfsliY3l5LSw5d3MJyL3cGfkZgVwnC2IbAAOu2PsU=";
  };

  # The patched server, produced offline: every input is already fetched, so
  # the sandbox needs no network.
  #
  # cache/ ships along, 61 MB that nothing reads after this build. Paperclip
  # touches the cache directory before it decides whether patching is needed at
  # all, and on a read-only path it fails there with a FileSystemException.
  # Measured, not assumed. Dropping it would mean a writable cache directory in
  # every pod, which is worse.
  repo = stdenvNoCC.mkDerivation {
    pname = "paper-repo";
    version = "${paperVersion}+${paperBuild}";

    dontUnpack = true;
    nativeBuildInputs = [ jdk25_headless ];

    buildPhase = ''
      runHook preBuild

      mkdir -p work/cache
      cp ${mojangJar} work/cache/mojang_${paperVersion}.jar
      cd work
      java -Dpaperclip.patchonly=true -DbundlerRepoDir=. -jar ${paperJar}
      cd ..

      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall

      mkdir -p $out
      cp -r work/versions work/libraries work/cache $out/
      chmod -R a-w $out

      runHook postInstall
    '';

    meta.description = "Paper ${paperVersion} build ${paperBuild}, patched at build time";
  };
}
```

- [ ] **Step 2: Wire it into flake.nix**

In `flake.nix`, inside the `outputs` attribute set and beside the existing `devShells`, add:

```nix
      packages = forAllSystems (pkgs:
        let
          paper = pkgs.callPackage ./nix/paper.nix { };
        in
        {
          paper-repo = paper.repo;
        });
```

`paper` stays a `let` binding rather than becoming `packages.paper`: the values under `packages.<system>` are expected to be derivations, and `nix/paper.nix` returns an attribute set holding several. Only the derivations are exposed by name.

- [ ] **Step 3: Build it and verify the tree**

Run: `nix build .#paper-repo --no-link --print-out-paths`
Expected: a store path. Then, with `REPO=$(nix build .#paper-repo --no-link --print-out-paths)`:

Run: `ls $REPO`
Expected: `cache  libraries  versions`

Run: `ls $REPO/versions/26.2/`
Expected: `paper-26.2.jar` — this is the patched server, the thing that must not be produced at runtime.

Run: `du -sh $REPO`
Expected: roughly 166M.

- [ ] **Step 4: Verify the build is bit-reproducible**

Run: `nix build .#paper-repo --rebuild`
Expected: no output and exit 0. `--rebuild` builds a second time and compares the results byte for byte; a mismatch prints `derivation ... may not be deterministic: output differs`.

If it does differ, the cause is almost certainly a timestamp inside `versions/26.2/paper-26.2.jar`. Do not lower the bar. Normalize instead, by adding to `installPhase` before the `chmod`, and then rerun this step:

```bash
find $out -name '*.jar' -exec touch -d @1 {} +
```

If the jar's *internal* entry timestamps differ rather than its mtime, repack it deterministically with `${strip-nondeterminism}/bin/strip-nondeterminism --type zip` and add `strip-nondeterminism` to `nativeBuildInputs`.

- [ ] **Step 5: Commit**

`make test` is unaffected by this task, but run it anyway to confirm nothing regressed.

Run: `nix develop -c make test`
Expected: PASS.

```bash
git add nix/paper.nix flake.nix
git commit -F - <<'EOF'
Paper is patched at build time, not in every pod

The jar PaperMC publishes is a paperclip bootstrap: on first start it fetches
Mojang's server jar from piston-data.mojang.com and patches it. That breaks
the promise that nothing is downloaded at runtime, and it breaks it at the
first start of every pod rather than at some edge.

Both artifacts are now pinned and the patching happens in the build. The
Mojang hash comes from META-INF/download-context inside the Paper jar, which
is itself pinned, so for once the checksum genuinely does not come from the
host serving the artifact.

Beyond the promise, this is why /data holds 2 MB instead of 173 MB: without
it every ephemeral pod extracts 166 MB into its emptyDir on every start.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 5: The image

Design sections 5.2 through 5.4. **This task requires `x86_64-linux`.**

**Files:**
- Create: `nix/paper-image.nix`
- Modify: `flake.nix` — extend `packages`
- Modify: `Makefile` — add `image` and `image-load`

**Interfaces:**
- Consumes: `packages.paper` from task 4 (`paperVersion`, `paperBuild`, `paperJar`, `repo`), `image/entrypoint.sh` from task 3, `cmd/spawnery-slp` from task 2.
- Produces: `packages.spawnery-slp` (the Go binary) and `packages.paper-image` (a gzipped OCI tarball loadable with `docker load`), tagged `ghcr.io/spawnery/paper:26.2-0.1.0`. Tasks 6 and 7 consume the image.

- [ ] **Step 1: Write nix/paper-image.nix**

```nix
# The Paper base image.
#
# Everything the podspec prescribes has to be satisfied here, because the pod
# spec is already written: /usr/local/bin/spawnery-slp, port 25565, working
# directory /data, scratch /tmp, a numeric user, and nothing else writable.
{ bash
, buildEnv
, coreutils
, dockerTools
, gnugrep
, jdk25_headless
, runCommand
, runtimeShell
, writeTextDir
, paper
, spawnery-slp
, imageVersion ? "0.1.0"
}:

let
  # runAsNonRoot refuses to start an image with no numeric user. The probe
  # measured that Java itself does not need the passwd entry as long as HOME is
  # set; it is here because a failing getpwuid inside a library on the
  # classpath surfaces as an error that says nothing about its cause.
  passwd = writeTextDir "etc/passwd" ''
    root:x:0:0:root:/root:/bin/sh
    spawnery:x:10001:10001:spawnery:/data:/bin/sh
  '';

  group = writeTextDir "etc/group" ''
    root:x:0:
    spawnery:x:10001:
  '';

  # The shebang is rewritten to a shell that exists in this image. Relying on
  # /bin/sh in a Nix-built image would work today and break the day the tool
  # set changes.
  entrypoint = runCommand "spawnery-entrypoint" { } ''
    mkdir -p $out/usr/local/bin
    substitute ${../image/entrypoint.sh} $out/usr/local/bin/spawnery-entrypoint \
      --replace-fail '#!/bin/sh' '#!${runtimeShell}'
    chmod +x $out/usr/local/bin/spawnery-entrypoint
  '';

  # Copied rather than symlinked, so the path in the image is exactly the one
  # internal/podspec names and does not depend on a store link resolving.
  slp = runCommand "spawnery-slp-image-path" { } ''
    mkdir -p $out/usr/local/bin
    cp ${spawnery-slp}/bin/spawnery-slp $out/usr/local/bin/spawnery-slp
  '';

  paperHome = runCommand "paper-home" { } ''
    mkdir -p $out/opt/paper
    cp ${paper.paperJar} $out/opt/paper/paper.jar
    cp -r ${paper.repo} $out/opt/paper/repo
    chmod -R a-w $out/opt/paper
  '';
in
dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/paper";
  tag = "${paper.paperVersion}-${imageVersion}";

  # amd64 explicitly, not the host's architecture: milestone 2b targets
  # linux/amd64 only, and an image that silently inherited aarch64 binaries
  # under an amd64 label would be worse than a build that fails.
  architecture = "amd64";

  # Ordered by rate of change. The JRE and the patched Paper repo are large and
  # almost static; our own two files are small and change per commit. Milestone
  # 2c adds the agent plugin as another small layer without touching either.
  copyToRoot = [
    (buildEnv {
      name = "paper-tools";
      # grep and mv come from coreutils and gnugrep because the entrypoint uses
      # them; bash because the entrypoint's shebang points at it.
      paths = [ bash coreutils gnugrep jdk25_headless ];
      pathsToLink = [ "/bin" ];
    })
    passwd
    group
    paperHome
    slp
    entrypoint
  ];

  # /data and /tmp are always mounted over in Kubernetes, so their mode there
  # comes from the kubelet, which creates an emptyDir world-writable. The mode
  # set here is what makes the same image usable under a plain container
  # runtime with a fresh volume — which is exactly what make image-test does.
  extraCommands = ''
    mkdir -p data tmp
    chmod 0777 data
    chmod 1777 tmp
  '';

  config = {
    User = "10001:10001";
    WorkingDir = "/data";
    Env = [
      "HOME=/data"
      "PATH=/bin:/usr/local/bin"
      "PAPER_HOME=/opt/paper"
    ];
    ExposedPorts = { "25565/tcp" = { }; };
    Entrypoint = [ "/usr/local/bin/spawnery-entrypoint" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery Paper base image";
      "org.opencontainers.image.version" = "${paper.paperVersion}-${imageVersion}";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
      # The Paper build number lives here rather than in the tag, so an
      # upstream rebuild does not force every sample manifest to be touched.
      "cloud.spawnery.paper-build" = paper.paperBuild;
    };
  };
}
```

- [ ] **Step 2: Extend flake.nix**

Replace the `packages` block from task 4 with:

```nix
      packages = forAllSystems (pkgs:
        let
          paper = pkgs.callPackage ./nix/paper.nix { };
        in
        rec {
          paper-repo = paper.repo;

          spawnery-slp = pkgs.buildGoModule {
            pname = "spawnery-slp";
            version = "0.1.0";
            src = ./.;
            vendorHash = "sha256-93cgbNfJURfz1mOM0nnOp9WGuMcFqkKlFGJ4tmdXeiw=";
            subPackages = [ "cmd/spawnery-slp" ];
            # Static, because the image carries no libc of its own for it.
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
          };

          # Only builds on x86_64-linux. On aarch64-darwin this needs a Linux
          # builder; see docs/known-issues.md.
          paper-image = pkgs.callPackage ./nix/paper-image.nix {
            inherit paper spawnery-slp;
          };
        });
```

`rec` is what lets `paper-image` refer to `spawnery-slp` by name; `paper` comes from the `let` above it.

- [ ] **Step 3: Add the Makefile targets**

At the top of the `Makefile`, beside `CONTROLLER_GEN`:

```make
CONTAINER ?= docker
IMAGE ?= ghcr.io/spawnery/paper:26.2-0.1.0
```

At the end of the `Makefile`:

```make
.PHONY: image
image:
	nix build .#paper-image

.PHONY: image-load
image-load: image
	$(CONTAINER) load < result
```

- [ ] **Step 4: Build the image and verify its contents**

Run: `nix build .#paper-image && ls -lh result`
Expected: a gzipped tarball, on the order of 400–500 MB.

Run: `make image-load`
Expected: `Loaded image: ghcr.io/spawnery/paper:26.2-0.1.0` (Docker) or the Podman equivalent.

Verify the paths the podspec depends on actually exist, by overriding the entrypoint so the check does not start a server:

```bash
docker run --rm --entrypoint /bin/sh ghcr.io/spawnery/paper:26.2-0.1.0 -c \
  'ls -l /usr/local/bin/spawnery-slp /usr/local/bin/spawnery-entrypoint /opt/paper/paper.jar && ls /opt/paper/repo && id && echo $HOME && pwd'
```

Expected: both binaries present and executable, `paper.jar` present, `/opt/paper/repo` listing `cache libraries versions`, `uid=10001 gid=10001`, `HOME` `/data`, working directory `/data`.

Run: `docker run --rm --entrypoint /usr/local/bin/spawnery-slp ghcr.io/spawnery/paper:26.2-0.1.0 --host 127.0.0.1 --port 1`
Expected: exit 1 and a line on stderr starting `spawnery-slp:` — the binary runs in the image, and fails the way it should when nothing answers.

If `copyToRoot` needs adjusting — a missing `/bin/grep`, a `/opt/paper` that is a dangling symlink — fix it here and repeat this step. The listing above is the check that catches it.

- [ ] **Step 5: Verify reproducibility**

Run: `nix build .#paper-image --rebuild`
Expected: no output and exit 0.

This is the criterion the whole Nix decision rests on. If it fails, the message names the differing output; fix the cause rather than dropping the check. `dockerTools.buildLayeredImage` already pins the image timestamp to `1970-01-01T00:00:01Z`, so a difference here comes from an input, most likely the jar covered in task 4 step 4.

- [ ] **Step 6: Commit**

Run: `nix develop -c make test`
Expected: PASS.

```bash
git add nix/paper-image.nix flake.nix Makefile
git commit -F - <<'EOF'
There is an image to pull

A JRE 25, the pre-patched Paper repo, the entrypoint and the SLP binary,
layered by rate of change and configured to satisfy what internal/podspec has
been prescribing since milestone 1: /usr/local/bin/spawnery-slp, port 25565,
/data as the working directory, uid 10001, and nothing else writable.

Reproducibility is checked rather than claimed: nix build --rebuild builds a
second time and compares byte for byte. That check is the reason the image is
built with Nix at all, so it is a gate, not a nicety.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 6: The offline smoke test

Design section 9, level B. **This task requires `x86_64-linux` and a container runtime.**

The test runs the image under the podspec's constraints rather than more comfortable ones, and offline — so passing it is what keeps the no-runtime-downloads promise guarded rather than remembered.

**Files:**
- Create: `hack/image-test.sh`
- Modify: `Makefile` — add `image-test`

**Interfaces:**
- Consumes: `packages.paper-image` from task 5, and `/usr/local/bin/spawnery-slp` inside it from task 2.
- Produces: `make image-test`, exit 0 on success.

- [ ] **Step 1: Write hack/image-test.sh**

Create the file and make it executable (`chmod +x hack/image-test.sh`):

```bash
#!/usr/bin/env bash
# Smoke test for the Paper base image.
#
# It runs the image under exactly the constraints internal/podspec imposes,
# rather than more comfortable ones, and with no network at all. The offline
# part is the point: the day somebody unpins the pre-patched repo, this test
# fails instead of a pod quietly downloading from Mojang in production.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
DEADLINE="${DEADLINE:-180}"

NAME="spawnery-image-test-$$"
VOLUME="spawnery-image-test-$$"

cleanup() {
	"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A named volume rather than a host directory: the container writes as uid
# 10001, and cleaning those files up from the host afterwards is a fight that
# has nothing to do with what is being tested.
"$CONTAINER" volume create "$VOLUME" >/dev/null

"$CONTAINER" run -d --name "$NAME" \
	--network none \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--user 10001:10001 \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME:/data" \
	-e SPAWNERY_MAX_PLAYERS=100 \
	"$IMAGE" >/dev/null

echo "waiting up to ${DEADLINE}s for a server list ping..."
start=$SECONDS
until "$CONTAINER" exec "$NAME" /usr/local/bin/spawnery-slp --host 127.0.0.1 --port 25565 >/dev/null 2>&1; do
	if [ -z "$("$CONTAINER" ps -q --filter "name=^${NAME}$")" ]; then
		echo "the container exited before answering:" >&2
		"$CONTAINER" logs "$NAME" >&2
		exit 1
	fi
	if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
		echo "no server list ping within ${DEADLINE}s:" >&2
		"$CONTAINER" logs "$NAME" >&2
		exit 1
	fi
	sleep 2
done
echo "the server answered after $((SECONDS - start))s"

# Nothing may have reached for the network. With --network none a download
# attempt cannot succeed, but it can still be attempted, and an attempt means
# the pre-patching stopped working.
if "$CONTAINER" logs "$NAME" 2>&1 | grep -qiE 'downloading|UnknownHostException|piston-data'; then
	echo "the image tried to download something at runtime:" >&2
	"$CONTAINER" logs "$NAME" >&2
	exit 1
fi
echo "no download attempted"

# SIGTERM reaches PID 1 and saves the world. Without exec in the entrypoint the
# grace period would run out empty and every stop would lose world state.
"$CONTAINER" stop -t 60 "$NAME" >/dev/null
if ! "$CONTAINER" logs "$NAME" 2>&1 | grep -q 'All dimensions are saved'; then
	echo "SIGTERM did not produce a clean shutdown:" >&2
	"$CONTAINER" logs "$NAME" 2>&1 | tail -30 >&2
	exit 1
fi
echo "clean shutdown on SIGTERM"

echo "image-test: ok"
```

- [ ] **Step 2: Add the Makefile target**

```make
.PHONY: image-test
image-test: image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) hack/image-test.sh
```

- [ ] **Step 3: Run it and verify it passes**

Run: `make image-test`
Expected, in order:

```
waiting up to 180s for a server list ping...
the server answered after <N>s
no download attempted
clean shutdown on SIGTERM
image-test: ok
```

`<N>` should be under 30 seconds on a cold world.

- [ ] **Step 4: Verify the test can fail**

A green run proves nothing on its own. Break the offline promise on purpose and confirm the test notices:

```bash
docker volume create smoke-negative
docker run --rm --network none --read-only --tmpfs /tmp:rw,exec,size=256m \
  --user 10001:10001 -v smoke-negative:/data -e SPAWNERY_MAX_PLAYERS=100 \
  -e PAPER_HOME=/nonexistent ghcr.io/spawnery/paper:26.2-0.1.0 ; echo "exit=$?"
docker volume rm -f smoke-negative
```

Expected: a non-zero exit and an error naming the missing jar — which is the same path the smoke test's "container exited before answering" branch reports. If this run succeeds, the entrypoint is not using `PAPER_HOME` and the test's guarantees are weaker than they look.

- [ ] **Step 5: Commit**

```bash
git add hack/image-test.sh Makefile
git commit -F - <<'EOF'
The image is tested where make test cannot look

make test never sees a container, so the four things only a real runtime can
answer went unchecked: whether the JVM starts as uid 10001 on a read-only
root filesystem, whether Paper loads a world, whether it answers a server
list ping, and whether our own binary works in the environment it actually
runs in.

The test runs with --network none, which makes it the standing guard on the
promise that nothing is downloaded at runtime, and it stops the container to
confirm SIGTERM still reaches PID 1 and saves the world.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 7: The milestone's evidence

Design section 9, level C, and section 10. **This task requires `x86_64-linux`, a container runtime and k3d.**

This is where the milestone's headline claim is produced: a pod that runs, a probe that goes green, and a `Server` that stops in `Starting`. It is also the first time the k3d flow in the README is executed at all.

**Files:**
- Modify: `config/samples/network.yaml:46` — the image reference
- Modify: `README.md` — the k3d section and the status paragraph
- Modify: `docs/known-issues.md` — a section for what 2b leaves open

**Interfaces:**
- Consumes: the image from task 5.
- Produces: nothing further tasks depend on; this is the last task.

- [ ] **Step 1: Point the sample at the image that now exists**

In `config/samples/network.yaml`, change the ServerGroup's image:

```yaml
  image: ghcr.io/spawnery/paper:26.2-0.1.0
```

The tag is not `latest`, so Kubernetes' default pull policy is `IfNotPresent` and an imported image is used without reaching for a registry. That is what makes the next step work without publishing anything.

- [ ] **Step 2: Run the whole path against k3d**

```bash
nix develop -c k3d cluster create spawnery-dev --agents 1
nix develop -c k3d image import ghcr.io/spawnery/paper:26.2-0.1.0 -c spawnery-dev
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false --operator-namespace minecraft &
sleep 60
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

Expected:

- `network production` with `Accepted=True`
- `servergroup lobby` with `REPLICAS 1`
- a pod `lobby-xxxx` in `Running` with `READY 1/1`
- a `server lobby-xxxx` in phase **`Starting`**, and staying there

The last line is the milestone's result. Pod readiness is now real — a Paper process answered `spawnery-slp` — and the `Server` still does not reach `Ready`, because the second half of the gate in `internal/controller/server_controller.go:333-340` asks for an agent that does not exist until 2c.

Confirm the probe is genuinely green rather than merely not failing:

```bash
nix develop -c kubectl get pod -n minecraft -l spawnery.cloud/role=server \
  -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}{"\n"}'
```

Expected: `True`.

And confirm no agent connected, so the phase is explained by what is missing and not by something broken:

```bash
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/role=server --tail=5
```

Expected: Paper's own startup log ending in `Done (…)!`, with nothing about a gRPC connection.

Then clean up:

```bash
kill %1
nix develop -c k3d cluster delete spawnery-dev
```

- [ ] **Step 3: Update the README**

In the status paragraph, replace the sentence about milestone 2b being missing with what is now true. Add after the milestone 2a paragraph:

```markdown
Milestone 2b is done: the Paper base image. `nix build .#paper-image` produces
a reproducible image holding Paper 26.2, a JRE 25 and `spawnery-slp`, the tool
the readiness probe calls to speak a real server list ping. Paper is patched at
build time, so a pod downloads nothing at startup; `make image-test` runs the
image offline to keep that true.

A pod now starts and its readiness probe turns green — and the `Server` stops
in phase `Starting`, because the second half of the ready gate wants an agent.
That agent is the Kotlin plugin from milestone 2c, and until it exists no
player can join: the Velocity proxy layer (milestone 3) is missing too.
```

In the "Trying it locally against k3d" section, delete the paragraph explaining that the flow has never been run, and replace the expected outcome list with:

```markdown
Expected:

- `network production` with `Accepted=True`,
- `servergroup lobby` with `REPLICAS 1`,
- a pod `lobby-xxxx` in `Running` with `READY 1/1` — the readiness probe spoke
  a real server list ping to a real Paper process,
- a `server lobby-xxxx` in phase `Starting`, staying there. That is the
  expected end state after milestone 2b: pod readiness is one half of the
  gate, and the other half waits for the agent from milestone 2c.
```

Insert the image import into that section's command block, after the cluster is created:

```bash
nix develop -c k3d image import ghcr.io/spawnery/paper:26.2-0.1.0 -c spawnery-dev
```

Also add, under `## Development`, after the `make proto` paragraph:

```markdown
`make image` builds the Paper base image, `make image-load` hands it to the
local container runtime, and `make image-test` runs it offline under the same
constraints the podspec imposes. All three need Docker or Podman and only work
on `x86_64-linux`. Pass `CONTAINER=podman` if `docker` is not your runtime.

Running this image accepts
[Mojang's EULA](https://www.minecraft.net/eula) on your behalf: the entrypoint
writes `eula=true`, because Paper does not start otherwise.
```

- [ ] **Step 4: Record what 2b leaves open**

In `docs/known-issues.md`, two edits.

First, rename the existing heading

```markdown
## Preconditions for milestone 2b (base images, Kotlin agent)
```

to

```markdown
## Preconditions for milestone 2c (the Kotlin agent)
```

Its three bold obligations — reconnecting with overlap, the `Bearer ` header, and `Hello{ready:false}` — all concern the agent and stay exactly as they are. Only the base image half of that heading is settled.

Second, insert a new section immediately after that one and before "Preconditions for milestone 3":

```markdown
## From milestone 2b (the base image)

**The Darwin machine cannot build the image.** A Linux image needs a Linux
builder, so `nix build .#paper-image`, `make image-test` and the k3d flow only
work on `x86_64-linux`. `make test` still runs everywhere, including the
entrypoint and SLP tests. This is the mirror image of the envtest gate that
milestone 2a closed, and it cannot be closed with a checked-in hash.

**Without a memory limit the JVM sizes itself against the whole node.** The
entrypoint passes `-XX:MaxRAMPercentage=75`, which is a share of the container
limit — and of the node when there is no limit. `AlwaysPreTouch` then claims
that share immediately. Neither `ServerGroup` nor `Network` is required to set
`resources`, and no CEL rule demands it; the sample manifest sets 2Gi and
nothing makes anyone else do so.

**`fsGroup` is missing.** For ephemeral groups this does not bite: the kubelet
creates an `emptyDir` world-writable, so uid 10001 writes into `/data` fine. A
PVC in milestone 5 arrives owned by root, and uid 10001 does not. The fix
belongs in `podspec.BuildServerPod`'s `PodSecurityContext` and has to land
before the first persistent server exists.

**Following Paper upstream is manual.** A new build means new hashes in
`nix/paper.nix`, by hand, including the Mojang hash out of the new jar's
`META-INF/download-context`. The automated image pipeline is project 3 in the
main design.

**`cache/mojang_26.2.jar` ships unused**, 61 MB of the image. Paperclip touches
the cache directory before deciding whether it needs to patch, and fails on a
read-only path if it is absent. Removing it would require a writable cache
directory in every pod, which is the worse trade.

**No image is published.** The tag `ghcr.io/spawnery/paper:26.2-0.1.0` is
correct but nothing pushes it, so every consumer needs `k3d image import` or
the equivalent. Publishing belongs with CI in milestone 6.
```

- [ ] **Step 5: Verify and commit**

Run: `nix develop -c make test`
Expected: PASS.

Run: `nix develop -c kubectl apply --dry-run=client -f config/samples/network.yaml`
Expected: all three objects report `(dry run)` without error — the sample still validates after the image change.

```bash
git add config/samples/network.yaml README.md docs/known-issues.md
git commit -F - <<'EOF'
A pod starts, and stops exactly where it should

The sample points at an image that exists now, and the k3d flow in the README
has been run for the first time since it was written: the pod reaches Running
with a green readiness probe, because a real Paper process answered a real
server list ping.

The Server stays in Starting, and that is the result rather than a shortfall.
Milestone 2a proved an agent without pod readiness is not enough; this is the
same gate seen from the other side. Ready waits for the Kotlin agent.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

## What this plan does not do

- No Kotlin, no agent, no `Ready` phase. Milestone 2c.
- No Velocity image. Milestone 3.
- No configuration rendering per group, and no `config` field on `ServerGroup`. The first field that needs it is the forwarding mode, which arrives with milestone 3.
- No registry push and no CI. Milestone 6.
- No `linux/arm64` and no multi-arch manifest.

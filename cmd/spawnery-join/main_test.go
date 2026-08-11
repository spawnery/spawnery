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
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// serveOneJoin serves a status ping and then one uncompressed offline login,
// which is everything run() has to get through to print a line.
//
// Every frame here is built and parsed by hand, and every length in it is
// below 128 so each VarInt is a single byte — the same tactic
// cmd/spawnery-slp's test uses, and for the same reason: this test should fail
// when the command stops speaking the protocol, not when internal/mcjoin's
// unexported helpers are refactored.
func serveOneJoin(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				serveConn(conn)
			}()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

// readFrame reads one length-prefixed frame and returns the packet id and the
// rest of it.
func readFrame(r io.Reader) (byte, []byte, error) {
	var header [1]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	body := make([]byte, header[0])
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return body[0], body[1:], nil
}

func writeFrame(w io.Writer, id byte, payload []byte) error {
	body := append([]byte{id}, payload...)
	_, err := w.Write(append([]byte{byte(len(body))}, body...))
	return err
}

func serveConn(conn net.Conn) {
	_, handshake, err := readFrame(conn)
	if err != nil {
		return
	}
	// The handshake's last byte is its next-state field: 1 asks for a status
	// document, 2 begins a login.
	if handshake[len(handshake)-1] == 1 {
		if _, _, err := readFrame(conn); err != nil { // the status request
			return
		}
		doc := `{"version":{"name":"fake","protocol":776}}`
		_ = writeFrame(conn, 0x00, append([]byte{byte(len(doc))}, doc...))
		return
	}

	_, loginStart, err := readFrame(conn)
	if err != nil {
		return
	}
	// Login Start is a length-prefixed username followed by sixteen raw UUID
	// bytes, both of which are echoed back in Login Success.
	nameLen := int(loginStart[0])
	name := loginStart[1 : 1+nameLen]
	uuid := loginStart[1+nameLen : 1+nameLen+16]

	success := append([]byte{}, uuid...)
	success = append(success, byte(len(name)))
	success = append(success, name...)
	success = append(success, 0x00) // no properties
	if err := writeFrame(conn, 0x02, success); err != nil {
		return
	}

	if id, _, err := readFrame(conn); err != nil || id != 0x03 {
		return // Login Acknowledged, or nothing worth answering
	}
	// A configuration-state Plugin Message, which is what a routed player
	// really receives first.
	brand := "minecraft:brand"
	_ = writeFrame(conn, 0x01, append([]byte{byte(len(brand))}, brand...))

	// Stay open, so a --hold has something to hold.
	_, _ = conn.Read(make([]byte, 1))
}

func TestRunPrintsOneJSONLineAndExitsZero(t *testing.T) {
	port := serveOneJoin(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", "127.0.0.1", "--port", strconv.Itoa(port)}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code is %d, want 0; stderr: %s", code, stderr.String())
	}
	line := stdout.String()
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("stdout is %q, want exactly one line; a runbook pipes this into jq", line)
	}

	var got struct {
		Protocol   int    `json:"protocol"`
		Username   string `json:"username"`
		UUID       string `json:"uuid"`
		Compressed bool   `json:"compressed"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, line)
	}
	if got.Protocol != 776 {
		t.Errorf(`"protocol" is %d, want the 776 the server reported`, got.Protocol)
	}
	// The default username, and its offline-mode UUID as Paper 26.2 logged it
	// during the end-to-end run.
	if got.Username != "spawnery_probe" {
		t.Errorf(`"username" is %q, want the default`, got.Username)
	}
	if got.UUID != "bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f" {
		t.Errorf(`"uuid" is %q`, got.UUID)
	}
	if got.Compressed {
		t.Error(`"compressed" is true, but this server never sent Set Compression`)
	}
}

func TestRunHoldsTheConnectionOpen(t *testing.T) {
	port := serveOneJoin(t)

	var stdout, stderr bytes.Buffer
	began := time.Now()
	code := run([]string{
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--hold", "400ms",
	}, &stdout, &stderr)
	elapsed := time.Since(began)

	if code != 0 {
		t.Fatalf("exit code is %d, want 0; stderr: %s", code, stderr.String())
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("run returned after %s, before the hold was up", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("run took %s for a 400ms hold", elapsed)
	}
}

func TestRunRejectsAHoldTheTimeoutCannotCover(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// No server: this has to be refused before anything is dialled.
	code := run([]string{"--timeout", "5s", "--hold", "10s"}, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code is %d, want 2 for a usage error", code)
	}
	if !strings.Contains(stderr.String(), "--hold") || !strings.Contains(stderr.String(), "--timeout") {
		t.Errorf("stderr is %q, want it to name both flags", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout is %q, want nothing", stdout.String())
	}
}

func TestRunFailsWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// Closed immediately, so the port is almost certainly free and refuses.
	_ = ln.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--timeout", "2s",
	}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.HasPrefix(stderr.String(), "spawnery-join: ") {
		t.Errorf("stderr is %q, want the reason prefixed with the command name", stderr.String())
	}
	// Nothing on stdout, or a runbook's jq would be handed a reason it cannot
	// parse instead of an empty stream it can test for.
	if stdout.Len() != 0 {
		t.Errorf("stdout is %q, want nothing on a failure", stdout.String())
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--nonsense"}, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code is %d, want 2 for a usage error", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout is %q, want nothing", stdout.String())
	}
}

func TestRunRefusesAUsernameMinecraftWouldNot(t *testing.T) {
	port := serveOneJoin(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--username", "spawnery-probe",
	}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "valid Minecraft username") {
		t.Errorf("stderr is %q, want it to name the rule", stderr.String())
	}
}

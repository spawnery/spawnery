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

package mcjoin

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/mcproto"
)

// The fake server below writes its frames by hand rather than through
// framedConn, and that is the point: a test that encoded with the same code it
// decodes with would pass on a framing that is internally consistent and wrong
// on the wire — which is exactly the failure mode compression introduces.
// Only mcproto's VarInt and string primitives are shared, and those have their
// own tests.

// configurationDisconnectNBT is the payload Velocity 3.5.1 actually sent when
// it could not route a player, captured from the Step 1 rig: a nameless root
// compound holding color=red and text=<the message>. Kept verbatim rather than
// rebuilt from a description, so the one test that reads an NBT reason reads
// the bytes a real proxy produces.
var configurationDisconnectNBT = []byte{
	0x0a,
	0x08, 0x00, 0x05, 'c', 'o', 'l', 'o', 'r', 0x00, 0x03, 'r', 'e', 'd',
	0x08, 0x00, 0x04, 't', 'e', 'x', 't', 0x00, 0x3c,
	'U', 'n', 'a', 'b', 'l', 'e', ' ', 't', 'o', ' ', 'c', 'o', 'n', 'n', 'e', 'c', 't',
	' ', 'y', 'o', 'u', ' ', 't', 'o', ' ', 'u', 'n', 'r', 'o', 'u', 't', 'a', 'b', 'l', 'e', '.',
	' ', 'P', 'l', 'e', 'a', 's', 'e', ' ', 't', 'r', 'y', ' ', 'a', 'g', 'a', 'i', 'n',
	' ', 'l', 'a', 't', 'e', 'r', '.',
	0x00,
}

// fake is the server half: what it should do, and what it saw.
type fake struct {
	// protocol is what the status document reports.
	protocol int
	// compressAt is the Set Compression threshold to send during login, or a
	// negative number to never send one.
	compressAt int
	// loginDisconnect, when set, is the JSON reason sent instead of Login
	// Success.
	loginDisconnect string
	// encryptionRequest answers Login Start with packet 0x01 instead.
	encryptionRequest bool
	// closeAfterLoginStart hangs up instead of answering.
	closeAfterLoginStart bool
	// stallAfterLoginStart answers nothing at all and keeps the socket open.
	stallAfterLoginStart bool
	// afterAck is sent once Login Acknowledged arrives. The default is a
	// configuration-state plugin message, standing in for the proxy
	// forwarding a backend's configuration.
	afterAckID      int32
	afterAckPayload []byte
	// closeAfterAck hangs up instead of sending afterAck.
	closeAfterAck bool

	mu              sync.Mutex
	handshake       handshakeFields
	statusHandshake handshakeFields
	loginName       string
	loginUUID       [16]byte
	gotLoginAck     bool
	serverErrors    []error
}

type handshakeFields struct {
	protocol  int32
	host      string
	port      uint16
	nextState int32
}

func (f *fake) record(err error) {
	if err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverErrors = append(f.serverErrors, err)
}

func (f *fake) errorsSeen() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.serverErrors...)
}

func (f *fake) sawLoginAck() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotLoginAck
}

func (f *fake) sawHandshake() handshakeFields {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handshake
}

func (f *fake) sawStatusHandshake() handshakeFields {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusHandshake
}

func (f *fake) sawLogin() (string, [16]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginName, f.loginUUID
}

// start listens on a loopback port and serves connections until the test ends.
// Join makes two: the status ping first, then the login.
func start(t *testing.T, f *fake) (string, int) {
	t.Helper()
	if f.protocol == 0 {
		f.protocol = 771
	}
	if f.compressAt == 0 {
		f.compressAt = -1
	}
	if f.afterAckID == 0 && f.afterAckPayload == nil {
		// Configuration-state Plugin Message carrying a brand, which is the
		// shape of the first thing a routed player really receives.
		f.afterAckID = 0x01
		f.afterAckPayload = mcproto.AppendString(nil, "minecraft:brand")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				f.record(f.serve(conn))
			}()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (f *fake) serve(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	id, payload, err := readPlainPacket(conn)
	if err != nil {
		return err
	}
	if id != 0x00 {
		return errors.New("first packet was not a handshake")
	}
	fields, err := parseHandshake(payload)
	if err != nil {
		return err
	}

	if fields.nextState == 1 {
		f.mu.Lock()
		f.statusHandshake = fields
		f.mu.Unlock()
		return f.serveStatus(conn)
	}

	f.mu.Lock()
	f.handshake = fields
	f.mu.Unlock()
	return f.serveLogin(conn)
}

func (f *fake) serveStatus(conn net.Conn) error {
	id, _, err := readPlainPacket(conn)
	if err != nil {
		return err
	}
	if id != 0x00 {
		return errors.New("expected a status request")
	}
	doc := `{"version":{"protocol":` + strconv.Itoa(f.protocol) + `,"name":"fake"},"players":{"online":0,"max":1}}`
	return writeFrame(conn, -1, 0x00, mcproto.AppendString(nil, doc))
}

func (f *fake) serveLogin(conn net.Conn) error {
	id, payload, err := readPlainPacket(conn)
	if err != nil {
		return err
	}
	if id != 0x00 {
		return errors.New("expected login start")
	}
	rest := bytes.NewReader(payload)
	name, err := readStringFrom(rest)
	if err != nil {
		return err
	}
	var uuid [16]byte
	if _, err := io.ReadFull(rest, uuid[:]); err != nil {
		return err
	}
	f.mu.Lock()
	f.loginName, f.loginUUID = name, uuid
	f.mu.Unlock()

	switch {
	case f.closeAfterLoginStart:
		return nil
	case f.stallAfterLoginStart:
		// Hold the connection open and say nothing. The read deadline set at
		// the top of serve is what eventually releases this goroutine.
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
		return nil
	case f.loginDisconnect != "":
		return writeFrame(conn, -1, 0x00, mcproto.AppendString(nil, f.loginDisconnect))
	case f.encryptionRequest:
		return writeFrame(conn, -1, 0x01, []byte{0x00})
	}

	threshold := -1
	if f.compressAt >= 0 {
		if err := writeFrame(conn, -1, 0x03, mcproto.AppendVarInt(nil, int32(f.compressAt))); err != nil {
			return err
		}
		threshold = f.compressAt
	}

	var success []byte
	success = append(success, uuid[:]...)
	success = mcproto.AppendString(success, name)
	success = mcproto.AppendVarInt(success, 0) // no properties
	if err := writeFrame(conn, threshold, 0x02, success); err != nil {
		return err
	}

	id, _, err = readFrame(conn, threshold)
	if err != nil {
		return err
	}
	if id != 0x03 {
		return errors.New("expected login acknowledged")
	}
	f.mu.Lock()
	f.gotLoginAck = true
	f.mu.Unlock()

	if f.closeAfterAck {
		return nil
	}
	return writeFrame(conn, threshold, f.afterAckID, f.afterAckPayload)
}

func parseHandshake(payload []byte) (handshakeFields, error) {
	var fields handshakeFields
	r := bytes.NewReader(payload)
	var err error
	if fields.protocol, err = mcproto.ReadVarInt(r); err != nil {
		return fields, err
	}
	if fields.host, err = readStringFrom(r); err != nil {
		return fields, err
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return fields, err
	}
	fields.port = binary.BigEndian.Uint16(port[:])
	if fields.nextState, err = mcproto.ReadVarInt(r); err != nil {
		return fields, err
	}
	return fields, nil
}

func readStringFrom(r *bytes.Reader) (string, error) {
	length, err := mcproto.ReadVarInt(r)
	if err != nil {
		return "", err
	}
	if length < 0 || int(length) > r.Len() {
		return "", errors.New("string longer than the bytes that follow it")
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func readPlainPacket(r io.Reader) (int32, []byte, error) {
	return readFrame(r, -1)
}

// writeFrame assembles one frame by hand. threshold below zero is the
// uncompressed framing; at or above it is the compressed one.
func writeFrame(w io.Writer, threshold int, id int32, payload []byte) error {
	body := mcproto.AppendVarInt(nil, id)
	body = append(body, payload...)

	inner := body
	if threshold >= 0 {
		if len(body) >= threshold {
			var deflated bytes.Buffer
			zw := zlib.NewWriter(&deflated)
			if _, err := zw.Write(body); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			inner = mcproto.AppendVarInt(nil, int32(len(body)))
			inner = append(inner, deflated.Bytes()...)
		} else {
			inner = append(mcproto.AppendVarInt(nil, 0), body...)
		}
	}

	frame := mcproto.AppendVarInt(nil, int32(len(inner)))
	frame = append(frame, inner...)
	_, err := w.Write(frame)
	return err
}

// readFrame is writeFrame's inverse, and asserts what the compressed framing
// requires: a body under the threshold must arrive with a data length of zero.
func readFrame(r io.Reader, threshold int) (int32, []byte, error) {
	length, err := mcproto.ReadVarInt(mcproto.ByteReader(r))
	if err != nil {
		return 0, nil, err
	}
	if length <= 0 || length > 1<<20 {
		return 0, nil, errors.New("implausible frame length")
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(r, frame); err != nil {
		return 0, nil, err
	}
	rest := bytes.NewReader(frame)

	if threshold >= 0 {
		dataLen, err := mcproto.ReadVarInt(rest)
		if err != nil {
			return 0, nil, err
		}
		if dataLen != 0 {
			if int(dataLen) < threshold {
				return 0, nil, errors.New("a packet below the threshold was compressed anyway")
			}
			zr, err := zlib.NewReader(rest)
			if err != nil {
				return 0, nil, err
			}
			plain, err := io.ReadAll(zr)
			if err != nil {
				return 0, nil, err
			}
			if len(plain) != int(dataLen) {
				return 0, nil, errors.New("the data length does not match what inflated")
			}
			rest = bytes.NewReader(plain)
		} else if rest.Len() >= threshold {
			return 0, nil, errors.New("a packet at or above the threshold was sent uncompressed")
		}
	}

	id, err := mcproto.ReadVarInt(rest)
	if err != nil {
		return 0, nil, err
	}
	payload, _ := io.ReadAll(rest)
	return id, payload, nil
}

func joinAgainst(t *testing.T, f *fake, username string, timeout time.Duration) (*Result, error) {
	t.Helper()
	host, port := start(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return Join(ctx, host, port, username)
}

func TestJoinCompletesAnOfflineLogin(t *testing.T) {
	f := &fake{}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	if result.Protocol != 771 {
		t.Errorf("Protocol = %d, want 771", result.Protocol)
	}
	if result.Username != "spawnery_probe" {
		t.Errorf("Username = %q, want %q", result.Username, "spawnery_probe")
	}
	// The offline-mode UUID of "spawnery_probe", written out rather than
	// derived from OfflineUUID so this pins the value and not the function.
	// It is not an arithmetic claim but a measured one: this is the UUID Paper
	// 26.2 logged for the player when spawnery-join really joined through the
	// pinned proxy — "UUID of player spawnery_probe is
	// bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f".
	const want = "bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f"
	if result.UUID != want {
		t.Errorf("UUID = %q, want %q", result.UUID, want)
	}
	if result.Compressed {
		t.Error("Compressed is true, but the server never sent Set Compression")
	}

	if !f.sawLoginAck() {
		t.Error("the server never received Login Acknowledged, so a real proxy would never have dialled a backend")
	}
	name, uuid := f.sawLogin()
	if name != "spawnery_probe" {
		t.Errorf("login start carried username %q", name)
	}
	if uuid != OfflineUUID("spawnery_probe") {
		t.Errorf("login start carried UUID %x, want the offline-mode one %x", uuid, OfflineUUID("spawnery_probe"))
	}
	if errs := f.errorsSeen(); len(errs) != 0 {
		t.Errorf("the server half reported %v", errs)
	}
}

func TestJoinSendsTheProtocolVersionItWasGiven(t *testing.T) {
	// A number no Minecraft release has ever used, so a client that quietly
	// substituted a constant of its own could not accidentally match.
	f := &fake{protocol: 4242}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if result.Protocol != 4242 {
		t.Errorf("Protocol = %d, want the 4242 the status document reported", result.Protocol)
	}

	got := f.sawHandshake()
	if got.protocol != 4242 {
		t.Errorf("the login handshake announced protocol %d, want the 4242 the status document reported", got.protocol)
	}
	if got.nextState != 2 {
		t.Errorf("the login handshake asked for next state %d, want 2", got.nextState)
	}
	if got.host != "127.0.0.1" {
		t.Errorf("the login handshake announced host %q, want the address dialled", got.host)
	}
}

func TestJoinAsksForTheVersionWithoutAnnouncingOneAServerSupports(t *testing.T) {
	// The status handshake has to announce a version no server supports, or a
	// proxy answers with the version it was asked about instead of its own and
	// the login that follows is refused by the backend. Measured against the
	// pinned pair — see announceUnsupported.
	f := &fake{protocol: 776}
	if _, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second); err != nil {
		t.Fatalf("Join: %v", err)
	}
	// -1 written out rather than compared against announceUnsupported: an
	// assertion phrased in terms of the constant it is testing passes whatever
	// that constant is changed to, which is how a test that cannot fail gets
	// written. Measured against Velocity 3.5.1, -1, 0 and 2147483647 all
	// produce its own maximum; a supported version produces itself.
	if got := f.sawStatusHandshake().protocol; got != -1 {
		t.Errorf("the status handshake announced protocol %d, want -1, which no server supports", got)
	}
	if got := f.sawStatusHandshake().nextState; got != 1 {
		t.Errorf("the status handshake asked for next state %d, want 1", got)
	}
	// And the login handshake still announces what came back, not what was
	// asked with.
	if got := f.sawHandshake().protocol; got != 776 {
		t.Errorf("the login handshake announced protocol %d, want the 776 the status document reported", got)
	}
}

func TestJoinRefusesAUsernameMinecraftWouldNot(t *testing.T) {
	// "spawnery-probe" is the shape this defaulted to until a real Paper
	// backend refused it. Velocity logs it in happily, so nothing before the
	// backend catches it.
	f := &fake{}
	_, err := joinAgainst(t, f, "spawnery-probe", 10*time.Second)
	if err == nil {
		t.Fatal("Join accepted a username with a hyphen in it")
	}
	if !strings.Contains(err.Error(), "valid Minecraft username") {
		t.Errorf("error is %q, want it to name the rule", err)
	}
	if f.sawLoginAck() {
		t.Error("Join went through with the login anyway")
	}
}

func TestNbtTextRendersTheMeasuredDisconnectReason(t *testing.T) {
	const want = "color red text Unable to connect you to unroutable. Please try again later."
	if got := nbtText(configurationDisconnectNBT); got != want {
		t.Errorf("nbtText =\n\t%q\nwant\n\t%q", got, want)
	}
}

func TestJoinHandlesSetCompression(t *testing.T) {
	// Eight, so Login Success is well over the threshold and really arrives
	// as a zlib stream, while Login Acknowledged is under it and must go back
	// with a data length of zero. The fake's reader refuses both mistakes.
	f := &fake{compressAt: 8}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !result.Compressed {
		t.Error("Compressed is false although the server sent Set Compression")
	}
	if result.Username != "spawnery_probe" {
		t.Errorf("Username = %q; a client that ignored the compression switch would read nonsense here", result.Username)
	}
	if !f.sawLoginAck() {
		t.Error("the server never received a readable Login Acknowledged after the switch")
	}
	if errs := f.errorsSeen(); len(errs) != 0 {
		t.Errorf("the server half reported %v", errs)
	}
}

func TestJoinReportsADisconnectReason(t *testing.T) {
	const reason = `{"translate":"multiplayer.disconnect.outdated_client","with":[{"text":"1.7.2-26.2"}]}`
	f := &fake{loginDisconnect: reason}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err == nil {
		t.Fatalf("Join succeeded with %+v, want the disconnect reported", result)
	}
	if !strings.Contains(err.Error(), "multiplayer.disconnect.outdated_client") {
		t.Errorf("error is %q, want it to carry the server's own reason", err)
	}
}

func TestJoinReportsAFailureToRouteAfterTheLoginSucceeded(t *testing.T) {
	// The measured shape of a proxy that logged the player in and then found
	// no backend: a configuration-state Disconnect, packet 0x02, carrying
	// network NBT rather than JSON. A client that returned as soon as it had
	// sent Login Acknowledged would call this a successful join.
	f := &fake{afterAckID: 0x02, afterAckPayload: configurationDisconnectNBT}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err == nil {
		t.Fatalf("Join succeeded with %+v, want the routing failure reported", result)
	}
	if !strings.Contains(err.Error(), "Unable to connect you to unroutable") {
		t.Errorf("error is %q, want it to carry the proxy's own reason", err)
	}
}

func TestJoinRefusesAnOnlineModeServer(t *testing.T) {
	f := &fake{encryptionRequest: true}
	if _, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second); err == nil {
		t.Fatal("Join succeeded against a server that asked for encryption")
	} else if !strings.Contains(err.Error(), "online mode") {
		t.Errorf("error is %q, want it to name online mode rather than the packet id", err)
	}
}

func TestJoinFailsCleanlyOnAClosedConnection(t *testing.T) {
	f := &fake{closeAfterLoginStart: true}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err == nil {
		t.Fatalf("Join succeeded with %+v against a server that hung up", result)
	}
	if result != nil {
		t.Errorf("Join returned %+v alongside its error", result)
	}
	if !strings.Contains(err.Error(), "EOF") {
		t.Errorf("error is %q, want it to name the hangup", err)
	}
}

func TestJoinFailsCleanlyOnAHangupAfterTheAcknowledgement(t *testing.T) {
	// The same hangup, one step later: nothing follows Login Acknowledged.
	// A proxy that dropped the connection here routed the player nowhere.
	f := &fake{closeAfterAck: true}
	if _, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second); err == nil {
		t.Fatal("Join succeeded although nothing followed Login Acknowledged")
	} else if !strings.Contains(err.Error(), "login acknowledged") {
		t.Errorf("error is %q, want it to say where the connection died", err)
	}
}

func TestJoinRespectsAContextDeadline(t *testing.T) {
	f := &fake{stallAfterLoginStart: true}
	start := time.Now()
	_, err := joinAgainst(t, f, "spawnery_probe", 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Join succeeded against a server that never answered")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Join took %s to give up on a 300ms deadline", elapsed)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("error is %q, want a timeout", err)
	}
}

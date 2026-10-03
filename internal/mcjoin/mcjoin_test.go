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

package mcjoin

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/mcproto"
)

// The fake server writes its frames by hand rather than through framedConn:
// encoding with the code under test would pass on a framing that is consistent
// and wrong on the wire.

// configurationDisconnectNBT is the payload Velocity 3.5.1 sent when it could
// not route a player: a nameless root compound holding color=red and text=<the
// message>. Kept verbatim from a real proxy.
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

type fake struct {
	protocol int
	// compressAt is the Set Compression threshold to send during login, or
	// negative for none.
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
	// afterAck is sent once Login Acknowledged arrives; the default stands in
	// for the proxy forwarding a backend's configuration.
	afterAckID      int32
	afterAckPayload []byte
	// closeAfterAck hangs up instead of sending afterAck.
	closeAfterAck bool
	// keepAlives is how many keep alives to send once the join is done, each
	// echoed before the next is sent. The connection then stays open until the
	// client closes it.
	keepAlives int
	// holdDisconnect, when set, is sent right after afterAck.
	holdDisconnect []byte
	// loginCookieRequests are asked for before Login Success, the way a proxy
	// receiving a transfer asks.
	loginCookieRequests []string
	// play drives the client into the play state at protocol 777 before
	// anything below; reconfigure then sends it back through the configuration
	// state once, as a server switch does.
	play        bool
	reconfigure bool
	// storeCookies are stored on the client right after afterAck (or after
	// Join Game), and cookieRequests asked for after them.
	storeCookies   map[string][]byte
	cookieRequests []string
	// transferHost and transferPort, when set, are sent as a Transfer after the
	// cookies and transferDelay.
	transferHost  string
	transferPort  int
	transferDelay time.Duration

	mu              sync.Mutex
	handshake       handshakeFields
	statusHandshake handshakeFields
	answered        int
	loginName       string
	loginUUID       [16]byte
	gotLoginAck     bool
	serverErrors    []error
	cookieAnswers   map[string]cookieAnswer
	logins          int
}

type cookieAnswer struct {
	present bool
	payload []byte
}

func (f *fake) sawCookieAnswers() map[string]cookieAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]cookieAnswer{}
	for k, v := range f.cookieAnswers {
		out[k] = v
	}
	return out
}

func (f *fake) loginsSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

// stateIDs are written out rather than shared with the client.
type stateIDs struct {
	storeCookie, transfer, cookieRequest, cookieResponse, keepAlive, keepAliveAnswer int32
}

func configIDs(protocol int) stateIDs {
	ids := stateIDs{storeCookie: 0x0a, transfer: 0x0b, cookieRequest: 0x00, cookieResponse: 0x01, keepAlive: 0x04, keepAliveAnswer: 0x04}
	if protocol >= 777 {
		ids.storeCookie, ids.transfer = 0x0b, 0x0c
	}
	return ids
}

// playIDs are protocol 777's, from Velocity 4.2.0-30's StateRegistry.
var playIDs = stateIDs{storeCookie: 0x7a, transfer: 0x84, cookieRequest: 0x15, cookieResponse: 0x15, keepAlive: 0x2d, keepAliveAnswer: 0x1c}

// configure ends with Join Game and one play packet the client has to ignore.
func (f *fake) configure(conn net.Conn, threshold int) error {
	knownPacks := mcproto.AppendVarInt(nil, 1)
	knownPacks = mcproto.AppendString(knownPacks, "minecraft")
	knownPacks = mcproto.AppendString(knownPacks, "core")
	knownPacks = mcproto.AppendString(knownPacks, "26.3")
	if err := writeFrame(conn, threshold, 0x0f, knownPacks); err != nil {
		return err
	}
	if err := expect(conn, threshold, 0x07, "known packs"); err != nil {
		return err
	}
	if err := writeFrame(conn, threshold, 0x03, nil); err != nil {
		return err
	}
	if err := expect(conn, threshold, 0x03, "acknowledge finish configuration"); err != nil {
		return err
	}
	if err := writeFrame(conn, threshold, 0x32, []byte{0, 0, 0, 1}); err != nil {
		return err
	}
	return writeFrame(conn, threshold, 0x27, bytes.Repeat([]byte{0x2c}, 64))
}

func expect(conn net.Conn, threshold int, want int32, name string) error {
	id, _, err := readFrame(conn, threshold)
	if err != nil {
		return fmt.Errorf("waiting for %s: %w", name, err)
	}
	if id != want {
		return fmt.Errorf("expected %s 0x%02x, got packet 0x%02x", name, want, id)
	}
	return nil
}

func (f *fake) askForCookie(conn net.Conn, threshold int, requestID, responseID int32, key string) error {
	if err := writeFrame(conn, threshold, requestID, mcproto.AppendString(nil, key)); err != nil {
		return err
	}
	id, payload, err := readFrame(conn, threshold)
	if err != nil {
		return err
	}
	if id != responseID {
		return fmt.Errorf("expected a cookie response 0x%02x, got packet 0x%02x", responseID, id)
	}
	r := bytes.NewReader(payload)
	got, err := readStringFrom(r)
	if err != nil {
		return err
	}
	if got != key {
		return fmt.Errorf("cookie response names %q, want %q", got, key)
	}
	present, err := r.ReadByte()
	if err != nil {
		return err
	}
	answer := cookieAnswer{present: present == 1}
	if answer.present {
		n, err := mcproto.ReadVarInt(r)
		if err != nil {
			return err
		}
		answer.payload = make([]byte, n)
		if _, err := io.ReadFull(r, answer.payload); err != nil {
			return err
		}
	}
	if r.Len() != 0 {
		return fmt.Errorf("cookie response carries %d trailing bytes", r.Len())
	}
	f.mu.Lock()
	if f.cookieAnswers == nil {
		f.cookieAnswers = map[string]cookieAnswer{}
	}
	f.cookieAnswers[key] = answer
	f.mu.Unlock()
	return nil
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

func (f *fake) keepAlivesAnswered() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answered
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

// start serves connections on a loopback port until the test ends. Join makes
// two: the status ping, then the login.
func start(t *testing.T, f *fake) (string, int) {
	t.Helper()
	if f.protocol == 0 {
		f.protocol = 771
	}
	if f.compressAt == 0 {
		f.compressAt = -1
	}
	if f.afterAckID == 0 && f.afterAckPayload == nil {
		f.afterAckID = 0x01
		f.afterAckPayload = mcproto.AppendString(nil, "minecraft:brand")
	}

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
				defer func() { _ = conn.Close() }()
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
	f.logins++
	f.mu.Unlock()

	for _, key := range f.loginCookieRequests {
		if err := f.askForCookie(conn, -1, 0x05, 0x04, key); err != nil {
			return err
		}
	}

	switch {
	case f.closeAfterLoginStart:
		return nil
	case f.stallAfterLoginStart:
		// The read deadline set at the top of serve releases this goroutine.
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
	if err := writeFrame(conn, threshold, f.afterAckID, f.afterAckPayload); err != nil {
		return err
	}

	if f.holdDisconnect != nil {
		return writeFrame(conn, threshold, 0x02, f.holdDisconnect)
	}
	ids := configIDs(f.protocol)
	if f.play {
		if err := f.configure(conn, threshold); err != nil {
			return err
		}
		if f.reconfigure {
			if err := writeFrame(conn, threshold, 0x78, nil); err != nil {
				return err
			}
			if err := expect(conn, threshold, 0x10, "acknowledge configuration"); err != nil {
				return err
			}
			if err := f.configure(conn, threshold); err != nil {
				return err
			}
		}
		ids = playIDs
	}
	for key, value := range f.storeCookies {
		payload := mcproto.AppendString(nil, key)
		payload = mcproto.AppendVarInt(payload, int32(len(value)))
		payload = append(payload, value...)
		if err := writeFrame(conn, threshold, ids.storeCookie, payload); err != nil {
			return err
		}
	}
	for _, key := range f.cookieRequests {
		if err := f.askForCookie(conn, threshold, ids.cookieRequest, ids.cookieResponse, key); err != nil {
			return err
		}
	}
	if f.transferHost != "" {
		time.Sleep(f.transferDelay)
		payload := mcproto.AppendString(nil, f.transferHost)
		payload = mcproto.AppendVarInt(payload, int32(f.transferPort))
		if err := writeFrame(conn, threshold, ids.transfer, payload); err != nil {
			return err
		}
		_, _ = conn.Read(make([]byte, 1))
		return nil
	}
	for i := 0; i < f.keepAlives; i++ {
		sent := []byte{0, 0, 0, 0, 0, 0, 0, byte(i + 1)}
		if err := writeFrame(conn, threshold, ids.keepAlive, sent); err != nil {
			return err
		}
		id, echoed, err := readFrame(conn, threshold)
		if err != nil {
			return err
		}
		if id != ids.keepAliveAnswer {
			return fmt.Errorf("expected a keep alive answer, got packet 0x%02x", id)
		}
		if !bytes.Equal(echoed, sent) {
			return fmt.Errorf("keep alive answered with %x, want %x", echoed, sent)
		}
		f.mu.Lock()
		f.answered++
		f.mu.Unlock()
	}
	if f.keepAlives > 0 {
		_, _ = conn.Read(make([]byte, 1))
	}
	return nil
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

// writeFrame assembles one frame by hand. A negative threshold is the
// uncompressed framing.
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

// readFrame also asserts that a body under the threshold arrives with a data
// length of zero.
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
	// The UUID Paper 26.2 logged for spawnery_probe joining through the pinned
	// proxy; a literal, so this pins the value and not the function.
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
	if got := formatUUID(uuid[:]); got != want {
		t.Errorf("login start carried UUID %s, want the offline-mode one %s", got, want)
	}
	if errs := f.errorsSeen(); len(errs) != 0 {
		t.Errorf("the server half reported %v", errs)
	}
}

func TestJoinSendsTheProtocolVersionItWasGiven(t *testing.T) {
	// A number no Minecraft release has used.
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
	f := &fake{protocol: 776}
	if _, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second); err != nil {
		t.Fatalf("Join: %v", err)
	}
	// -1 written out rather than compared against announceUnsupported, so
	// changing the constant fails this.
	if got := f.sawStatusHandshake().protocol; got != -1 {
		t.Errorf("the status handshake announced protocol %d, want -1, which no server supports", got)
	}
	if got := f.sawStatusHandshake().nextState; got != 1 {
		t.Errorf("the status handshake asked for next state %d, want 1", got)
	}
	if got := f.sawHandshake().protocol; got != 776 {
		t.Errorf("the login handshake announced protocol %d, want the 776 the status document reported", got)
	}
}

func TestJoinRefusesAUsernameMinecraftWouldNot(t *testing.T) {
	// Velocity accepts "spawnery-probe"; only Paper rejects it.
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
	// Login Success arrives as a zlib stream; Login Acknowledged is under the
	// threshold and must go back with a data length of zero.
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

func TestJoinReadsPacketsSentWithAZeroDataLength(t *testing.T) {
	// Velocity's threshold is 256 and everything this client reads is smaller,
	// so every packet arrives compressed-framed with a data length of zero.
	// This puts every packet on that branch.
	f := &fake{compressAt: 4096}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !result.Compressed {
		t.Error("Compressed is false although the server sent Set Compression")
	}
	if result.Username != "spawnery_probe" {
		t.Errorf("Username = %q; a client that mishandled the zero data length would read nonsense here", result.Username)
	}
	if !f.sawLoginAck() {
		t.Error("the server never received a readable Login Acknowledged")
	}
	if errs := f.errorsSeen(); len(errs) != 0 {
		t.Errorf("the server half reported %v", errs)
	}
}

func TestJoinReportsAFailureToRouteUnderCompression(t *testing.T) {
	// Against a real proxy this Disconnect is always compressed-framed.
	f := &fake{compressAt: 4096, afterAckID: 0x02, afterAckPayload: configurationDisconnectNBT}
	result, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second)
	if err == nil {
		t.Fatalf("Join succeeded with %+v, want the routing failure reported", result)
	}
	if !strings.Contains(err.Error(), "Unable to connect you to unroutable") {
		t.Errorf("error is %q, want it to carry the proxy's own reason", err)
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
	// A proxy that logged the player in and found no backend. A client
	// returning right after Login Acknowledged would call this a success.
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
	f := &fake{closeAfterAck: true}
	if _, err := joinAgainst(t, f, "spawnery_probe", 10*time.Second); err == nil {
		t.Fatal("Join succeeded although nothing followed Login Acknowledged")
	} else if !strings.Contains(err.Error(), "login acknowledged") {
		t.Errorf("error is %q, want it to say where the connection died", err)
	}
}

func TestJoinAndHoldStaysConnectedAndAnswersKeepAlives(t *testing.T) {
	// The fake sends the second keep alive only after the first came back.
	f := &fake{keepAlives: 2}
	host, port := start(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	began := time.Now()
	result, err := JoinAndHold(ctx, host, port, "spawnery_probe", 400*time.Millisecond)
	elapsed := time.Since(began)
	if err != nil {
		t.Fatalf("JoinAndHold: %v", err)
	}
	if result.Username != "spawnery_probe" {
		t.Errorf("Username = %q", result.Username)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("JoinAndHold returned after %s, before the 400ms hold was up", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("JoinAndHold took %s for a 400ms hold", elapsed)
	}
	if got := f.keepAlivesAnswered(); got != 2 {
		t.Errorf("the server got %d keep alive answers, want 2", got)
	}
	if errs := f.errorsSeen(); len(errs) != 0 {
		t.Errorf("the server half reported %v", errs)
	}
}

func TestJoinAndHoldReportsADisconnectDuringTheHold(t *testing.T) {
	f := &fake{holdDisconnect: configurationDisconnectNBT}
	host, port := start(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := JoinAndHold(ctx, host, port, "spawnery_probe", 3*time.Second)
	if err == nil {
		t.Fatalf("JoinAndHold succeeded with %+v although the player was disconnected", result)
	}
	if !strings.Contains(err.Error(), "during the hold") {
		t.Errorf("error is %q, want it to say the player did not stay", err)
	}
	if !strings.Contains(err.Error(), "Unable to connect you to unroutable") {
		t.Errorf("error is %q, want it to carry the server's own reason", err)
	}
}

func TestJoinAndHoldRefusesAHoldTheDeadlineCannotCover(t *testing.T) {
	// A truncated hold ends in the same read timeout as a completed one.
	f := &fake{}
	host, port := start(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := JoinAndHold(ctx, host, port, "spawnery_probe", 10*time.Second); err == nil {
		t.Fatal("JoinAndHold accepted a hold longer than the deadline")
	} else if !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("error is %q, want it to name the mismatch", err)
	}
	if f.sawLoginAck() {
		t.Error("JoinAndHold logged in anyway")
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

func TestJoinAndHoldAnswersCookieRequestsFromWhatWasStored(t *testing.T) {
	for _, protocol := range []int{776, 777} {
		t.Run(strconv.Itoa(protocol), func(t *testing.T) {
			f := &fake{
				protocol:       protocol,
				compressAt:     4096,
				storeCookies:   map[string][]byte{"spawnery:transfer": []byte("ticket")},
				cookieRequests: []string{"spawnery:transfer", "spawnery:unknown"},
				keepAlives:     1,
			}
			host, port := start(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if _, err := JoinWith(ctx, host, port, "spawnery_probe", Options{Hold: 400 * time.Millisecond}); err != nil {
				t.Fatalf("JoinWith: %v", err)
			}
			answers := f.sawCookieAnswers()
			if got := answers["spawnery:transfer"]; !got.present || string(got.payload) != "ticket" {
				t.Errorf("the stored cookie was answered with %+v, want \"ticket\"", got)
			}
			if got, ok := answers["spawnery:unknown"]; !ok || got.present {
				t.Errorf("a cookie never stored was answered with %+v (asked: %v), want an answer without a payload", got, ok)
			}
			if errs := f.errorsSeen(); len(errs) != 0 {
				t.Errorf("the server half reported %v", errs)
			}
		})
	}
}

func TestJoinAndHoldFollowsATransferWithItsCookies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		play        bool
		reconfigure bool
	}{
		{name: "configuration"},
		{name: "play", play: true},
		{name: "play after a reconfiguration", play: true, reconfigure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &fake{
				protocol:            777,
				compressAt:          4096,
				play:                tc.play,
				loginCookieRequests: []string{"spawnery:transfer", "spawnery:unknown"},
				keepAlives:          1,
			}
			targetHost, targetPort := start(t, target)
			source := &fake{
				protocol:       777,
				compressAt:     4096,
				play:           tc.play,
				reconfigure:    tc.reconfigure,
				storeCookies:   map[string][]byte{"spawnery:transfer": []byte("ticket")},
				cookieRequests: []string{"spawnery:transfer"},
				transferHost:   targetHost,
				transferPort:   targetPort,
				transferDelay:  500 * time.Millisecond,
			}
			host, port := start(t, source)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			const hold = 1500 * time.Millisecond
			began := time.Now()
			result, err := JoinWith(ctx, host, port, "spawnery_probe", Options{Hold: hold, FollowTransfers: true})
			elapsed := time.Since(began)
			if err != nil {
				t.Fatalf("JoinWith: %v", err)
			}
			if result.Transfers != 1 {
				t.Errorf("Transfers = %d, want 1", result.Transfers)
			}
			// The hold is one span across both connections.
			if elapsed < hold || elapsed > hold+300*time.Millisecond {
				t.Errorf("JoinWith returned after %s, want about the %s hold, not the hold plus the 500ms before the transfer", elapsed, hold)
			}

			hs := target.sawHandshake()
			if hs.nextState != 3 {
				t.Errorf("the transferred handshake asked for next state %d, want 3", hs.nextState)
			}
			if hs.protocol != 777 || hs.host != targetHost || int(hs.port) != targetPort {
				t.Errorf("the transferred handshake was %+v, want protocol 777 to %s:%d", hs, targetHost, targetPort)
			}
			if got := source.sawCookieAnswers()["spawnery:transfer"]; !got.present || string(got.payload) != "ticket" {
				t.Errorf("the source's own cookie request was answered with %+v, want \"ticket\"", got)
			}
			answers := target.sawCookieAnswers()
			if got := answers["spawnery:transfer"]; !got.present || string(got.payload) != "ticket" {
				t.Errorf("the transfer cookie was answered with %+v, want \"ticket\"", got)
			}
			if got, ok := answers["spawnery:unknown"]; !ok || got.present {
				t.Errorf("a cookie never stored was answered with %+v (asked: %v), want an answer without a payload", got, ok)
			}
			if got := target.keepAlivesAnswered(); got != 1 {
				t.Errorf("the target got %d keep alive answers, want 1: the hold did not continue there", got)
			}
			for _, f := range []*fake{source, target} {
				if errs := f.errorsSeen(); len(errs) != 0 {
					t.Errorf("a server half reported %v", errs)
				}
			}
		})
	}
}

func TestJoinWithRefusesToFollowTransfersAtAnotherProtocol(t *testing.T) {
	f := &fake{protocol: 776, play: true}
	host, port := start(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := JoinWith(ctx, host, port, "spawnery_probe", Options{Hold: time.Second, FollowTransfers: true})
	if err == nil {
		t.Fatal("JoinWith followed transfers at protocol 776, whose play-state ids it does not know")
	}
	if !strings.Contains(err.Error(), "776") || !strings.Contains(err.Error(), "777") {
		t.Errorf("error is %q, want it to name the protocol reported and the one supported", err)
	}
	if got := f.loginsSeen(); got != 0 {
		t.Errorf("the server saw %d logins, want the refusal before any", got)
	}
}

func TestJoinAndHoldEndsAtATransferItDoesNotFollow(t *testing.T) {
	target := &fake{keepAlives: 1}
	targetHost, targetPort := start(t, target)
	source := &fake{protocol: 777, transferHost: targetHost, transferPort: targetPort}
	host, port := start(t, source)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := JoinAndHold(ctx, host, port, "spawnery_probe", 3*time.Second)
	if err == nil {
		t.Fatalf("JoinAndHold succeeded with %+v although the player was transferred away", result)
	}
	if !strings.Contains(err.Error(), "transferred") || !strings.Contains(err.Error(), net.JoinHostPort(targetHost, strconv.Itoa(targetPort))) {
		t.Errorf("error is %q, want it to name the transfer and its target", err)
	}
	if got := target.loginsSeen(); got != 0 {
		t.Errorf("the target saw %d logins, want none", got)
	}
}

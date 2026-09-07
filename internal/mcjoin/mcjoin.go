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

// Package mcjoin logs in to a Minecraft proxy far enough to be routed to a
// backend, so that "a player can join" can be claimed without a human holding
// a Microsoft account.
//
// Velocity does not dial a backend when the login completes. It dials it when
// the client answers Login Success with Login Acknowledged, and needs nothing
// further -- no FinishConfiguration, no acknowledgement of one. A client that
// stops short of that acknowledgement still produces Velocity's "has
// connected" line and no backend attempt at all, so that line is not evidence
// of routing.
//
// Join reads one packet past the acknowledgement, because a routing failure
// reaches the client only after the login has already succeeded, as a
// configuration-state Disconnect. Returning at the acknowledgement would
// report success for a proxy that can route the player nowhere, which is the
// one failure this tool exists to catch. So a configuration-state Disconnect
// is an error, and anything else is the proxy forwarding the backend's
// configuration and therefore proof of a backend.
//
// The two disconnects are neither the same packet nor the same encoding,
// which is why they are handled separately below: in login state, packet 0x00
// carrying a length-prefixed JSON string; in configuration state, packet 0x02
// carrying network NBT with a nameless root compound.
//
// The proxy has to be in offline mode, since this client authenticates
// against nothing and an online-mode proxy answers it with an encryption
// request it cannot satisfy. Set spec.config.onlineMode: false on the
// ProxyGroup -- the CRD field, which no configOverlay can reach, because the
// renderer reasserts the keys it owns after merging one.
package mcjoin

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spawnery/spawnery/internal/mcproto"
	"github.com/spawnery/spawnery/internal/slp"
)

// Packet ids. Only the ones this client sends or recognises are named; the
// numbers are the same across every protocol version Velocity 3.5.1 accepts
// for the login and configuration states.
const (
	// Serverbound.
	idHandshake         = 0x00
	idLoginStart        = 0x00
	idLoginAcknowledged = 0x03

	// Clientbound, login state.
	idLoginDisconnect    = 0x00
	idEncryptionRequest  = 0x01
	idLoginSuccess       = 0x02
	idSetCompression     = 0x03
	idLoginPluginRequest = 0x04
	// Configuration state. Disconnect is clientbound only; keep alive has the
	// same id in both directions and is answered by echoing its payload.
	idConfigurationDisconnect = 0x02
	idConfigurationKeepAlive  = 0x04
)

// nextStateLogin is the handshake's final field. internal/slp sends 1 for a
// status request; this is the other half of the same enum.
const nextStateLogin = 2

// announceUnsupported is the protocol version this client announces when it
// asks a server which version it speaks, and it is deliberately one no server
// supports.
//
// The obvious thing does not work: Velocity answers a status request with the
// asker's own version whenever it supports it, so slp.Ping's handshake
// version comes back unchanged, and logging in with it reaches the backend
// only to be refused as an outdated client. Announcing a version no server
// supports is answered with the proxy's own maximum instead. -1 is what a
// client with no version to declare conventionally sends; 0 and 2147483647
// behave identically.
//
// What this relies on is that the proxy's newest supported version and the
// backend's version agree, which is true of every pinned pair this repository
// ships and is not guaranteed of an arbitrary one. When it stops being true,
// the failure is the "Outdated client!" line above, which names the version
// to fix it to.
const announceUnsupported = -1

// maxPacketLen bounds a compressed packet's frame and its inflated size, for
// the same reason mcproto bounds an uncompressed one: it is the only
// unbounded allocation on this path, and a data-length VarInt is attacker
// controlled twice over — once as the frame length and once as the size the
// zlib stream claims to expand to.
const maxPacketLen = 2 << 20

// validUsername is what a Minecraft username may be, and what Paper enforces
// on a player arriving through a proxy.
var validUsername = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

// Result is what a completed login proves. The JSON tags are cmd/spawnery-join's:
// it prints a Result as one line for a runbook step to assert on with jq, and
// lower-case keys are what such a step should have to type.
type Result struct {
	// Protocol is the version number the server reported for itself and this
	// client then announced. It is asked for rather than hardcoded — see Join.
	Protocol int `json:"protocol"`
	// Username and UUID are the server's own answer in Login Success, not the
	// values sent. Against an offline-mode server they come back unchanged,
	// and a difference is worth seeing rather than hiding.
	Username string `json:"username"`
	UUID     string `json:"uuid"`
	// Compressed records whether the server switched on compression during
	// login. Velocity's default compression-threshold is 256, so a false here
	// against a real proxy means the framing was never exercised.
	Compressed bool `json:"compressed"`
}

// Join connects, logs in as username against an offline-mode server, and
// returns once the server has acknowledged the login and shown that it can
// route the player somewhere.
//
// The protocol version is asked for rather than hardcoded, which is what
// keeps this client in step with a Paper or Velocity bump — but it is asked
// for with announceUnsupported rather than through slp.Ping, for the reason
// recorded there.
//
// The deadline comes from ctx, applied to both connections. Plain
// cancellation with no deadline set is not otherwise observed, the same
// caveat slp.Ping carries.
func Join(ctx context.Context, host string, port int, username string) (*Result, error) {
	return JoinAndHold(ctx, host, port, username, 0)
}

// JoinAndHold is Join with the connection kept open for hold once the join has
// succeeded, which is the only way anything outside this process can observe
// the player: Join closes the socket as it returns, and a proxy's
// status.connectedPlayers is back to zero before a kubectl in the next line of
// a runbook could read it.
//
// During the hold this client answers configuration-state keep alives, because
// a server that gets no answer disconnects. It returns early, with an error, if
// the server disconnects the player anyway — a hold that ended in a disconnect
// is not a player who stayed.
//
// hold must fit inside ctx's deadline. A hold that would outlast it is refused
// rather than silently truncated, because a truncated hold and a completed one
// end with the same read timeout and would be indistinguishable here.
func JoinAndHold(ctx context.Context, host string, port int, username string, hold time.Duration) (*Result, error) {
	if hold < 0 {
		return nil, fmt.Errorf("a hold of %s is negative", hold)
	}
	if deadline, ok := ctx.Deadline(); ok && hold > 0 && !time.Now().Add(hold).Before(deadline) {
		return nil, fmt.Errorf("a hold of %s does not fit inside the %s left on the deadline",
			hold, time.Until(deadline).Round(time.Millisecond))
	}

	// Checked here rather than left to the server, because the two disagree
	// and the second one is late. Velocity 3.5.1 in offline mode accepted
	// "spawnery-probe" and logged it in; Paper 26.2 then dropped the forwarded
	// connection with "Internal Exception: java.lang.IllegalStateException:
	// Invalid characters in username", which reached the client as the proxy's
	// own "Unable to connect to lobby: disconnect.genericReason" — a message
	// naming neither the username nor the character in it.
	if !validUsername.MatchString(username) {
		return nil, fmt.Errorf("%q is not a valid Minecraft username: 1 to 16 characters of A-Z, a-z, 0-9 or _", username)
	}

	status, err := slp.PingVersion(ctx, host, port, announceUnsupported)
	if err != nil {
		return nil, fmt.Errorf("ask for the protocol version: %w", err)
	}
	protocol := status.Version.Protocol
	if protocol <= 0 {
		return nil, fmt.Errorf("the server reported protocol version %d, which cannot be announced in a handshake", protocol)
	}

	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = c.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		if err := c.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set deadline: %w", err)
		}
	}

	conn := &framedConn{rw: c, threshold: -1}

	var handshake []byte
	handshake = mcproto.AppendVarInt(handshake, int32(protocol))
	handshake = mcproto.AppendString(handshake, host)
	handshake = binary.BigEndian.AppendUint16(handshake, uint16(port))
	handshake = mcproto.AppendVarInt(handshake, nextStateLogin)
	if err := conn.writePacket(idHandshake, handshake); err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	// The offline-mode UUID, computed the way Velocity and Paper compute it,
	// so the client does not present an identity the proxy would reject: the
	// MD5 name-based UUID of "OfflinePlayer:<username>", with the version
	// nibble forced to 3 and the variant to RFC 4122.
	offline := OfflineUUID(username)
	var loginStart []byte
	loginStart = mcproto.AppendString(loginStart, username)
	loginStart = append(loginStart, offline[:]...)
	if err := conn.writePacket(idLoginStart, loginStart); err != nil {
		return nil, fmt.Errorf("write login start: %w", err)
	}

	result := &Result{Protocol: protocol}
	for {
		id, payload, err := conn.readPacket()
		if err != nil {
			return nil, fmt.Errorf("read login response: %w", err)
		}
		switch id {
		case idSetCompression:
			threshold, err := mcproto.ReadVarInt(bytes.NewReader(payload))
			if err != nil {
				return nil, fmt.Errorf("read compression threshold: %w", err)
			}
			// A negative threshold turns compression back off; the spec
			// allows it and nothing in this project sends it, so it is
			// handled by the same assignment rather than by a special case.
			conn.threshold = int(threshold)
			result.Compressed = threshold >= 0
		case idLoginDisconnect:
			reason, err := readString(payload)
			if err != nil {
				return nil, fmt.Errorf("read disconnect reason: %w", err)
			}
			return nil, fmt.Errorf("the server refused the login: %s", reason)
		case idEncryptionRequest:
			// Not a protocol failure but a configuration one, and worth its
			// own sentence: this client has no Microsoft account and cannot
			// answer.
			return nil, errors.New("the server is in online mode and asked for encryption, which this client cannot answer")
		case idLoginPluginRequest:
			// Velocity sends these towards backends as its modern forwarding
			// handshake, not towards clients, so this is unreachable against
			// the proxy this tool exists to test. Refusing loudly beats the
			// alternative: a server that gets no response waits forever, and
			// the symptom would be a timeout naming nothing.
			return nil, errors.New("the server sent a login plugin request, which this client does not implement")
		case idLoginSuccess:
			if err := readLoginSuccess(payload, result); err != nil {
				return nil, err
			}
			if err := conn.writePacket(idLoginAcknowledged, nil); err != nil {
				return nil, fmt.Errorf("write login acknowledged: %w", err)
			}
			// Login Acknowledged is what makes Velocity dial a backend; see
			// the package comment. What comes back says whether it found one.
			if err := conn.awaitRouting(); err != nil {
				return nil, err
			}
			if hold > 0 {
				// The read timeout is how the hold ends, so the deadline is
				// moved in from ctx's to the end of the hold. The check at the
				// top of this function is what makes that a shortening.
				if err := c.SetDeadline(time.Now().Add(hold)); err != nil {
					return nil, fmt.Errorf("set hold deadline: %w", err)
				}
				if err := conn.holdOpen(); err != nil {
					return nil, err
				}
			}
			return result, nil
		default:
			return nil, fmt.Errorf("unexpected packet id 0x%02x during login", id)
		}
	}
}

// holdOpen reads until the connection's deadline expires, which is how a
// successful hold ends: there is nothing this client wants from those packets
// except the two it has to act on.
//
// # What a held connection is not
//
// It stops one packet after Login Acknowledged, in the configuration state,
// which is as far as Velocity needs before it dials a backend. Paper's
// getOnlinePlayers() never contains such a client, because it never finishes
// the configuration phase Paper is waiting for -- so the Paper agent reports
// zero players and Server.status.players reads zero for a connection the
// proxy is actively holding open. A held player therefore cannot stand in for
// a real one wherever the *backend's* count is what is read.
//
// Closing that needs this client to drive the exchange rather than answer it,
// which is the part a reading of the protocol gets wrong. After Login
// Acknowledged the server sends Plugin Message 0x01 (minecraft:brand),
// Feature Flags 0x0c and Select Known Packs 0x0e, and then nothing but Keep
// Alive 0x04 for as long as the client waits: it never sends Finish
// Configuration unprompted, so a case that answers one packet waits for a
// packet that never comes. What moves it:
//
//   - serverbound Known Packs 0x07 with an empty list, after which the server
//     sends its Registry Data 0x07 and Update Tags 0x0d, tens of kilobytes
//   - clientbound Finish Configuration 0x03, empty payload
//   - serverbound Acknowledge Finish Configuration 0x03, empty -- after which
//     the server counts the player
//
// The hold then sits in the play state, where Keep Alive is 0x2c carrying a
// millisecond timestamp rather than the configuration state's 0x04. So it is
// four constants and a small state machine that leads, not two constants and
// a case.
func (c *framedConn) holdOpen() error {
	for {
		id, payload, err := c.readPacket()
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return nil
			}
			return fmt.Errorf("hold the connection open: %w", err)
		}
		switch id {
		case idConfigurationDisconnect:
			return fmt.Errorf("the player was disconnected during the hold: %s", nbtText(payload))
		case idConfigurationKeepAlive:
			// Echoed whole: the payload is the id the server wants back, and
			// a server that does not get it drops the connection. Measured
			// against the pinned pair — which sends one about every second —
			// by holding a real join open for 40 seconds, several times any
			// keep-alive timeout: the proxy logged the disconnect only when
			// this client closed the socket, 40s after the connect.
			//
			//	[19:04:13 INFO]: [server connection] spawnery_probe -> lobby has connected
			//	[19:04:54 INFO]: [connected player] spawnery_probe has disconnected
			if err := c.writePacket(idConfigurationKeepAlive, payload); err != nil {
				return fmt.Errorf("answer a keep alive: %w", err)
			}
		}
	}
}

// awaitRouting reads the one packet that follows Login Acknowledged.
func (c *framedConn) awaitRouting() error {
	id, payload, err := c.readPacket()
	if err != nil {
		return fmt.Errorf("read the proxy's answer to login acknowledged: %w", err)
	}
	if id == idConfigurationDisconnect {
		return fmt.Errorf("the login succeeded but the player was not routed: %s", nbtText(payload))
	}
	return nil
}

// OfflineUUID is the UUID an offline-mode server assigns to username: the MD5
// name-based UUID of "OfflinePlayer:<username>", version 3, RFC 4122 variant.
// Exported because cmd/spawnery-join prints it and a runbook step that greps
// a server log for the player's UUID needs the same value.
func OfflineUUID(username string) [16]byte {
	sum := md5.Sum([]byte("OfflinePlayer:" + username))
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	return sum
}

// formatUUID renders sixteen bytes in the canonical 8-4-4-4-12 form.
func formatUUID(b []byte) string {
	const hex = "0123456789abcdef"
	var out []byte
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hex[v>>4], hex[v&0x0f])
	}
	return string(out)
}

// readLoginSuccess reads the two fields this tool reports out of Login
// Success: the UUID as sixteen raw bytes, then the username. Everything after
// them — the property array, and whatever a later protocol version appends —
// is deliberately not parsed, because none of it is evidence of anything this
// tool claims and every field parsed is a field that can break on a bump.
func readLoginSuccess(payload []byte, result *Result) error {
	if len(payload) < 16 {
		return fmt.Errorf("login success carries %d bytes, too few for a UUID", len(payload))
	}
	result.UUID = formatUUID(payload[:16])
	name, err := readString(payload[16:])
	if err != nil {
		return fmt.Errorf("read the username from login success: %w", err)
	}
	result.Username = name
	return nil
}

// readString reads one length-prefixed string from the front of b.
func readString(b []byte) (string, error) {
	r := bytes.NewReader(b)
	length, err := mcproto.ReadVarInt(r)
	if err != nil {
		return "", err
	}
	if length < 0 || int(length) > r.Len() {
		return "", fmt.Errorf("string length %d exceeds the %d bytes that follow it", length, r.Len())
	}
	s := make([]byte, length)
	if _, err := io.ReadFull(r, s); err != nil {
		return "", err
	}
	return string(s), nil
}

// nbtText renders a configuration-state disconnect reason readably.
//
// That payload is network NBT, not JSON, and its text sits in string tags
// whose lengths and tag bytes are binary. Rather than carry an NBT decoder
// for a diagnostic message, this keeps printable runs of three characters or
// more and joins them with a space. The field names survive alongside their
// values, which is untidy and honest: the payload rendered legibly, not a
// component tree resolved.
//
// The one place it does more is the length prefix. An NBT string is two
// big-endian length bytes and then its content, so a message of 32 to 126
// characters has a low length byte that is itself printable and would lead
// the message as a stray character. A run whose first byte is exactly the
// length of what follows it is that prefix, and is dropped.
func nbtText(payload []byte) string {
	var runs []string
	start := -1
	flush := func(end int) {
		if start >= 0 && end-start >= 3 {
			from := start
			// The two bytes before the content of an NBT string are its
			// length. When the low one is printable it opened this run.
			if from >= 1 && int(payload[from-1])<<8|int(payload[from]) == end-from-1 {
				from++
			}
			runs = append(runs, string(payload[from:end]))
		}
		start = -1
	}
	for i, b := range payload {
		if r := rune(b); r < unicode.MaxASCII && unicode.IsPrint(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(payload))
	if len(runs) == 0 {
		return fmt.Sprintf("an unreadable %d-byte reason", len(payload))
	}
	return strings.Join(runs, " ")
}

// framedConn is one connection and the framing currently in force on it.
// Compression is negotiated mid-login, so the same connection speaks both
// framings and the switch is a field rather than a type.
type framedConn struct {
	rw io.ReadWriter
	// threshold is the compressed framing's minimum body size, or a negative
	// number while compression is off. Zero is a real value and means
	// compress everything, so "off" cannot be spelled as zero here.
	threshold int
}

func (c *framedConn) compressed() bool { return c.threshold >= 0 }

// writePacket frames and writes one packet in whichever framing is in force.
func (c *framedConn) writePacket(id int32, payload []byte) error {
	if !c.compressed() {
		return mcproto.WritePacket(c.rw, id, payload)
	}

	var body []byte
	body = mcproto.AppendVarInt(body, id)
	body = append(body, payload...)

	// A body under the threshold is sent with a data length of zero and no
	// zlib stream at all. Compressing it anyway is not merely wasteful, it is
	// wrong: a server reading a data length below its own threshold treats
	// the connection as broken.
	var inner []byte
	if len(body) >= c.threshold {
		var deflated bytes.Buffer
		zw := zlib.NewWriter(&deflated)
		if _, err := zw.Write(body); err != nil {
			return fmt.Errorf("deflate: %w", err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("deflate: %w", err)
		}
		inner = mcproto.AppendVarInt(nil, int32(len(body)))
		inner = append(inner, deflated.Bytes()...)
	} else {
		inner = mcproto.AppendVarInt(nil, 0)
		inner = append(inner, body...)
	}

	var frame []byte
	frame = mcproto.AppendVarInt(frame, int32(len(inner)))
	frame = append(frame, inner...)
	_, err := c.rw.Write(frame)
	return err
}

// readPacket reads one packet in whichever framing is in force.
func (c *framedConn) readPacket() (int32, []byte, error) {
	if !c.compressed() {
		return mcproto.ReadPacket(c.rw)
	}

	length, err := mcproto.ReadVarInt(mcproto.ByteReader(c.rw))
	if err != nil {
		return 0, nil, fmt.Errorf("read packet length: %w", err)
	}
	if length <= 0 || length > maxPacketLen {
		return 0, nil, fmt.Errorf("read packet length: implausible value %d", length)
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(c.rw, frame); err != nil {
		return 0, nil, fmt.Errorf("read packet body: %w", err)
	}

	rest := bytes.NewReader(frame)
	dataLen, err := mcproto.ReadVarInt(rest)
	if err != nil {
		return 0, nil, fmt.Errorf("read data length: %w", err)
	}
	var body *bytes.Reader
	switch {
	case dataLen == 0:
		body = rest
	case dataLen < 0 || dataLen > maxPacketLen:
		return 0, nil, fmt.Errorf("read data length: implausible value %d", dataLen)
	default:
		zr, err := zlib.NewReader(rest)
		if err != nil {
			return 0, nil, fmt.Errorf("inflate: %w", err)
		}
		defer func() { _ = zr.Close() }()
		// dataLen is what the server says the packet inflates to, so it is
		// both the allocation and the bound: reading one byte more than that
		// means the stream disagrees with its own header.
		plain := make([]byte, dataLen)
		if _, err := io.ReadFull(zr, plain); err != nil {
			return 0, nil, fmt.Errorf("inflate: %w", err)
		}
		body = bytes.NewReader(plain)
	}

	id, err := mcproto.ReadVarInt(body)
	if err != nil {
		return 0, nil, fmt.Errorf("read packet id: %w", err)
	}
	payload, _ := io.ReadAll(body)
	return id, payload, nil
}

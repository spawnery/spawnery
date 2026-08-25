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
// backend. It is the automated half of milestone 3's success criterion — a
// player can join — and it exists because the only other way to make that
// claim is a human with a Microsoft account.
//
// # How far a login has to get, measured
//
// Velocity does not dial a backend when the login completes. It dials it when
// the client answers Login Success with Login Acknowledged, and it needs
// nothing further from the client: no FinishConfiguration, no
// AcknowledgeFinishConfiguration. Join therefore stops one packet after Login
// Success.
//
// Measured on 2026-08-11 against the pinned image
// ghcr.io/spawnery/velocity:3.5.1-0.2.0 (Velocity 3.5.1 build 615) with no
// operator endpoint, so the agent stayed dormant and the only routing was a
// static [servers] entry pointing at 127.0.0.1:25566, where nothing listens.
//
// The proxy had to be in offline mode, and it still does: this client
// authenticates against nothing, so an online-mode proxy answers it with an
// encryption request it cannot satisfy. What has changed is how that is said.
// Set spec.config.onlineMode: false on the ProxyGroup — the CRD field, added
// in 14331b2, which render.Velocity carries into velocity.toml and which no
// configOverlay can reach, because the renderer reasserts the keys it owns
// after merging one. That is the whole precondition for using this package
// against a Spawnery-managed proxy.
//
// The measurements below were taken before that field existed, on a rig that
// ran the real spawnery-config and then rewrote online-mode in
// /data/velocity.toml by hand before starting the JVM. The hand rewrite is
// gone; nothing in the packet exchange depends on how the file got its value,
// so the measurements stand as written.
//
// A client that sent handshake and Login Start, received Set Compression and
// Login Success, and then held the socket open for eight seconds without
// answering produced exactly one line and no backend attempt at all:
//
//	[18:13:53 INFO]: [connected player] step1probe (/10.1.90.12:60882) has connected
//	[18:14:01 INFO]: [connected player] step1probe (/10.1.90.12:60882) has disconnected
//
// Note the first line: "has connected" is logged before any backend exists, so
// it is not evidence of routing. The same client, differing only in that it
// sent Login Acknowledged, produced the dial immediately:
//
//	[18:14:19 INFO]: [connected player] step1probe (/10.1.90.12:56024) has connected
//	[18:14:19 ERROR]: [connected player] step1probe (/10.1.90.12:56024): unable to connect to server unroutable
//	io.netty.channel.AbstractChannel$AnnotatedConnectException: finishConnect(..) failed with error(-111): Connection refused: /127.0.0.1:25566
//	[18:14:19 INFO]: [connected player] step1probe (/10.1.90.12:56024) has disconnected: Unable to connect you to unroutable. Please try again later.
//
// # Why Join reads one more packet after that
//
// The second measurement above also shows how a routing failure reaches the
// client: as a configuration-state Disconnect, after the login has already
// succeeded. A client that returned the moment it had sent Login Acknowledged
// would report success for a proxy that could not route it anywhere, which is
// the one failure this tool exists to catch. So Join waits for the next
// packet: a configuration-state Disconnect is an error, anything else is the
// proxy forwarding the backend's configuration and therefore proof of a
// backend.
//
// That the success path really does deliver such a packet, and promptly, was
// measured the same way, with the pinned Paper image behind the same proxy on
// a container network — and with a second hand edit, since fixed. render.Paper
// wrote the forwarding secret under proxies.velocity.secret-key while Paper
// 26.2 reads proxies.velocity.secret: the rendered node failed to bind, came
// back as enabled: false with an empty secret, and the proxy refused the
// player with "Your server did not send a forwarding request to the proxy".
// The backend in this rig had that node corrected by hand because the
// measurement predates 494fa47, which made render.Paper write the key Paper
// reads. A Spawnery-rendered backend now accepts a forwarded player as
// rendered, and two guards keep it that way:
// TestPaperWritesTheKeysPaperItselfReads checks the renderer's key names
// against a fixture of Paper's own defaults, and hack/image-test.sh reads the
// file back out of a running Paper.
//
// The packet after Login Acknowledged arrived at once and was a
// configuration-state Plugin Message carrying minecraft:brand — whose value
// says where it came from:
//
//	packet id=0x01 hex=0f6d696e6563726166743a6272616e64105061706572202856656c6f63697479 29
//	                    m i n e c r a f t : b r a n d          Paper (Velocity)
//
//	[18:28:54 INFO]: [server connection] step1probe -> lobby has connected
//	[18:28:54 INFO]: UUID of player step1probe is b91758c8-e99d-34a7-aea0-b6e54ca3b36b   (Paper's log)
//
// The two disconnects are not the same packet and are not even the same
// encoding, which is why they are handled separately below. In login state it
// is packet 0x00 carrying a length-prefixed JSON string; measured:
//
//	{"translate":"multiplayer.disconnect.outdated_client","with":[{"text":"1.7.2-26.2"}]}
//
// In configuration state it is packet 0x02 carrying network NBT — a nameless
// root compound; measured, with the tag bytes shown as escapes:
//
//	\x0a \x08\x00\x05 color \x00\x03 red \x08\x00\x04 text \x00\x3c Unable to connect you to unroutable. Please try again later. \x00
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
// Measured, because the obvious thing does not work. Against the pinned pair
// — Velocity 3.5.1 in front of Paper 26.2 — slp.Ping's own handshake version
// of 771 comes back unchanged, because Velocity answers a status request with
// the asker's version whenever it supports it rather than with one of its
// own. Logging in as 771 then gets as far as the backend and no further:
//
//	[ERROR]: [connected player] step1probe: disconnected while connecting to lobby: Outdated client! Please use 26.2
//
// Announcing -1 instead is answered "776", which is Velocity's own maximum
// and Paper 26.2's protocol, and that login reaches the backend. 0 and
// 2147483647 were measured to work identically; -1 is what a client with no
// version to declare conventionally sends.
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
// which is the part a reading of the protocol gets wrong. Measured on the
// wire 2026-08-25 against Paper 26.2, protocol 776: after Login Acknowledged
// the server sends Plugin Message 0x01 (minecraft:brand), Feature Flags 0x0c
// and Select Known Packs 0x0e -- and then nothing but Keep Alive 0x04, for as
// long as the client waits. It never sends Finish Configuration unprompted,
// so a case that answers one packet waits for a packet that never comes. What
// moves it, each step confirmed by the server's own next move:
//
//   - serverbound Known Packs 0x07 with an empty list, after which the server
//     sends 29 Registry Data 0x07 packets and Update Tags 0x0d, 35 KB of them
//   - clientbound Finish Configuration 0x03, empty payload
//   - serverbound Acknowledge Finish Configuration 0x03, empty -- after which
//     the server logs "<name> joined the game" and counts the player
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
// That payload is network NBT, not JSON — see the package comment for the
// measured bytes — and its text is carried in string tags whose lengths and
// tag bytes are binary. Rather than carry an NBT decoder for what is only a
// diagnostic message, this keeps the printable runs of three characters or
// more and joins them with a space, which on the measured failure yields
//
//	color red text Unable to connect you to unroutable. Please try again later.
//
// The field names survive alongside their values, which is untidy and honest:
// this is the payload rendered legibly, not a component tree resolved.
//
// The one place it does more than that is the length prefix. An NBT string is
// two big-endian length bytes and then its content, and a message of 32 to
// 126 characters has a low length byte that is itself printable and would be
// carried into the message as a leading character — the measured reason is 60
// bytes long, so it read as "<Unable to connect you to...". A run whose first
// byte is exactly the length of what follows it is that prefix, so it is
// dropped.
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

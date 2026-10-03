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

// Package mcjoin logs in to a Minecraft proxy far enough to be routed to a
// backend, so that "a player can join" can be claimed without a Microsoft
// account.
//
// Velocity dials a backend when the client answers Login Success with Login
// Acknowledged, not when the login completes; its "has connected" line alone is
// not evidence of routing. A routing failure reaches the client only
// afterwards, as a configuration-state Disconnect, so Join reads one packet
// past the acknowledgement: that Disconnect is an error, anything else is the
// backend's configuration forwarded.
//
// The two disconnects differ: in login state, packet 0x00 with a
// length-prefixed JSON string; in configuration state, packet 0x02 with network
// NBT and a nameless root compound.
//
// The proxy has to be in offline mode. Set spec.config.onlineMode: false on the
// ProxyGroup; no configOverlay can reach that key.
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

// Packet ids. The login and configuration ids are the same across every
// protocol version Velocity 3.5.1 accepts.
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

	// From 1.20.5 on. Login: clientbound request, serverbound response.
	idLoginCookieRequest  = 0x05
	idLoginCookieResponse = 0x04
	// Configuration: clientbound request, serverbound response.
	idConfigurationCookieRequest  = 0x00
	idConfigurationCookieResponse = 0x01
)

// Configuration state from 1.20.5 on, used only when following transfers:
// clientbound Known Packs is postEffectsProtocol's id.
const (
	idConfigurationKnownPacks       = 0x0f
	idConfigurationKnownPacksAnswer = 0x07
	idConfigurationFinish           = 0x03
	idConfigurationFinishAnswer     = 0x03
)

// Play state, playProtocol only: these ids move with nearly every release.
const (
	playProtocol = 777

	idPlayDisconnect         = 0x20
	idPlayKeepAlive          = 0x2d
	idPlayKeepAliveAnswer    = 0x1c
	idPlayCookieRequest      = 0x15
	idPlayCookieResponse     = 0x15
	idPlayStoreCookie        = 0x7a
	idPlayTransfer           = 0x84
	idPlayStartConfiguration = 0x78
	idPlayAcknowledgeConfig  = 0x10
)

// cookieProtocol (1.20.5) introduced cookies and transfers. 26.3
// (postEffectsProtocol) moved the configuration-state Store Cookie and Transfer
// ids.
const (
	cookieProtocol      = 766
	postEffectsProtocol = 777
)

const (
	nextStateLogin    = 2
	nextStateTransfer = 3
)

// announceUnsupported is the protocol version announced when asking a server
// which version it speaks, deliberately one no server supports: Velocity
// answers a status request with the asker's own version whenever it supports
// it, while an unsupported one gets the proxy's own maximum.
//
// This relies on the proxy's newest version matching the backend's. When it
// does not, the backend refuses with "Outdated client!".
const announceUnsupported = -1

// maxPacketLen bounds a compressed packet's frame: the frame length is attacker
// controlled.
const maxPacketLen = 2 << 20

// maxInflatedLen is the vanilla client's bound on a decompressed packet;
// play-state chunk data can exceed maxPacketLen.
const maxInflatedLen = 8 << 20

var validUsername = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

// Result is what a completed login proves. The JSON tags are what
// cmd/spawnery-join prints for a runbook step to assert on with jq.
type Result struct {
	// Protocol is the version the server reported and this client then
	// announced.
	Protocol int `json:"protocol"`
	// Username and UUID are the server's own answer in Login Success, not the
	// values sent.
	Username string `json:"username"`
	UUID     string `json:"uuid"`
	// Compressed records whether the server switched on compression during
	// login. Velocity's default threshold is 256, so false against a real proxy
	// means the compressed framing was never exercised.
	Compressed bool `json:"compressed"`
	Transfers  int  `json:"transfers"`
}

type Options struct {
	Hold time.Duration
	// FollowTransfers holds the player in the play state, where a proxy counts
	// them as on a server and transfers them from, and reconnects where a
	// Transfer points until Hold is up. Without it a Transfer ends the hold
	// with an error. Play-state packet ids are known for playProtocol only.
	FollowTransfers bool
}

// Join connects, logs in as username against an offline-mode server, and
// returns once the server has shown that it can route the player somewhere.
//
// The deadline comes from ctx, applied to every connection; cancellation
// without a deadline is not observed.
func Join(ctx context.Context, host string, port int, username string) (*Result, error) {
	return JoinAndHold(ctx, host, port, username, 0)
}

// JoinAndHold is Join with the connection kept open for hold once the join has
// succeeded, so something outside this process can observe the player. A
// disconnect during the hold is an error.
//
// hold must fit inside ctx's deadline: a truncated hold and a completed one
// would both end in the same read timeout.
func JoinAndHold(ctx context.Context, host string, port int, username string, hold time.Duration) (*Result, error) {
	return JoinWith(ctx, host, port, username, Options{Hold: hold})
}

func JoinWith(ctx context.Context, host string, port int, username string, opts Options) (*Result, error) {
	hold := opts.Hold
	if hold < 0 {
		return nil, fmt.Errorf("a hold of %s is negative", hold)
	}
	if deadline, ok := ctx.Deadline(); ok && hold > 0 && !time.Now().Add(hold).Before(deadline) {
		return nil, fmt.Errorf("a hold of %s does not fit inside the %s left on the deadline",
			hold, time.Until(deadline).Round(time.Millisecond))
	}

	// Checked here because Velocity in offline mode accepts names Paper later
	// rejects, and the proxy's error then names neither the username nor the
	// character.
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
	if opts.FollowTransfers && protocol != playProtocol {
		return nil, fmt.Errorf("following transfers holds the player in the play state, whose packet ids this client knows for protocol %d only; the server reported %d",
			playProtocol, protocol)
	}

	s := newSession(protocol, username, opts)
	result := &Result{Protocol: protocol}
	nextState := int32(nextStateLogin)
	for {
		to, err := s.visit(ctx, host, port, nextState, result)
		if err != nil {
			if result.Transfers > 0 {
				return nil, fmt.Errorf("after %d transfers, at %s: %w",
					result.Transfers, net.JoinHostPort(host, strconv.Itoa(port)), err)
			}
			return nil, err
		}
		if to == nil {
			return result, nil
		}
		result.Transfers++
		host, port, nextState = to.host, to.port, nextStateTransfer
	}
}

type transfer struct {
	host string
	port int
}

// session is what outlives one connection: a transfer reconnects with the
// same protocol and identity, the cookies stored so far, and the end of the
// hold that began on the first connection.
type session struct {
	protocol int
	username string
	opts     Options
	cookies  map[string][]byte
	holdEnd  time.Time

	transfers     bool
	idStoreCookie int32
	idTransfer    int32
}

func newSession(protocol int, username string, opts Options) *session {
	s := &session{
		protocol:  protocol,
		username:  username,
		opts:      opts,
		cookies:   map[string][]byte{},
		transfers: protocol >= cookieProtocol,
	}
	if protocol >= postEffectsProtocol {
		s.idStoreCookie, s.idTransfer = 0x0b, 0x0c
	} else {
		s.idStoreCookie, s.idTransfer = 0x0a, 0x0b
	}
	return s
}

// visit is one connection: handshake, login, routing, and the hold. It
// returns where a followed Transfer points, or nil once the join or the hold
// is done.
func (s *session) visit(ctx context.Context, host string, port int, nextState int32, result *Result) (*transfer, error) {
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
	handshake = mcproto.AppendVarInt(handshake, int32(s.protocol))
	handshake = mcproto.AppendString(handshake, host)
	handshake = binary.BigEndian.AppendUint16(handshake, uint16(port))
	handshake = mcproto.AppendVarInt(handshake, nextState)
	if err := conn.writePacket(idHandshake, handshake); err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	// The offline-mode UUID Velocity and Paper compute; see OfflineUUID.
	offline := OfflineUUID(s.username)
	var loginStart []byte
	loginStart = mcproto.AppendString(loginStart, s.username)
	loginStart = append(loginStart, offline[:]...)
	if err := conn.writePacket(idLoginStart, loginStart); err != nil {
		return nil, fmt.Errorf("write login start: %w", err)
	}

	result.Compressed = false
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
			// A negative threshold turns compression back off.
			conn.threshold = int(threshold)
			result.Compressed = threshold >= 0
		case idLoginDisconnect:
			reason, err := readString(payload)
			if err != nil {
				return nil, fmt.Errorf("read disconnect reason: %w", err)
			}
			return nil, fmt.Errorf("the server refused the login: %s", reason)
		case idEncryptionRequest:
			return nil, errors.New("the server is in online mode and asked for encryption, which this client cannot answer")
		case idLoginPluginRequest:
			// Velocity sends these only towards backends. Unanswered, the
			// server would wait forever.
			return nil, errors.New("the server sent a login plugin request, which this client does not implement")
		case idLoginCookieRequest:
			if err := s.answerCookie(conn, idLoginCookieResponse, payload); err != nil {
				return nil, err
			}
		case idLoginSuccess:
			if err := readLoginSuccess(payload, result); err != nil {
				return nil, err
			}
			if err := conn.writePacket(idLoginAcknowledged, nil); err != nil {
				return nil, fmt.Errorf("write login acknowledged: %w", err)
			}
			// Login Acknowledged is what makes Velocity dial a backend; see the
			// package comment.
			id, payload, err := conn.awaitRouting()
			if err != nil {
				return nil, err
			}
			if s.opts.Hold == 0 {
				return nil, nil
			}
			if s.holdEnd.IsZero() {
				s.holdEnd = time.Now().Add(s.opts.Hold)
			}
			// The read timeout is how the hold ends; JoinWith checked that
			// holdEnd is within ctx's deadline.
			if err := c.SetDeadline(s.holdEnd); err != nil {
				return nil, fmt.Errorf("set hold deadline: %w", err)
			}
			return s.holdOpen(conn, id, payload)
		default:
			return nil, fmt.Errorf("unexpected packet id 0x%02x during login", id)
		}
	}
}

// answerCookie answers a Cookie Request from the cookies stored so far, with
// no payload for a key never stored.
func (s *session) answerCookie(c *framedConn, responseID int32, request []byte) error {
	key, err := readString(request)
	if err != nil {
		return fmt.Errorf("read a cookie request: %w", err)
	}
	answer := mcproto.AppendString(nil, key)
	if value, ok := s.cookies[key]; ok {
		answer = append(answer, 1)
		answer = mcproto.AppendVarInt(answer, int32(len(value)))
		answer = append(answer, value...)
	} else {
		answer = append(answer, 0)
	}
	if err := c.writePacket(responseID, answer); err != nil {
		return fmt.Errorf("answer the cookie request for %q: %w", key, err)
	}
	return nil
}

func (s *session) storeCookie(payload []byte) error {
	r := bytes.NewReader(payload)
	key, err := nextString(r)
	if err != nil {
		return fmt.Errorf("read a stored cookie's key: %w", err)
	}
	value, err := nextBytes(r)
	if err != nil {
		return fmt.Errorf("read the cookie stored as %q: %w", key, err)
	}
	s.cookies[key] = value
	return nil
}

func readTransfer(payload []byte) (*transfer, error) {
	r := bytes.NewReader(payload)
	host, err := nextString(r)
	if err != nil {
		return nil, fmt.Errorf("read a transfer's host: %w", err)
	}
	port, err := mcproto.ReadVarInt(r)
	if err != nil {
		return nil, fmt.Errorf("read a transfer's port: %w", err)
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("a transfer to %s names port %d", host, port)
	}
	return &transfer{host: host, port: int(port)}, nil
}

// holdOpen acts on first, the packet awaitRouting read, and then on every
// packet until the connection's deadline expires, which is how a successful
// hold ends, or until a Transfer is followed.
//
// Without FollowTransfers the client stays in the configuration state. Paper's
// getOnlinePlayers() never contains such a client, so Server.status.players
// reads zero, and Velocity never sets its current server, which it does only on
// Join Game.
//
// Reaching the play state takes the client driving the exchange: after Login
// Acknowledged the server sends brand, Feature Flags and Known Packs and then
// only Keep Alives. The client answers Known Packs with an empty list, the
// server sends registry data and Finish Configuration, and the client
// acknowledges it. In the play state a server switch sends Start Configuration,
// which leads back through the same exchange; other play packets are ignored.
func (s *session) holdOpen(c *framedConn, id int32, payload []byte) (*transfer, error) {
	play := false
	for {
		var to *transfer
		var err error
		if play {
			to, play, err = s.playPacket(c, id, payload)
		} else {
			to, play, err = s.configurationPacket(c, id, payload)
		}
		if err != nil || to != nil {
			return to, err
		}
		id, payload, err = c.readPacket()
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return nil, nil
			}
			return nil, fmt.Errorf("hold the connection open: %w", err)
		}
	}
}

// configurationPacket reports whether the client is now in the play state.
func (s *session) configurationPacket(c *framedConn, id int32, payload []byte) (*transfer, bool, error) {
	switch id {
	case idConfigurationDisconnect:
		return nil, false, fmt.Errorf("the player was disconnected during the hold: %s", nbtText(payload))
	case idConfigurationKeepAlive:
		// The payload is the id the server wants back; without the echo it
		// drops the connection.
		if err := c.writePacket(idConfigurationKeepAlive, payload); err != nil {
			return nil, false, fmt.Errorf("answer a keep alive: %w", err)
		}
		return nil, false, nil
	}
	if !s.transfers {
		return nil, false, nil
	}
	switch id {
	case idConfigurationCookieRequest:
		return nil, false, s.answerCookie(c, idConfigurationCookieResponse, payload)
	case s.idStoreCookie:
		return nil, false, s.storeCookie(payload)
	case s.idTransfer:
		to, err := s.transferTo(payload)
		return to, false, err
	}
	if !s.opts.FollowTransfers {
		return nil, false, nil
	}
	switch id {
	case idConfigurationKnownPacks:
		if err := c.writePacket(idConfigurationKnownPacksAnswer, mcproto.AppendVarInt(nil, 0)); err != nil {
			return nil, false, fmt.Errorf("answer known packs: %w", err)
		}
	case idConfigurationFinish:
		if err := c.writePacket(idConfigurationFinishAnswer, nil); err != nil {
			return nil, false, fmt.Errorf("acknowledge finish configuration: %w", err)
		}
		return nil, true, nil
	}
	return nil, false, nil
}

// playPacket reports whether the client is still in the play state.
func (s *session) playPacket(c *framedConn, id int32, payload []byte) (*transfer, bool, error) {
	switch id {
	case idPlayDisconnect:
		return nil, true, fmt.Errorf("the player was disconnected during the hold: %s", nbtText(payload))
	case idPlayKeepAlive:
		if err := c.writePacket(idPlayKeepAliveAnswer, payload); err != nil {
			return nil, true, fmt.Errorf("answer a keep alive: %w", err)
		}
	case idPlayCookieRequest:
		return nil, true, s.answerCookie(c, idPlayCookieResponse, payload)
	case idPlayStoreCookie:
		return nil, true, s.storeCookie(payload)
	case idPlayTransfer:
		to, err := s.transferTo(payload)
		return to, true, err
	case idPlayStartConfiguration:
		if err := c.writePacket(idPlayAcknowledgeConfig, nil); err != nil {
			return nil, true, fmt.Errorf("acknowledge configuration: %w", err)
		}
		return nil, false, nil
	}
	return nil, true, nil
}

func (s *session) transferTo(payload []byte) (*transfer, error) {
	to, err := readTransfer(payload)
	if err != nil {
		return nil, err
	}
	if !s.opts.FollowTransfers {
		return nil, fmt.Errorf("the player was transferred to %s during the hold, and following transfers is off",
			net.JoinHostPort(to.host, strconv.Itoa(to.port)))
	}
	return to, nil
}

// awaitRouting reads the one packet that follows Login Acknowledged and
// returns it for the hold to act on.
func (c *framedConn) awaitRouting() (int32, []byte, error) {
	id, payload, err := c.readPacket()
	if err != nil {
		return 0, nil, fmt.Errorf("read the proxy's answer to login acknowledged: %w", err)
	}
	if id == idConfigurationDisconnect {
		return 0, nil, fmt.Errorf("the login succeeded but the player was not routed: %s", nbtText(payload))
	}
	return id, payload, nil
}

// OfflineUUID is the UUID an offline-mode server assigns to username: the MD5
// name-based UUID of "OfflinePlayer:<username>", version 3, RFC 4122 variant.
func OfflineUUID(username string) [16]byte {
	sum := md5.Sum([]byte("OfflinePlayer:" + username))
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	return sum
}

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

// readLoginSuccess reads the UUID and username out of Login Success. The rest
// is deliberately not parsed: every field parsed is one that can break on a
// protocol bump.
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

func readString(b []byte) (string, error) {
	return nextString(bytes.NewReader(b))
}

func nextString(r *bytes.Reader) (string, error) {
	b, err := nextBytes(r)
	return string(b), err
}

func nextBytes(r *bytes.Reader) ([]byte, error) {
	length, err := mcproto.ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	if length < 0 || int(length) > r.Len() {
		return nil, fmt.Errorf("length %d exceeds the %d bytes that follow it", length, r.Len())
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// nbtText renders a configuration-state disconnect reason readably without an
// NBT decoder: it keeps printable runs of three bytes or more, joined by a
// space. A run whose first byte equals the length of the rest is an NBT string
// length prefix (two big-endian bytes, the low one printable for 32 to 126
// characters) and loses that byte.
func nbtText(payload []byte) string {
	var runs []string
	start := -1
	flush := func(end int) {
		if start >= 0 && end-start >= 3 {
			from := start
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

// framedConn is one connection. Compression is negotiated mid-login, so the
// framing is a field rather than a type.
type framedConn struct {
	rw io.ReadWriter
	// threshold is the compressed framing's minimum body size, or negative
	// while compression is off; zero means compress everything.
	threshold int
}

func (c *framedConn) compressed() bool { return c.threshold >= 0 }

func (c *framedConn) writePacket(id int32, payload []byte) error {
	if !c.compressed() {
		return mcproto.WritePacket(c.rw, id, payload)
	}

	var body []byte
	body = mcproto.AppendVarInt(body, id)
	body = append(body, payload...)

	// A body under the threshold is sent uncompressed with a data length of
	// zero; a server reading a data length below its own threshold treats the
	// connection as broken.
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
	case dataLen < 0 || dataLen > maxInflatedLen:
		return 0, nil, fmt.Errorf("read data length: implausible value %d", dataLen)
	default:
		zr, err := zlib.NewReader(rest)
		if err != nil {
			return 0, nil, fmt.Errorf("inflate: %w", err)
		}
		defer func() { _ = zr.Close() }()
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

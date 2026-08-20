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

	"github.com/spawnery/spawnery/internal/mcproto"
)

// handshakeProtocolVersion is the protocol version announced in the handshake.
// For a status request against a server it does not have to match: a Paper
// 26.2 server (protocol 776) answers a handshake announcing 771. This tool
// therefore never has to track Minecraft versions, which is measured, not
// hoped for — see section 7 of the design.
//
// A proxy is the exception, and it is why PingVersion exists: Velocity 3.5.1
// answers with the version the handshake announced whenever it supports it,
// so a caller that needs the version the proxy would really speak has to
// announce one it does not support. See PingVersion, and internal/mcjoin,
// which is that caller.
const handshakeProtocolVersion = 771

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
// version field. The deadline comes from ctx: after the dial, ctx.Deadline()
// is applied to the connection, but plain cancellation with no deadline set
// is not otherwise observed, so it will not abort an in-flight read or write.
func Ping(ctx context.Context, host string, port int) (*Status, error) {
	return PingVersion(ctx, host, port, handshakeProtocolVersion)
}

// PingVersion is Ping with the protocol version the handshake announces made
// explicit. The readiness probe has no use for it — every server answers a
// status request whatever it is told — but a client that has to log in
// afterwards does: Velocity echoes a version it supports back at the asker,
// so announcing 771 to the pinned proxy is answered "771" and the login that
// follows is then refused by a Paper 26.2 backend with "Outdated client!
// Please use 26.2". Announcing something no server supports is what makes the
// answer the proxy's own maximum instead. See internal/mcjoin for the
// measurement.
func PingVersion(ctx context.Context, host string, port int, protocol int32) (*Status, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set deadline: %w", err)
		}
	}

	if err := writeHandshake(conn, host, port, protocol); err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}
	// The status request itself is an empty packet 0x00.
	if err := mcproto.WritePacket(conn, 0x00, nil); err != nil {
		return nil, fmt.Errorf("write status request: %w", err)
	}

	return readStatusResponse(conn)
}

func writeHandshake(w io.Writer, host string, port int, protocol int32) error {
	var payload []byte
	payload = mcproto.AppendVarInt(payload, protocol)
	payload = mcproto.AppendString(payload, host)
	payload = binary.BigEndian.AppendUint16(payload, uint16(port))
	payload = mcproto.AppendVarInt(payload, 1) // next state: status
	return mcproto.WritePacket(w, 0x00, payload)
}

func readStatusResponse(r io.Reader) (*Status, error) {
	id, payload, err := mcproto.ReadPacket(r)
	if err != nil {
		return nil, err
	}
	if id != 0x00 {
		return nil, fmt.Errorf("unexpected packet id %d", id)
	}

	rest := bytes.NewReader(payload)

	docLen, err := mcproto.ReadVarInt(rest)
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

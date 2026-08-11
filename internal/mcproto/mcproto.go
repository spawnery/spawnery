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

// Package mcproto encodes and decodes the primitives shared by every part of
// the Minecraft protocol this project speaks: VarInts, length-prefixed
// strings, and the uncompressed packet framing used before compression is
// negotiated. internal/slp and the agent's join client both build on it, so
// the framing exists in exactly one place.
package mcproto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// maxPacketLen bounds what ReadPacket is willing to allocate for one packet.
// A status document or a login packet is at most a few hundred bytes;
// anything near this is a broken or hostile peer, and reading it would be
// the only unbounded allocation here.
const maxPacketLen = 2 << 20

// AppendVarInt appends v to b in the Minecraft VarInt encoding.
func AppendVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			return append(b, byte(u))
		}
		b = append(b, byte(u&0x7F)|0x80)
		u >>= 7
	}
}

// AppendString appends s to b as a VarInt length followed by its bytes.
func AppendString(b []byte, s string) []byte {
	b = AppendVarInt(b, int32(len(s)))
	return append(b, s...)
}

// ReadVarInt reads one Minecraft VarInt from r.
func ReadVarInt(r io.ByteReader) (int32, error) {
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

// WritePacket frames one uncompressed packet. Compression is negotiated
// during login; callers that have not reached that point yet can rely on
// this simple framing.
func WritePacket(w io.Writer, id int32, payload []byte) error {
	var body []byte
	body = AppendVarInt(body, id)
	body = append(body, payload...)

	var frame []byte
	frame = AppendVarInt(frame, int32(len(body)))
	frame = append(frame, body...)

	_, err := w.Write(frame)
	return err
}

// ReadPacket reads one uncompressed packet: a VarInt length, then a VarInt
// packet id, then the rest. The length bounds the read, so a server that
// announces more than it sends fails here rather than blocking forever.
func ReadPacket(r io.Reader) (int32, []byte, error) {
	length, err := ReadVarInt(ByteReader(r))
	if err != nil {
		return 0, nil, fmt.Errorf("read packet length: %w", err)
	}
	if length <= 0 || length > maxPacketLen {
		return 0, nil, fmt.Errorf("read packet length: implausible value %d", length)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, fmt.Errorf("read packet body: %w", err)
	}

	rest := bytes.NewReader(body)
	id, err := ReadVarInt(rest)
	if err != nil {
		return 0, nil, fmt.Errorf("read packet id: %w", err)
	}
	// rest is a bytes.Reader over an in-memory slice, so ReadAll cannot fail.
	payload, _ := io.ReadAll(rest)
	return id, payload, nil
}

// ByteReader adapts a plain io.Reader to io.ByteReader without buffering.
// Buffering would be wrong here: a length prefix is often followed by
// exactly that many bytes, and a bufio.Reader could swallow part of them
// into its buffer where io.ReadFull on the underlying connection would not
// find them.
func ByteReader(r io.Reader) io.ByteReader {
	return &byteReader{r: r}
}

type byteReader struct {
	r   io.Reader
	buf [1]byte
}

func (s *byteReader) ReadByte() (byte, error) {
	if _, err := io.ReadFull(s.r, s.buf[:]); err != nil {
		return 0, err
	}
	return s.buf[0], nil
}

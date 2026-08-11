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

package mcproto

import (
	"bytes"
	"strings"
	"testing"
)

func TestVarIntRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, 127, 128, 255, 2097151, 2147483647, -1} {
		encoded := AppendVarInt(nil, v)
		decoded, err := ReadVarInt(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("ReadVarInt(%d): %v", v, err)
		}
		if decoded != v {
			t.Errorf("round trip of %d gave %d", v, decoded)
		}
	}
}

func TestReadVarIntRejectsAnOverlongEncoding(t *testing.T) {
	// Five bytes, each with the continuation bit set: a well-behaved encoder
	// never emits this, since 0 fits in one byte. A loop that does not cap
	// the byte count would happily keep reading and decode it.
	overlong := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x00}

	_, err := ReadVarInt(bytes.NewReader(overlong))
	if err == nil {
		t.Fatal("ReadVarInt succeeded on an overlong encoding, want an error")
	}
	if !strings.Contains(err.Error(), "varint longer than five bytes") {
		t.Errorf("error is %q, want it to contain %q", err, "varint longer than five bytes")
	}
}

func TestPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePacket(&buf, 0x02, []byte("hello")); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	id, payload, err := ReadPacket(&buf)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if id != 0x02 {
		t.Errorf("id is %d, want 2", id)
	}
	if string(payload) != "hello" {
		t.Errorf("payload is %q, want %q", payload, "hello")
	}
}

func TestReadPacketRejectsAnAbsurdLength(t *testing.T) {
	var buf bytes.Buffer
	// Announce a length just over the 2 MiB cap. No body follows: a correct
	// implementation must reject the length before trying to read one.
	buf.Write(AppendVarInt(nil, maxPacketLen+1))

	_, _, err := ReadPacket(&buf)
	if err == nil {
		t.Fatal("ReadPacket succeeded with an absurd length, want an error")
	}
	if !strings.Contains(err.Error(), "read packet length") {
		t.Errorf("error is %q, want it to contain %q", err, "read packet length")
	}
}

func TestReadPacketFailsOnATruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	// Announce a length of 10 but supply only 2 bytes.
	buf.Write(AppendVarInt(nil, 10))
	buf.Write([]byte{0x00, 0x01})

	_, _, err := ReadPacket(&buf)
	if err == nil {
		t.Fatal("ReadPacket succeeded on a truncated body, want an error")
	}
	if !strings.Contains(err.Error(), "read packet body") {
		t.Errorf("error is %q, want it to contain %q", err, "read packet body")
	}
}

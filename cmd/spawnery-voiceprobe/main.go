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

// Command spawnery-voiceprobe sends one Simple Voice Chat frame for a player
// and checks that it comes back from spawnery-stubop's --voice-echo through a
// proxy's voice relay. Test-only; hack/agent-test.sh runs it on the host.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const (
	magic   = 0xFF
	payload = "probe"
	prefix  = "echo:"
)

type result struct {
	OK    bool   `json:"ok"`
	RTT   int64  `json:"rtt_ms"`
	Error string `json:"error"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spawnery-voiceprobe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "host:port of the voice relay (required)")
	player := fs.String("uuid", "", "the player the frame is for (required)")
	timeout := fs.Duration("timeout", 5*time.Second, "how long to wait for the echo")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *to == "" || *player == "" {
		_, _ = fmt.Fprintln(stderr, "--to and --uuid are required")
		return 1
	}
	id, err := parseUUID(*player)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "--uuid: %v\n", err)
		return 1
	}

	frame := append(append([]byte{magic}, id...), payload...)
	rtt, err := probe(*to, frame, *timeout)
	r := result{OK: err == nil, RTT: rtt.Milliseconds()}
	if err != nil {
		r.Error = err.Error()
	}
	line, _ := json.Marshal(r)
	_, _ = fmt.Fprintln(stdout, string(line))
	if !r.OK {
		return 1
	}
	return 0
}

func probe(to string, frame []byte, timeout time.Duration) (time.Duration, error) {
	conn, err := net.Dial("udp", to)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}

	start := time.Now()
	if _, err := conn.Write(frame); err != nil {
		return 0, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	rtt := time.Since(start)
	if err != nil {
		return rtt, err
	}
	if want := append([]byte(prefix), frame...); !bytes.Equal(buf[:n], want) {
		return rtt, fmt.Errorf("the reply %q is not %q followed by the frame", buf[:n], prefix)
	}
	return rtt, nil
}

func parseUUID(s string) ([]byte, error) {
	id, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil {
		return nil, err
	}
	if len(id) != 16 {
		return nil, errors.New("not 16 bytes")
	}
	return id, nil
}

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

// Command spawnery-join logs in to a Minecraft proxy far enough to be routed
// to a backend, and turns the result into an exit code and one line of JSON.
// It is the automated half of milestone 3's success criterion, run from a
// developer machine or a runbook against a NodePort:
//
//	spawnery-join --host 192.168.1.10 --port 30565
//
// It is test-only: no image carries it, and it is of no use against an
// online-mode proxy, which asks for an encryption handshake this client has
// no Microsoft account to answer.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spawnery/spawnery/internal/mcjoin"
)

const (
	defaultHost     = "127.0.0.1"
	defaultPort     = 25565
	defaultUsername = "spawnery_probe"

	// defaultTimeout covers a proxy that has to dial a backend, and a backend
	// that may be answering its first player. Nothing kills this process from
	// outside, unlike spawnery-slp's readiness probe, so it is generous.
	defaultTimeout = 30 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the process exit code: 0 once the login succeeded and the proxy
// showed it could route the player, 1 when it did not, 2 on a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spawnery-join", flag.ContinueOnError)
	fs.SetOutput(stderr)

	host := fs.String("host", defaultHost, "host to join")
	port := fs.Int("port", defaultPort, "TCP port to join")
	username := fs.String("username", defaultUsername, "username to log in as")
	timeout := fs.Duration("timeout", defaultTimeout, "deadline for the whole join")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := mcjoin.Join(ctx, *host, *port, *username)
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-join: %v\n", err)
		return 1
	}

	// One line, so a runbook step can pipe it into jq and assert on a field
	// rather than on prose.
	line, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-join: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", line)
	return 0
}

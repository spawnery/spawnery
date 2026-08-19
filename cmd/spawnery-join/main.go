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
// no Microsoft account to answer. So the proxy it is pointed at needs
// spec.config.onlineMode: false on its ProxyGroup — that field, and not a
// configOverlay: internal/render reasserts the keys it owns after merging an
// overlay, so the custom resource is the only place online-mode can be moved
// from. Nothing else about the proxy has to be special, and nothing has to be
// edited by hand.
//
// --hold keeps the connection open after a successful join, which is the only
// way a proxy's status.connectedPlayers can be non-zero when the next line of
// a runbook reads it.
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
	hold := fs.Duration("hold", 0, "how long to stay connected after a successful join")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Caught here rather than in mcjoin, where it is also caught, because
	// this one can name the two flags that disagree. Both have defaults, so
	// asking for a hold at all is what usually produces it.
	if *hold >= *timeout {
		_, _ = fmt.Fprintf(stderr, "spawnery-join: --hold %s does not fit inside --timeout %s\n", *hold, *timeout)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := mcjoin.JoinAndHold(ctx, *host, *port, *username, *hold)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-join: %v\n", err)
		return 1
	}

	// One line, so a runbook step can pipe it into jq and assert on a field
	// rather than on prose.
	line, err := json.Marshal(result)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-join: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", line)
	return 0
}

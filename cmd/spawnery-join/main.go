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

// Command spawnery-join logs in to a Minecraft proxy far enough to be routed
// to a backend, and turns the result into an exit code and one line of JSON:
//
//	spawnery-join --host 192.168.1.10 --port 30565
//
// It is test-only and needs a proxy with spec.config.onlineMode: false, since
// it has no Microsoft account for the encryption handshake.
//
// --hold keeps the connection open after a successful join.
// --follow-transfers lets that hold survive a proxy that transfers the player
// away; "transfers" in the output counts how often.
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

	// Generous: a backend may be answering its first player.
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
	follow := fs.Bool("follow-transfers", false, "during --hold, reconnect where a Transfer points, as a vanilla client does, instead of failing")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	// mcjoin catches this too, but cannot name the flags.
	if *hold >= *timeout {
		_, _ = fmt.Fprintf(stderr, "spawnery-join: --hold %s does not fit inside --timeout %s\n", *hold, *timeout)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := mcjoin.JoinWith(ctx, *host, *port, *username, mcjoin.Options{Hold: *hold, FollowTransfers: *follow})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-join: %v\n", err)
		return 1
	}

	line, err := json.Marshal(result)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-join: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", line)
	return 0
}

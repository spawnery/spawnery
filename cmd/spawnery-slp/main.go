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

// Command spawnery-slp performs one Minecraft server list ping and turns the
// result into an exit code. It is the exec readiness probe of the Paper base
// image; internal/podspec invokes it as
//
//	spawnery-slp --host 127.0.0.1 --port 25565
//
// and passes nothing else, so every other flag needs a working default.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spawnery/spawnery/internal/slp"
)

const (
	defaultHost = "127.0.0.1"
	defaultPort = 25565

	// defaultTimeout sits below the probe's TimeoutSeconds of 5 on purpose. If
	// ours fired later, the kubelet would kill the process and the log would
	// say nothing about why the server did not answer.
	defaultTimeout = 4 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run returns the process exit code: 0 when the server answered, 1 when it did
// not, 2 on a usage error.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("spawnery-slp", flag.ContinueOnError)
	fs.SetOutput(stderr)

	host := fs.String("host", defaultHost, "host to ping")
	port := fs.Int("port", defaultPort, "TCP port to ping")
	timeout := fs.Duration("timeout", defaultTimeout, "deadline for the whole ping")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if _, err := slp.Ping(ctx, *host, *port); err != nil {
		_, _ = fmt.Fprintf(stderr, "spawnery-slp: %v\n", err)
		return 1
	}
	return 0
}

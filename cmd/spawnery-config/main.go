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

// Command spawnery-config turns the operator's rendered configuration into
// the files Paper or Velocity actually read, before the JVM starts. It is
// baked into both images, and image/entrypoint.sh and
// image/velocity-entrypoint.sh each run it inside the main container, ahead
// of the exec that replaces the shell with the JVM — there is no init
// container anywhere in this repository. That placement is why it must exit
// non-zero, not just log, on anything that would otherwise let the JVM start
// against bad configuration: a failure here surfaces as this same
// container's CrashLoopBackOff, not as an Init:Error the kubelet would report
// separately.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spawnery/spawnery/internal/render"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run returns the process exit code: 0 once every rendered file is on disk,
// 1 when Load or the chosen flavour refuses, 2 on a usage error.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("spawnery-config", flag.ContinueOnError)
	fs.SetOutput(stderr)

	flavor := fs.String("flavor", "", `which server this renders for: "paper" or "velocity"`)
	configDir := fs.String("config-dir", render.ConfigDir, "directory the operator's rendered configuration is mounted at")
	out := fs.String("out", "/data", "directory to write the rendered files under")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *flavor != "paper" && *flavor != "velocity" {
		fmt.Fprintf(stderr, "spawnery-config: --flavor must be \"paper\" or \"velocity\", got %q\n", *flavor)
		return 2
	}

	values, secret, overlay, err := render.Load(*configDir)
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-config: %v\n", err)
		return 1
	}

	var files map[string][]byte
	switch *flavor {
	case "paper":
		// Paper has no file reference for the forwarding secret, so it is
		// the content that must reach paper-global.yml.
		files, err = render.Paper(values, secret, overlay)
	case "velocity":
		// Velocity's forwarding-secret-file points at the mount itself, so
		// it is the path — not the content Load also returns — that must
		// reach velocity.toml. See TestRunWiresTheSecretPathNotItsContentIntoVelocity.
		files, err = render.Velocity(values, filepath.Join(*configDir, render.SecretFile), overlay)
	}
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-config: %v\n", err)
		return 1
	}

	if err := render.WriteAll(*out, files); err != nil {
		fmt.Fprintf(stderr, "spawnery-config: %v\n", err)
		return 1
	}
	return 0
}

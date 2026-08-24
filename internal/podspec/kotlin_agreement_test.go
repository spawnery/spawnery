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

package podspec

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// TestTheReadyPortAgreesWithTheVelocityAgent closes the entry in
// docs/known-issues.md: "the ready port is spelled in two languages —
// internal/podspec.ProxyReadyPort and a Kotlin constant in AgentPlugin.kt —
// with no test that can compare them. Only the level-2 harness catches a
// divergence, and only when it runs."
//
// A Go test cannot import Kotlin, but it can read it. The same technique this
// repository already uses on cmd/spawnery-operator/main.go, where an AST pin
// asserts a wiring that no compiler checks either.
//
// What a divergence costs is why this is worth a source-reading test rather
// than a comment asking people to be careful: podspec puts the port on the pod
// as the kubelet's readiness probe target, and the agent binds it. Move one and
// the probe dials a port nothing is listening on, so the pod never goes Ready,
// so the proxy never joins the Service — and nothing anywhere says the two
// numbers disagree. The only thing that catches it today is hack/agent-test.sh
// phase four, which is not on the commit loop.
func TestTheReadyPortAgreesWithTheVelocityAgent(t *testing.T) {
	const source = "agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt"
	raw, err := os.ReadFile(testenv.RepoPath(t, source))
	if err != nil {
		t.Fatalf("read the Velocity agent's source: %v", err)
	}

	// Anchored on the declaration rather than searching for the number, because
	// finding the number would make the test pass for the wrong reason: the
	// same digits appear in a comment about hack/velocity-image-test.sh a
	// hundred lines above.
	re := regexp.MustCompile(`(?m)^\s*const val READY_PORT\s*=\s*(\d+)\s*$`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no `const val READY_PORT = <number>` in %s. Either it was renamed — in "+
			"which case this test has to follow it, since nothing else compares the two "+
			"languages — or it is gone, and podspec is putting a probe on a port nothing "+
			"binds", source)
	}
	got, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("READY_PORT = %q, which is not a number: %v", m[1], err)
	}
	if int32(got) != ProxyReadyPort {
		t.Errorf("%s declares READY_PORT = %d, podspec.ProxyReadyPort = %d.\n"+
			"podspec puts this port on the pod as the kubelet's readiness probe target "+
			"and the agent binds it. Disagreeing means the probe dials a port nothing is "+
			"listening on: the pod never goes Ready, the proxy never joins the Service, "+
			"and nothing reports why.", source, got, ProxyReadyPort)
	}
}

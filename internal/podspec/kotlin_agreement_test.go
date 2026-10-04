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

package podspec

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// A divergence makes the readiness probe dial a port nothing listens on, so
// the proxy never goes Ready, and only hack/agent-test.sh would notice.
func TestTheReadyPortAgreesWithTheVelocityAgent(t *testing.T) {
	const source = "agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt"
	raw, err := os.ReadFile(testenv.RepoPath(t, source))
	if err != nil {
		t.Fatalf("read the Velocity agent's source: %v", err)
	}

	// Anchored on the declaration: the same digits appear in a comment in the
	// Kotlin file.
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

func TestTheTransferEnvNamesAgreeWithTheVelocityAgent(t *testing.T) {
	const source = "agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ProxyEnvironment.kt"
	raw, err := os.ReadFile(testenv.RepoPath(t, source))
	if err != nil {
		t.Fatalf("read the Velocity agent's source: %v", err)
	}

	for constant, want := range map[string]string{
		"TRANSFER_FORCE_AFTER_SECONDS": EnvTransferForceAfterSeconds,
		"FORWARDING_SECRET_FILE":       EnvForwardingSecretFile,
		"TRANSFER_FORCE_GROUPS":        EnvTransferForceGroups,
	} {
		re := regexp.MustCompile(`(?m)^\s*const val ` + constant + `\s*=\s*"([^"]*)"\s*$`)
		m := re.FindSubmatch(raw)
		if m == nil {
			t.Errorf("no `const val %s = \"...\"` in %s", constant, source)
			continue
		}
		if got := string(m[1]); got != want {
			t.Errorf("%s declares %s = %q, podspec sets %q: the agent would never see the "+
				"variable and the group would drain without transferring", source, constant, got, want)
		}
	}
}

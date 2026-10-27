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
	"path"
	"regexp"
	"sort"
	"strconv"
	"testing"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/prune"
	"github.com/spawnery/spawnery/internal/render"
	"github.com/spawnery/spawnery/internal/testenv"
	"github.com/spawnery/spawnery/internal/voice"
	"github.com/spawnery/spawnery/internal/worldsync"
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

func TestTheWorldSyncNamesAgreeWithThePaperAgent(t *testing.T) {
	const dir = "agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/"
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile(testenv.RepoPath(t, dir+name))
		if err != nil {
			t.Fatalf("read the Paper agent's source: %v", err)
		}
		return raw
	}
	worldSync := read("WorldSync.kt")
	plugin := read("AgentPlugin.kt")

	for constant, want := range map[string]string{
		"ENV_ENABLED":  EnvWorldSync,
		"ENV_INTERVAL": EnvWorldSyncInterval,
		"CONTROL_DIR":  prune.ControlDir,
		"READY_FILE":   path.Base(worldsync.ReadyFile),
		"FAILED_FILE":  path.Base(worldsync.FailedFile),
		"REQUEST_FILE": path.Base(worldsync.RequestFile),
		"DONE_FILE":    path.Base(worldsync.DoneFile),
	} {
		re := regexp.MustCompile(`(?m)^\s*const val ` + constant + `\s*=\s*"([^"]*)"\s*$`)
		m := re.FindSubmatch(worldSync)
		if m == nil {
			t.Errorf("no `const val %s = \"...\"` in WorldSync.kt", constant)
			continue
		}
		if got := string(m[1]); got != want {
			t.Errorf("WorldSync.kt declares %s = %q, the Go side says %q: the agent would wait "+
				"on, or snapshot into, something the node agent never writes or reads",
				constant, got, want)
		}
	}
	for _, use := range []string{"WorldSync.ENV_ENABLED", "WorldSync.ENV_INTERVAL", "WorldSync.CONTROL_DIR"} {
		if !regexp.MustCompile(regexp.QuoteMeta(use)).Match(plugin) {
			t.Errorf("AgentPlugin.kt no longer reads %s, so the constant this test checks is unused", use)
		}
	}
}

const voiceKotlinDir = "agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/"

func readVoiceSource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(testenv.RepoPath(t, voiceKotlinDir+name))
	if err != nil {
		t.Fatalf("read the Velocity agent's source: %v", err)
	}
	return string(raw)
}

func TestTheVoiceEnvNamesAgreeWithTheVelocityAgent(t *testing.T) {
	plugin := readVoiceSource(t, "AgentPlugin.kt")
	for name, want := range map[string]string{
		"enable": EnvVoice,
		"host":   EnvHostIP,
	} {
		if !regexp.MustCompile(`System\.getenv\("` + regexp.QuoteMeta(want) + `"\)`).MatchString(plugin) {
			t.Errorf("AgentPlugin.kt does not read System.getenv(%q) (%s variable): podspec sets "+
				"it, the agent would never see it and voice would stay off or lose the node address",
				want, name)
		}
	}
}

func TestTheVoiceRelayPortAgreesWithTheVelocityAgent(t *testing.T) {
	control := readVoiceSource(t, "voice/VoiceControl.kt")
	m := regexp.MustCompile(`(?m)^\s*const val RELAY_PORT\s*=\s*(\d+)\s*$`).FindStringSubmatch(control)
	if m == nil {
		t.Fatalf("no `const val RELAY_PORT = <number>` in VoiceControl.kt")
	}
	got, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("RELAY_PORT = %q: %v", m[1], err)
	}
	if int32(got) != voice.RelayPort {
		t.Errorf("VoiceControl.kt binds RELAY_PORT = %d, voice.RelayPort = %d: the container port "+
			"and Service target would point at a port nothing listens on", got, voice.RelayPort)
	}
}

func TestTheVoiceCounterReasonsAgreeWithTheVelocityAgent(t *testing.T) {
	counters := readVoiceSource(t, "voice/VoiceCounters.kt")
	var got []string
	for _, m := range regexp.MustCompile(`(?m)^\s*const val [A-Z_]+\s*=\s*"([^"]*)"\s*$`).FindAllStringSubmatch(counters, -1) {
		got = append(got, m[1])
	}
	want := append([]string(nil), agent.VoiceReasons...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) == 0 || len(got) != len(want) {
		t.Fatalf("VoiceCounters.kt declares reasons %q, agent.VoiceReasons is %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("VoiceCounters.kt declares reasons %q, agent.VoiceReasons is %q: the operator "+
				"discards a reason it does not know, so the counter would silently stop", got, want)
		}
	}
}

func TestTheRendererReadsTheTransferVariableThePodCarries(t *testing.T) {
	if render.EnvTransfer != EnvTransferForceAfterSeconds {
		t.Errorf("render.EnvTransfer = %q, podspec sets %q", render.EnvTransfer, EnvTransferForceAfterSeconds)
	}
}

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

package agentserver

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/testenv"
)

const operatorChannelSource = "agent/common/src/main/kotlin/cloud/spawnery/agent/OperatorChannel.kt"

func kotlinSeconds(t *testing.T, name string) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(testenv.RepoPath(t, operatorChannelSource))
	if err != nil {
		t.Fatalf("read the agent's operator channel: %v", err)
	}
	re := regexp.MustCompile(name + ` = (\d+)L`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no `%s = <number>L` in %s. Either it was renamed, in which case "+
			"this test has to follow it, or the keepalive moved.", name, operatorChannelSource)
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return time.Duration(n) * time.Second
}

func TestTheAgentKeepaliveStaysAboveTheOperatorsMinimum(t *testing.T) {
	keepalive := kotlinSeconds(t, "KEEPALIVE_SECONDS")
	if keepalive <= MinKeepaliveInterval {
		t.Fatalf("KEEPALIVE_SECONDS in %s is %s, not above MinKeepaliveInterval (%s) in "+
			"internal/agentserver/server.go. A client pinging faster than the minimum "+
			"collects strikes and is sent a GOAWAY.",
			operatorChannelSource, keepalive, MinKeepaliveInterval)
	}
}

func TestADeadOperatorIsNoticedWithinTwentySeconds(t *testing.T) {
	keepalive := kotlinSeconds(t, "KEEPALIVE_SECONDS")
	timeout := kotlinSeconds(t, "KEEPALIVE_TIMEOUT_SECONDS")
	if keepalive+timeout > 20*time.Second {
		t.Fatalf("KEEPALIVE_SECONDS + KEEPALIVE_TIMEOUT_SECONDS in %s is %s, over 20s. "+
			"Spec §3.6: a standby takes over in about 17 s and phase.ReconnectGrace is "+
			"45 s, so an agent that needs longer to notice reconnects after the grace "+
			"has run out.", operatorChannelSource, keepalive+timeout)
	}
}

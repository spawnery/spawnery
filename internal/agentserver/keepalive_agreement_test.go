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
	"strings"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/testenv"
)

const (
	operatorChannelSource = "agent/common/src/main/kotlin/cloud/spawnery/agent/OperatorChannel.kt"
	sessionLoopSource     = "agent/common/src/main/kotlin/cloud/spawnery/agent/SessionLoop.kt"
)

func kotlinConstant(t *testing.T, source, name string) int {
	t.Helper()
	raw, err := os.ReadFile(testenv.RepoPath(t, source))
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	re := regexp.MustCompile(name + ` = ([\d_]+)L`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no `%s = <number>L` in %s. Either it was renamed, in which case "+
			"this test has to follow it, or it moved.", name, source)
	}
	n, err := strconv.Atoi(strings.ReplaceAll(string(m[1]), "_", ""))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return n
}

func kotlinSeconds(t *testing.T, name string) time.Duration {
	t.Helper()
	return time.Duration(kotlinConstant(t, operatorChannelSource, name)) * time.Second
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

// The pessimistic sum: an agent that notices the dead leader as late as its
// keepalive allows, and whose first dial still lands on the frozen pod, must
// be back before ReconnectGrace runs out after the fastest takeover measured.
func TestAReconnectToAFrozenLeaderEndsInsideTheGrace(t *testing.T) {
	notice := kotlinSeconds(t, "KEEPALIVE_SECONDS") + kotlinSeconds(t, "KEEPALIVE_TIMEOUT_SECONDS")
	bound := time.Duration(kotlinConstant(t, sessionLoopSource, "RECONNECT_ANSWER_BOUND_MILLIS")) * time.Millisecond
	// SessionLoop.backoffMillis(0) and (1), 1 s and 2 s, each with its 10 % jitter.
	const backoff = 1100*time.Millisecond + 2200*time.Millisecond
	// The node-loss e2e case measured takeovers of 15-19 s.
	const takeover = 15 * time.Second

	if back, deadline := notice+bound+backoff, takeover+phase.ReconnectGrace; back >= deadline {
		t.Fatalf("an agent can come back %s after a node loss: %s to notice (KEEPALIVE_SECONDS + "+
			"KEEPALIVE_TIMEOUT_SECONDS in %s), a first dial that kube-proxy still sends to the "+
			"frozen pod and that RECONNECT_ANSWER_BOUND_MILLIS in %s retires after %s, and %s "+
			"of backoff before and after it. ReconnectGrace (%s) runs from the takeover, %s at "+
			"the fastest measured, so the server leaves Ready at %s.",
			back, notice, operatorChannelSource, sessionLoopSource, bound, backoff,
			phase.ReconnectGrace, takeover, deadline)
	}
}

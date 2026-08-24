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

package rbacaudit_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// helmTemplate runs the chart with extra arguments and returns helm's combined
// output and whether it succeeded. Unlike renderChart it deliberately does not
// memoise: every case here is a different set of values, and the point is what
// helm decides about each.
func helmTemplate(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	full := append([]string{
		"template", "spawnery", testenv.RepoPath(t, "charts/spawnery"),
		"--namespace", renderNamespace,
	}, args...)
	cmd := exec.Command("helm", full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err == nil
}

// TestTheValuesSchemaRefusesWhatTheFlagParserUsedTo is what
// charts/spawnery/values.schema.json is for. docs/known-issues.md measured
// three ways to be wrong, and all three reached a cluster as a container that
// exited at startup — CrashLoopBackOff, with the operator's flag parser
// naming a flag rather than helm naming a value.
//
// Every case here asserts two things, and the second is the one worth having:
// that helm refuses, and that its message names the key. A refusal that does
// not say which of a dozen values is wrong has moved the problem earlier
// without making it easier.
func TestTheValuesSchemaRefusesWhatTheFlagParserUsedTo(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		// names is what the refusal has to mention for somebody to act on it.
		names string
		why   string
	}{
		{
			name:  "a startup deadline with no unit",
			args:  []string{"--set-string", "operator.startupDeadline=30"},
			names: "startupDeadline",
			why:   "the container exited with `invalid value \"30\" for flag -startup-deadline`",
		},
		{
			name:  "a startup deadline that YAML reads as a number",
			args:  []string{"--set", "operator.startupDeadline=30"},
			names: "startupDeadline",
			why:   "unquoted 30 is a number, and the flag takes a duration string",
		},
		{
			name:  "leaderElect as a YAML string",
			args:  []string{"--set", "operator.leaderElect=yes"},
			names: "leaderElect",
			why:   "the container exited with `invalid boolean value \"yes\"`",
		},
		{
			name:  "neither a tag nor a digest",
			args:  []string{"--set", "image.tag=", "--set", "image.digest="},
			names: "tag",
			why: "helm's own error was a YAML parse failure inside deployment.yaml, " +
				"naming neither the value nor the key",
		},
		{
			name:  "a digest without its algorithm",
			args:  []string{"--set", "image.digest=4c2c28592d9ccf60ecede425d6503c58aa293c9ee565d43e30e09ebd90dde42e"},
			names: "digest",
			why:   "a bare hex string is the shape somebody reaches for first",
		},
		{
			name:  "a pull policy that is not one",
			args:  []string{"--set", "image.pullPolicy=Sometimes"},
			names: "pullPolicy",
			why:   "the API server refuses it, one apply later",
		},
		{
			name:  "a mistyped key",
			args:  []string{"--set", "operator.startupDeadlien=5m"},
			names: "startupDeadlien",
			why: "a typo in a --set key silently did nothing at all, and the operator " +
				"ran with the default the user thought they had overridden",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := helmTemplate(t, tc.args...)
			if ok {
				t.Fatalf("helm accepted it. %s", tc.why)
			}
			if !strings.Contains(out, tc.names) {
				t.Errorf("helm refused it without naming %q, so a reader cannot tell which "+
					"value to fix:\n%s", tc.names, out)
			}
		})
	}
}

// TestTheValuesSchemaAcceptsWhatIsActuallyUsed is the other half, and it is
// the half the known-issues entry gave as the reason for not writing a schema
// at all: "hack/e2e.sh passes four --set values that a schema written slightly
// wrong would break in a target nobody runs on the commit loop." Milestone 6e
// put e2e on the commit loop, which is what made this worth doing — but a
// schema that breaks the e2e install should fail here, in two seconds, rather
// than there, in forty minutes.
func TestTheValuesSchemaAcceptsWhatIsActuallyUsed(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"the defaults", nil},
		{
			// Copied from hack/e2e.sh's helm install, values changed but shapes
			// kept. If that script grows a fifth --set, this is where a schema
			// that has not heard about it says so.
			name: "what hack/e2e.sh sets",
			args: []string{
				"--set", "image.repository=localhost/spawnery-operator",
				"--set", "image.tag=e2e",
				"--set", "image.digest=",
				"--set", "image.pullPolicy=Never",
				"--set", "operator.startupDeadline=20s",
			},
		},
		{"a compound duration", []string{"--set", "operator.startupDeadline=1h30m"}},
		{"sub-second durations", []string{"--set", "operator.startupDeadline=1500ms"}},
		{
			name: "a digest instead of a tag",
			args: []string{
				"--set", "image.tag=",
				"--set", "image.digest=sha256:4c2c28592d9ccf60ecede425d6503c58aa293c9ee565d43e30e09ebd90dde42e",
			},
		},
		{
			name: "both monitoring objects on",
			args: []string{
				"--set", "metrics.serviceMonitor.enabled=true",
				"--set", "metrics.prometheusRule.enabled=true",
				"--set", "metrics.serviceMonitor.interval=1m",
				"--set", "metrics.prometheusRule.caExpiryWarningDays=30",
			},
		},
		{"the network policy off", []string{"--set", "networkPolicy.enabled=false"}},
		{
			name: "pass-through values the schema deliberately does not model",
			args: []string{
				"--set", "nodeSelector.kubernetes\\.io/os=linux",
				"--set", "tolerations[0].key=node-role.kubernetes.io/control-plane",
				"--set", "tolerations[0].operator=Exists",
				"--set", "resources.limits.cpu=500m",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out, ok := helmTemplate(t, tc.args...); !ok {
				t.Errorf("helm refused values that are meant to work:\n%s", out)
			}
		})
	}
}

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

package image

import (
	"os"
	"regexp"
	"strings"
	"testing"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// The JVM reads these itself whatever the entrypoint does; the entrypoint only
// looks at them to leave its own flags out of their way.
var jvmOwnVariables = map[string]bool{"JAVA_TOOL_OPTIONS": true, "JDK_JAVA_OPTIONS": true}

// A variable spec.env could set would let a group choose the jar that runs.
func TestEveryVariableTheEntrypointsReadIsReserved(t *testing.T) {
	// `${NAME:-default}` is the one form the scripts use to read their environment.
	read := regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*):-`)
	for _, script := range []string{"entrypoint.sh", "velocity-entrypoint.sh"} {
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		found := read.FindAllStringSubmatch(string(raw), -1)
		if len(found) == 0 {
			t.Fatalf("%s reads no environment variable at all; the pattern this test scans for has drifted", script)
		}
		for _, m := range found {
			if jvmOwnVariables[m[1]] {
				continue
			}
			if !strings.HasPrefix(m[1], spawneryv1alpha1.ReservedEnvPrefix) {
				t.Errorf("%s reads %s from its environment, which a group's spec.env may set; "+
					"the reservation only covers the %s prefix", script, m[1], spawneryv1alpha1.ReservedEnvPrefix)
			}
		}
	}
}

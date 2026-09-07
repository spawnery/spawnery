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

package image

import (
	"os"
	"regexp"
	"strings"
	"testing"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// Every variable an entrypoint reads from its environment has to carry the
// prefix a group's spec.env may not set. The CRD rule reserves SPAWNERY_ and
// the scripts used to read PAPER_HOME and VELOCITY_HOME beside it, which
// decide the jar that runs and where the operator's agent jar is taken from;
// a group could set either through spec.env and the reservation protected
// nothing.
func TestEveryVariableTheEntrypointsReadIsReserved(t *testing.T) {
	// `${NAME:-default}` is the one form the scripts use to read their
	// environment. Variables the script assigns itself and then expands are
	// not matched, and are not the concern.
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
			if !strings.HasPrefix(m[1], spawneryv1alpha1.ReservedEnvPrefix) {
				t.Errorf("%s reads %s from its environment, which a group's spec.env may set; "+
					"the reservation only covers the %s prefix", script, m[1], spawneryv1alpha1.ReservedEnvPrefix)
			}
		}
	}
}

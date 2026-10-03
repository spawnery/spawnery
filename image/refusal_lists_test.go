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

	"github.com/spawnery/spawnery/internal/render"
	"github.com/spawnery/spawnery/internal/testenv"
)

// A shell script cannot read a Go slice, so the entrypoints' refusal lists are
// typed out by hand; these tests hold them to the renderer's lists both ways.

// ownedLoop matches the scan over the renderer's own files. The directory
// refusals above it (plugins/, lang/) have no renderer list to drift against.
var ownedLoop = regexp.MustCompile(`(?m)^[ \t]*for owned in (.+); do[ \t]*$`)

// scriptRefusalList returns the paths an entrypoint's scan refuses. It fails
// on a missing or duplicated loop: an empty list would pass every assertion.
func scriptRefusalList(t *testing.T, repoScript string) []string {
	t.Helper()

	body, err := os.ReadFile(testenv.RepoPath(t, repoScript))
	if err != nil {
		t.Fatalf("read %s: %v", repoScript, err)
	}

	found := ownedLoop.FindAllStringSubmatch(string(body), -1)
	switch len(found) {
	case 1:
	case 0:
		t.Fatalf("%s has no `for owned in ...; do` loop.\n"+
			"Either the renderer-owned scan was removed -- in which case an extraFiles\n"+
			"claim can now overwrite the operator's own files -- or it was rewritten in a\n"+
			"shape this test cannot read, in which case teach %s the new shape rather\n"+
			"than deleting the check.",
			repoScript, "image/refusal_lists_test.go")
	default:
		t.Fatalf("%s has %d `for owned in ...; do` loops; this test reads one and would\n"+
			"silently ignore the rest. Fold them together or teach %s to read all of them.",
			repoScript, len(found), "image/refusal_lists_test.go")
	}

	list := strings.Fields(found[0][1])
	if len(list) == 0 {
		t.Fatalf("the `for owned in ...; do` loop in %s refuses nothing", repoScript)
	}
	return list
}

// assertNoRefusalDrift names the direction of a drift: the two directions are
// different bugs with different fixes.
func assertNoRefusalDrift(t *testing.T, repoScript, goListName string, goList []string) {
	t.Helper()

	script := scriptRefusalList(t, repoScript)

	inScript := make(map[string]bool, len(script))
	for _, f := range script {
		inScript[f] = true
	}
	inGo := make(map[string]bool, len(goList))
	for _, f := range goList {
		inGo[f] = true
	}

	var missingFromScript, missingFromGo []string
	for _, f := range goList {
		if !inScript[f] {
			missingFromScript = append(missingFromScript, f)
		}
	}
	for _, f := range script {
		if !inGo[f] {
			missingFromGo = append(missingFromGo, f)
		}
	}

	if len(missingFromScript) > 0 {
		t.Errorf("drift: %s names %s, and %s does not refuse it.\n"+
			"The renderer writes a file the entrypoint will now accept from an extraFiles\n"+
			"claim, copy into place, and then silently overwrite -- the administrator's\n"+
			"file disappears with nothing said. Add it to the `for owned in` list in %s.\n"+
			"  %s: %s\n"+
			"  %s: %s",
			goListName, strings.Join(missingFromScript, ", "), repoScript, repoScript,
			goListName, strings.Join(goList, " "),
			repoScript, strings.Join(script, " "))
	}
	if len(missingFromGo) > 0 {
		t.Errorf("drift: %s refuses %s, and %s does not name it.\n"+
			"The entrypoint is refusing a path no owner claims. A group whose claim\n"+
			"happens to carry that file crash-loops for a rule with no reason behind it.\n"+
			"Drop it from the `for owned in` list in %s, or restore it to %s.\n"+
			"  %s: %s\n"+
			"  %s: %s",
			repoScript, strings.Join(missingFromGo, ", "), goListName, repoScript, goListName,
			goListName, strings.Join(goList, " "),
			repoScript, strings.Join(script, " "))
	}
}

func TestThePaperEntrypointRefusesExactlyWhatTheRendererOwns(t *testing.T) {
	assertNoRefusalDrift(t, "image/entrypoint.sh", "render.PaperFiles", render.PaperFiles)
}

func TestTheVelocityEntrypointRefusesExactlyWhatTheRendererOwns(t *testing.T) {
	assertNoRefusalDrift(t, "image/velocity-entrypoint.sh", "render.VelocityFiles", render.VelocityFiles)
}

// TestTheTwoFlavoursRefusalListsAreNotTheSame catches a tidy-up that gives both
// scripts the union of the lists while leaving the Go lists apart.
func TestTheTwoFlavoursRefusalListsAreNotTheSame(t *testing.T) {
	paper := scriptRefusalList(t, "image/entrypoint.sh")
	velocity := scriptRefusalList(t, "image/velocity-entrypoint.sh")

	for _, tc := range []struct {
		flavour string
		list    []string
		refused string
	}{
		{"image/entrypoint.sh", paper, "velocity.toml"},
		{"image/velocity-entrypoint.sh", velocity, "server.properties"},
	} {
		for _, got := range tc.list {
			if got == tc.refused {
				t.Errorf("%s refuses %s, which no server of that flavour writes.\n"+
					"The refusal list follows the flavour: refusing a path no owner claims\n"+
					"crash-loops a group for nothing.", tc.flavour, tc.refused)
			}
		}
	}
}

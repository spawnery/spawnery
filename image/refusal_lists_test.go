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

	"github.com/spawnery/spawnery/internal/render"
	"github.com/spawnery/spawnery/internal/testenv"
)

// Design spec 3.3 says the entrypoints' refusal entries are "read from the
// same lists the renderer refuses overlay keys against, so the two can never
// drift". A shell script cannot read a Go slice, so the entries are typed out
// twice -- and until this file existed, "can never drift" was a sentence with
// nothing behind it.
//
// This is what holds it instead: the scripts stay the source of what they
// refuse, and the lists are compared here. A fourth file added to
// render.PaperFiles without a matching entry in image/entrypoint.sh is the
// case worth naming. It fails no test today: the renderer writes the file, the
// entrypoint accepts it from an extraFiles claim, the copy lands, the renderer
// overwrites it, and the administrator's file is gone with nothing said. That
// silent no-op is what design 3.3 calls the worst outcome, and it is the one
// this test turns into a red build.
//
// The reverse direction matters too, and is the likelier mistake: an entry
// left in a script after the renderer stopped writing that file refuses a path
// no owner claims, which crash-loops a group for nothing.

// ownedLoop matches the one scan in each entrypoint that walks the renderer's
// own files:
//
//	for owned in server.properties config/paper-global.yml ...; do
//
// The directory refusals above it -- plugins/ and, on the proxy, lang/ -- are
// deliberately not matched. Neither comes from a renderer list: plugins/
// belongs to extraPlugins and lang/ to Velocity itself, so there is nothing on
// the Go side for them to drift against.
var ownedLoop = regexp.MustCompile(`(?m)^[ \t]*for owned in (.+); do[ \t]*$`)

// scriptRefusalList reads an entrypoint and returns the paths its scan
// refuses. It fails rather than returning nothing when the loop it is looking
// for is missing or duplicated: a regexp that quietly matches nothing would
// make every assertion below pass against an empty list, which is the one way
// a drift test can be worse than no test at all.
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

// assertNoRefusalDrift compares one script's list against one renderer list
// and says which way they went apart. "They differ" would send somebody to
// diff two files by hand; the direction is the whole diagnosis, because the
// two directions are different bugs with different fixes.
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

// TestTheTwoFlavoursRefusalListsAreNotTheSame is the other half of the
// promise. Design 3.3 says the entries are "the running flavour's own", and a
// well-meant tidy-up that gave both scripts the union of the two lists would
// pass every assertion above -- each script would still match its own Go list
// only if the Go lists were merged too, but the tempting version of that
// mistake merges the shell and leaves the Go alone. This says out loud that a
// Paper server must not refuse velocity.toml and a proxy must not refuse
// server.properties.
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

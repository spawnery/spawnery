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

package cloudevent

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// The agent's feed picks each line's row from a table of this operator's event
// reasons, spelled as strings in Kotlin. A renamed reason breaks nothing
// visibly: the agent just shows it in verbose only, forever.
//
// One direction only: the table names what has a row, not every reason.
func TestTheAgentsLevelTableNamesReasonsThisOperatorHas(t *testing.T) {
	const table = "agent/common/src/main/kotlin/cloud/spawnery/agent/EventLevels.kt"
	raw, err := os.ReadFile(testenv.RepoPath(t, table))
	if err != nil {
		t.Fatalf("read the agent's level table: %v", err)
	}

	sets := regexp.MustCompile(`(?s)private val (?:CREATED|ARRIVED|READY|LEAVING|GONE|FAILURES) = setOf\((.*?)\)`).
		FindAllSubmatch(raw, -1)
	if len(sets) != 6 {
		t.Fatalf("found %d of the six `private val … = setOf(...)` blocks in %s. Either they were "+
			"renamed, in which case this test has to follow them, or a row is gone", len(sets), table)
	}
	short := regexp.MustCompile(`(?s)private val SHORT = mapOf\((.*?)\n\)`).FindSubmatch(raw)
	if short == nil {
		t.Fatalf("found no `private val SHORT = mapOf(...)` block in %s", table)
	}

	named := map[string]bool{}
	for _, set := range sets {
		for _, m := range regexp.MustCompile(`"([A-Za-z]+)"`).FindAllSubmatch(set[1], -1) {
			named[string(m[1])] = true
		}
	}
	for _, m := range regexp.MustCompile(`"([A-Za-z]+)" to`).FindAllSubmatch(short[1], -1) {
		named[string(m[1])] = true
	}
	if len(named) == 0 {
		t.Fatal("the table names no reasons at all; a scanner that finds nothing " +
			"passes every assertion after it")
	}

	known := reasonsInThisOperator(t)
	for reason := range named {
		if !known[reason] {
			t.Errorf("the agent's table names %q, which is not an event reason this "+
				"operator records. Either it was renamed here, and the agent now shows it "+
				"in verbose only and says nothing, or the table has a typo. Reasons are "+
				"`Reason… = \"…\"` constants or string literals in an Eventf call under internal/.",
				reason)
		}
	}
}

// reasonsInThisOperator collects every `Reason… = "…"` constant and every reason spelled as a literal in an Eventf call under internal/.
func reasonsInThisOperator(t *testing.T) map[string]bool {
	t.Helper()
	root := testenv.RepoPath(t, "internal")
	re := regexp.MustCompile(`Reason[A-Za-z]*\s*=\s*"([A-Za-z]+)"`)
	literal := regexp.MustCompile(`(?s)Eventf\(\s*[^,()]+,\s*[^,()]+,\s*corev1\.EventType(?:Normal|Warning),\s*"([A-Za-z]+)"`)

	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllSubmatch(raw, -1) {
			out[string(m[1])] = true
		}
		for _, m := range literal.FindAllSubmatch(raw, -1) {
			out[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/ for event reasons: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no event reasons found under internal/; this test would pass on an empty set")
	}
	return out
}

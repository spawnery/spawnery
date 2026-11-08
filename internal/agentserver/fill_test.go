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
	"testing"
	"time"
)

func TestChooseFillFollowsTheSharedTable(t *testing.T) {
	c := func(name string, players, slots, line int32, stale bool, number int32) fillCandidate {
		return fillCandidate{Name: name, Players: players, Slots: slots, Line: line, Stale: stale, Number: number}
	}
	for _, tc := range []struct {
		name  string
		cands []fillCandidate
		want  string
	}{
		{"A: the fuller server below its line", []fillCandidate{
			c("a", 5, 25, 20, false, 1), c("b", 0, 25, 20, false, 2)}, "a"},
		{"B: below the line beats at it", []fillCandidate{
			c("a", 20, 25, 20, false, 1), c("b", 3, 25, 20, false, 2)}, "b"},
		{"C: above the line, more room wins", []fillCandidate{
			c("a", 20, 25, 20, false, 1), c("b", 22, 25, 20, false, 2)}, "a"},
		{"D: a tie goes to the lower number", []fillCandidate{
			c("a", 4, 25, 20, false, 1), c("b", 4, 25, 20, false, 2)}, "a"},
		{"E: number 0 comes last", []fillCandidate{
			c("a", 4, 25, 20, false, 0), c("b", 4, 25, 20, false, 2)}, "b"},
		{"F: current before stale", []fillCandidate{
			c("a", 10, 25, 20, true, 1), c("b", 2, 25, 20, false, 2)}, "b"},
		{"G: a full server and a stale one both lose", []fillCandidate{
			c("a", 10, 25, 20, false, 1), c("b", 25, 25, 20, false, 2), c("c", 3, 25, 20, true, 3)}, "a"},
		{"H: room before current", []fillCandidate{
			c("a", 25, 25, 20, false, 1), c("c", 3, 25, 20, true, 3)}, "c"},
		{"I: a server that has not reported is below an unlimited line", []fillCandidate{
			c("a", 0, 0, 0, false, 1), c("b", 3, 25, 20, false, 2)}, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := chooseFill(tc.cands)
			if !ok || got != tc.want {
				t.Errorf("chooseFill = %q ok=%v, want %q", got, ok, tc.want)
			}
		})
	}

	if got, ok := chooseFill(nil); ok {
		t.Errorf("chooseFill(nil) = %q, want no choice", got)
	}
}

func TestPendingMovesExpireAfterTheTTL(t *testing.T) {
	t0 := time.Unix(1000, 0)
	now := t0
	p := newPendingMoves(func() time.Time { return now }, 15*time.Second)

	p.add("ns", "lobby-a")

	now = t0.Add(14 * time.Second)
	if got := p.count("ns", "lobby-a"); got != 1 {
		t.Errorf("count at t0+14s = %d, want 1", got)
	}
	if got := p.count("other", "lobby-a"); got != 0 {
		t.Errorf("count of the same name in another namespace = %d, want 0", got)
	}
	now = t0.Add(15 * time.Second)
	if got := p.count("ns", "lobby-a"); got != 0 {
		t.Errorf("count at t0+15s = %d, want 0", got)
	}
}

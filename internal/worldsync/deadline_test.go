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

package worldsync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (h *harness) deadline(target string, seq int64, at time.Time) {
	h.t.Helper()
	body := fmt.Sprintf("%d %d", seq, at.UnixMilli())
	if err := os.WriteFile(filepath.Join(target, DeadlineFile), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) queued(id string) int {
	return len(h.node(id).lookup(w).Pending)
}

func requestWorld(t *testing.T) (*harness, string) {
	h := newHarness(t)
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	return h, target
}

func TestARequestPastItsDeadlineIsRefusedAndNothingIsCopied(t *testing.T) {
	h, target := requestWorld(t)
	h.deadline(target, 1, h.now.Add(-time.Second))
	copies := 0
	copyHook = func(string) { copies++ }
	t.Cleanup(func() { copyHook = nil })
	h.request(target, "1")

	got := strings.TrimSpace(h.waitFile(target, DoneFile))
	if !strings.HasPrefix(got, "failed 1 ") || !strings.Contains(got, "deadline") {
		t.Fatalf("done = %q, want a refusal naming the deadline", got)
	}
	if copies != 0 {
		t.Fatalf("%d files copied for a request past its deadline", copies)
	}
	if n := h.queued("a"); n != 0 {
		t.Fatalf("%d snapshots queued for a request nobody waits for", n)
	}
	if _, err := os.Stat(h.node("a").snapDir(w, 1)); !os.IsNotExist(err) {
		t.Fatalf("a snapshot directory exists for a refused request: %v", err)
	}
}

func TestARequestAtItsDeadlineIsRefused(t *testing.T) {
	h, target := requestWorld(t)
	h.deadline(target, 1, h.now)
	h.request(target, "1")
	if got := strings.TrimSpace(h.waitFile(target, DoneFile)); !strings.HasPrefix(got, "failed 1 ") {
		t.Fatalf("done = %q, want a refusal", got)
	}
}

func TestARequestBeforeItsDeadlineIsCopied(t *testing.T) {
	h, target := requestWorld(t)
	h.deadline(target, 1, h.now.Add(time.Minute))
	h.request(target, "1")
	if got := strings.TrimSpace(h.waitFile(target, DoneFile)); got != "1" {
		t.Fatalf("done = %q, want 1", got)
	}
}

func TestARequestWithoutADeadlineIsCopiedAsBefore(t *testing.T) {
	h, target := requestWorld(t)
	h.request(target, "1")
	if got := strings.TrimSpace(h.waitFile(target, DoneFile)); got != "1" {
		t.Fatalf("done = %q, want 1", got)
	}
}

func TestADeadlineWrittenForAnotherRequestDoesNotBindThisOne(t *testing.T) {
	h, target := requestWorld(t)
	h.deadline(target, 1, h.now.Add(-time.Hour))
	h.request(target, "2")
	if got := strings.TrimSpace(h.waitFile(target, DoneFile)); got != "2" {
		t.Fatalf("done = %q, want 2", got)
	}
}

func TestAnUnreadableDeadlineDoesNotStopTheCopy(t *testing.T) {
	for _, body := range []string{"", "1", "1 soon", "x 5", "1 2 3", "1 -5"} {
		t.Run(body, func(t *testing.T) {
			h, target := requestWorld(t)
			if err := os.WriteFile(filepath.Join(target, DeadlineFile), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			h.request(target, "1")
			if got := strings.TrimSpace(h.waitFile(target, DoneFile)); got != "1" {
				t.Fatalf("done = %q, want 1", got)
			}
		})
	}
}

func TestACopyThatOutlastsTheDeadlineIsDroppedAndRefused(t *testing.T) {
	h, target := requestWorld(t)
	h.deadline(target, 1, h.now.Add(time.Minute))
	copyHook = func(string) { h.now = h.now.Add(2 * time.Minute) }
	t.Cleanup(func() { copyHook = nil })
	h.request(target, "1")

	got := strings.TrimSpace(h.waitFile(target, DoneFile))
	if !strings.HasPrefix(got, "failed 1 ") || !strings.Contains(got, "deadline") {
		t.Fatalf("done = %q, want a refusal naming the deadline", got)
	}
	if n := h.queued("a"); n != 0 {
		t.Fatalf("%d snapshots queued after a copy that ran past its deadline", n)
	}
	if _, err := os.Stat(h.node("a").snapDir(w, 1)); !os.IsNotExist(err) {
		t.Fatalf("the late copy was left on disk: %v", err)
	}
}

func TestARefusedRequestIsNotRetried(t *testing.T) {
	h, target := requestWorld(t)
	h.deadline(target, 1, h.now.Add(-time.Second))
	h.request(target, "1")
	h.waitFile(target, DoneFile)
	h.deadline(target, 1, h.now.Add(time.Hour))
	for range 3 {
		h.node("a").Settle(t.Context())
	}
	if n := h.queued("a"); n != 0 {
		t.Fatalf("%d snapshots queued for a request already answered", n)
	}
}

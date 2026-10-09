# World history for object store worlds: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An `ObjectStore` world keeps older generations under a per-group retention policy in the style of Proxmox Backup Server's prune options, and a plugin can list a member's restore points and restore one while the member is stopped.

**Architecture:** Everything that touches the bucket stays in `internal/worldsync`: a pure retention selection (`Retention.Select`), history keys under `history/`, a `Prune` that replaces `dropUnnamed` and `dropStrayPacks`, a `CommitSnapshot` that copies the previous manifest into `history/` before the manifest commit, and `RestoreWorld`/`ListRestorePoints` that the operator calls under the world's lease. The node agent reads the group's policy from `.retention/<ns>/<group>.json`, which the ServerGroup reconciler writes. The operator answers two new `CloudRequest`s, and the Java API gains `listRestorePoints` and `restoreWorld`.

**Tech Stack:** Go 1.26, controller-runtime and envtest, protobuf (`make proto`), Kotlin agents and the Java API (`make agent`, JUnit), Grafana dashboard JSON, kind + MinIO for e2e, Nix devshell.

**Spec:** `docs/superpowers/specs/2026-10-08-world-history-design.md`

## Global Constraints

- Every build and test command runs in the devshell, with the flake path as an argument and no `cd` before `nix develop`: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c <command>`. Inside, address the repository with `go -C /home/paul/git/spawnery`, `make -C /home/paul/git/spawnery` and `git -C /home/paul/git/spawnery`, because an agent's shell does not keep its working directory.
- Run `hostname` before the first heavy command. On the dev VM (`dev`, 2 CPUs, 12 GB) every `go test` gets `-p 1` and a full `make test` runs as `env GOFLAGS=-p=1 make test`; envtest packages in parallel exhaust its memory (exit 137). On `paul-desktop` the flag may be dropped.
- New Go files start with the Apache header from `hack/boilerplate.go.txt` (`Copyright paul_wtf.`); new Java and Kotlin files carry the same header as their neighbours.
- Before each commit, `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery fmt`: struct fields inserted by the steps below realign under gofmt.
- The code of Tasks 1 to 11 and the dashboard script of Task 12 were applied to a scratch worktree of `f21480c` while this plan was written: Go compiles, `go vet` (also `-tags e2e`) is clean, the tests named in the steps pass (worldsync with `-race`; api and controller envtests filtered to the new tests), and `make agent` builds with its JUnit suites green. The kind e2e itself was not run. Line numbers given as "~" are from that commit.
- Comments: the default is none. A comment stays only where a reader cannot derive it from the code beside it (behaviour of a foreign system, an absence, a number that looks arbitrary, why the obvious way was not taken). No fix histories in comments.
- Commits are Conventional Commits in English with a scope (`feat(worldsync): …`), ending with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`. Commit bodies, docs prose and PR text go through the `humanizer` skill (embedded mode) before use; code and identifiers do not.
- Commits are gpg-signed. On the dev VM, before the first commit, probe with `echo probe | gpg --clearsign --pinentry-mode error -u "$(git -C /home/paul/git/spawnery config user.signingkey)" > /dev/null`; exit 0 means commit, anything else means ask Paul to unlock with `! echo probe | gpg --clearsign > /dev/null` and wait. Never disable signing. On `paul-desktop` just commit.
- After changing API types or markers: `make -C /home/paul/git/spawnery manifests generate` and commit the generated files. After changing the proto: `make -C /home/paul/git/spawnery proto` and commit `internal/agentpb` and `agent/common/src/proto/java`. Generated docs are checked by `crd-docs-test`, `metrics-docs-test` and `chart-values-docs-test` inside `make test`; page lengths by `docs-length-lint`.
- `git add` new files under `agent/` before `make agent`: the Nix build reads the git index.
- Bucket layout, verbatim from the spec: history entries at `history/<generation, 20 digits>-<taken, unix ms>.json` under the world's prefix; the group policy at `.retention/<namespace>/<group>.json` under the base; `Manifest` gains `taken` (unix ms) and `restoredFrom`. The full sweep runs on every 12th prune of a world and at the release of its lease. The operator takes the lease as `spawnery-operator`.
- Metric names, verbatim: `spawnery_worldsync_pruned_objects_total` (node agent), `spawnery_world_restores_total{result}` with `restored`, `refused`, `unavailable`, `failed` (operator).
- Retention periods are UTC; weeks are ISO weeks. The current generation is always kept and counts as one of `last`.
- No private network names anywhere in this repository (code, tests, docs, commits, PR): say "a network".
- The kind e2e (`make e2e-worldsync`) runs on `paul-desktop` only, not on the dev VM. If you execute on the dev VM, hand Task 11 Step 4 to Paul with the exact command and what to look for.
- Release numbers: operator, game images, Java API and chart move to `0.25.0` (minor: new CRD field, new API) in the last task. A PR is opened; merging, tagging and publishing wait for Paul's explicit word.

## Review Focus

1. **A prune that dies half-way, followed by a restore.** Prune deletes the dropped history entries before the objects only they name, so a restore never picks an entry whose objects are gone; stray objects are what the next sweep collects. Test in Task 3 (`TestHistoryEntriesGoBeforeTheObjectsOnlyTheyNamed`).
2. **A `.retention` file that cannot be read or is malformed.** It must never shrink a world's history: the node keeps the last policy it read, and before its first successful read it copies the previous manifest into history and deletes nothing. Test in Task 5 (`TestAPolicyThatCannotBeReadKeepsHistoryAndDeletesNothing`, `TestAMalformedPolicyKeepsWhatWasReadBefore`).
3. **The leftover history entry of a crash between the history write and the manifest commit.** Its generation equals the current one; it must be neither listed as a second restore point nor kept by prune. Tests in Task 4 (`TestACrashBeforeTheManifestLeavesAnEntryThePruneRemoves`) and Task 6 (`TestTheRestorePointsAreTheCurrentAndItsOlderEntriesNewestFirst`).
4. **A restore whose manifest put committed but whose SDK retry answered 412.** It is a restore, not a conflict; the plugin must not be told `UNAVAILABLE` for a world that already moved. Test in Task 6 (`TestARestoreWhoseCommitWasAnswered412IsARestore`).
5. **A restore asked while the member's `Server` is still being deleted.** The plugin was told to ask again after a stop, so this is `UNAVAILABLE`, not `REFUSED`. Test in Task 9 (`TestRestoreWorldAnswersEveryReasonOfTheAPI`, case "the member is stopping").

---

## File structure

| path | responsibility |
|---|---|
| `internal/worldsync/retention.go` | `Retention`, `Point`, `Retention.Select` (PBS `mark_selections`), `KeepsHistory` |
| `internal/worldsync/history.go` | `HistoryKey`, `ParseHistoryKey`, `HistoryEntry`, `ListHistory`, `readHistoryEntry` |
| `internal/worldsync/policy.go` | `ReadRetention`, `WriteRetention`, `Policies` (operator side, remembers what it wrote) |
| `internal/worldsync/prune.go` | `ObjectCache`, `PruneRequest`, `Prune` |
| `internal/worldsync/restore.go` | `RestorePoint`, `Restored`, `ListRestorePoints`, `RestoreWorld`, `BucketWorlds.RestorePoints/Restore` |
| `internal/worldsync/layout.go` | `HistoryDir`, `RetentionKey` |
| `internal/worldsync/manifest.go` | `Manifest.Taken`, `Manifest.RestoredFrom`, `TakenAt`, `readCurrent` |
| `internal/worldsync/snapshot.go` | `Snap.Taken`, `TakeSnapshot(…, taken)` |
| `internal/worldsync/transfer.go` | `CommitSnapshot`, `putManifest`, `putHistory`; `UploadSnapshot` = commit + prune; `dropUnnamed` gone |
| `internal/worldsync/node.go`, `state.go` | policy read at publish and before each upload, kept-object cache, sweep counter, sweep at release, `dropStrayPacks` gone |
| `internal/worldsync/metrics.go` | `prunedObjects` |
| `api/v1alpha1/servergroup_types.go` | `RetentionSpec`, `StorageSpec.Retention`, CEL rule, `WorldRetention()` |
| `internal/controller/retention.go`, `servergroup_controller.go`, `setup.go` | `RetentionPublisher`, publishing from the reconciler |
| `proto/spawnery/agent/v1alpha1/agent.proto` | `ListRestorePoints*`, `RestoreWorld*`, `RestorePoint` |
| `internal/agentserver/worldhistory.go`, `writer.go`, `requests.go`, `metrics.go` | writer methods, answers, `WorldRestores`, event |
| `cmd/spawnery-operator/main.go` | wiring of `Policies` and `WorldHistory` |
| `agent/api/…/RestorePoint.java`, `RestoredWorld.java`, `SpawneryApi.java` | Java API |
| `agent/common/…/CloudConnector.kt`, `MirrorApi.kt` | Kotlin implementation |
| `test/e2e/worldhistory_test.go`, `test/e2e/manifests/worldsync.yaml`, `hack/e2e-worldsync.sh` | e2e |
| `charts/spawnery/dashboards/worldsync.json`, `network.json` | pruned objects panel, restores panel |
| `docs/guides/object-store-worlds.md`, `docs/plugin-api/what-a-plugin-can-do.md`, `docs/guides/on-demand-servers.md`, `docs/reference/known-issues.md` | docs |

---

### Task 1: Retention selection

**Files:**
- Create: `internal/worldsync/retention.go`
- Test: `internal/worldsync/retention_test.go`

**Interfaces:**
- Produces:
  ```go
  type Retention struct { Last, Hourly, Daily, Weekly, Monthly, Yearly int32 } // json: last, hourly, daily, weekly, monthly, yearly (omitempty)
  func (r Retention) KeepsHistory() bool
  type Point struct { Generation int64; Taken time.Time }
  func (r Retention) Select(points []Point) []bool // points newest first, points[0] the current
  ```
  The field names, types and order of `Retention` must equal `v1alpha1.RetentionSpec` (Task 7) so that `worldsync.Retention(spec)` compiles.

The selection is Proxmox Backup Server's `mark_selections` (pbs-datastore/src/prune.rs), run once per option in the order last, hourly, daily, weekly, monthly, yearly over the points sorted newest first: a period that holds a point an earlier option kept is skipped whole; the newest unmarked point of each further period is kept until the option's count is reached; the other points of a period it kept from are marked and every later option passes over them. Afterwards `points[0]` is kept regardless. The spec's sentence "each considers only generations older than the oldest one the options before it kept" paraphrases the PBS documentation; the code differs from it (see the test "a later option skips a period an earlier one kept from"), and this plan follows the code, as the spec asks for PBS's behaviour.

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/retention_test.go`:

```go
package worldsync

import (
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// pointsAt numbers the times from newest to oldest: the first gets the
// highest generation.
func pointsAt(t *testing.T, times ...string) []Point {
	t.Helper()
	ps := make([]Point, len(times))
	for i, s := range times {
		ps[i] = Point{Generation: int64(len(times) - i), Taken: at(t, s)}
	}
	return ps
}

func keptGenerations(ps []Point, keep []bool) []int64 {
	var out []int64
	for i, k := range keep {
		if k {
			out = append(out, ps[i].Generation)
		}
	}
	return out
}

func TestRetentionSelect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Retention
		times  []string
		want   []int64
	}{
		{
			name:   "without options only the current stays",
			policy: Retention{},
			times:  []string{"2026-10-08T10:50:00Z", "2026-10-08T10:45:00Z", "2026-10-07T09:00:00Z"},
			want:   []int64{3},
		},
		{
			name:   "the current counts as one of last",
			policy: Retention{Last: 2},
			times:  []string{"2026-10-08T10:50:00Z", "2026-10-08T10:45:00Z", "2026-10-08T10:40:00Z", "2026-10-08T10:35:00Z"},
			want:   []int64{4, 3},
		},
		{
			name:   "the newest of each hour, and empty hours do not count",
			policy: Retention{Hourly: 3},
			times: []string{
				"2026-10-08T10:50:00Z", "2026-10-08T10:20:00Z",
				"2026-10-08T09:55:00Z", "2026-10-08T09:05:00Z",
				"2026-10-08T07:30:00Z", "2026-10-08T06:10:00Z",
			},
			want: []int64{6, 4, 2},
		},
		{
			name:   "a tie within one period goes to the newer generation",
			policy: Retention{Hourly: 2},
			times:  []string{"2026-10-08T10:30:00Z", "2026-10-08T10:30:00Z", "2026-10-08T09:00:00Z"},
			want:   []int64{3, 1},
		},
		{
			// The PBS documentation's wording would keep generation 3 as the
			// newest of 2026-10-08 among those older than 4; its code skips
			// the whole day, because last kept 4 from it.
			name:   "a later option skips a period an earlier one kept from",
			policy: Retention{Last: 1, Daily: 2},
			times:  []string{"2026-10-08T10:50:00Z", "2026-10-08T09:50:00Z", "2026-10-08T08:50:00Z", "2026-10-07T23:50:00Z"},
			want:   []int64{4, 1},
		},
		{
			// ISO week 14 of 2026 runs from Monday 30 March to 5 April.
			name:   "the passed-over points of a week across two months stay passed over",
			policy: Retention{Weekly: 1, Monthly: 2},
			times:  []string{"2026-04-02T12:00:00Z", "2026-03-31T12:00:00Z", "2026-03-20T12:00:00Z", "2026-02-10T12:00:00Z"},
			want:   []int64{4, 2, 1},
		},
		{
			// All three fall on 2026-10-07 in UTC; in UTC+2 the first is on the 8th.
			name:   "periods are UTC",
			policy: Retention{Daily: 2},
			times:  []string{"2026-10-08T00:30:00+02:00", "2026-10-07T23:30:00+02:00", "2026-10-07T12:00:00Z"},
			want:   []int64{3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := pointsAt(t, tc.times...)
			got := keptGenerations(ps, tc.policy.Select(ps))
			if len(got) != len(tc.want) {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("kept %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The PBS documentation's example for ten years of daily backups:
// keep-last 3, keep-daily 13, keep-weekly 8, keep-monthly 11, keep-yearly 9.
// Twelve years of points give every option all the periods it asks for.
func TestTheProxmoxDocumentationsTenYearExampleKeeps44(t *testing.T) {
	end := at(t, "2026-10-08T02:00:00Z")
	n := 12 * 366
	ps := make([]Point, n)
	for i := range ps {
		ps[i] = Point{Generation: int64(n - i), Taken: end.AddDate(0, 0, -i)}
	}
	keep := Retention{Last: 3, Daily: 13, Weekly: 8, Monthly: 11, Yearly: 9}.Select(ps)
	if got := len(keptGenerations(ps, keep)); got != 44 {
		t.Fatalf("kept %d, want 3+13+8+11+9 = 44", got)
	}
}

func TestSelectOfNothingIsNothing(t *testing.T) {
	if got := (Retention{Last: 3}).Select(nil); len(got) != 0 {
		t.Fatalf("Select(nil) = %v", got)
	}
}

func TestKeepsHistory(t *testing.T) {
	for _, tc := range []struct {
		r    Retention
		want bool
	}{
		{Retention{}, false},
		{Retention{Last: 1}, false},
		{Retention{Weekly: 1}, false},
		{Retention{Last: 2}, true},
		{Retention{Last: 1, Hourly: 1}, true},
		{Retention{Yearly: 3}, true},
	} {
		if got := tc.r.KeepsHistory(); got != tc.want {
			t.Errorf("%+v.KeepsHistory() = %v, want %v", tc.r, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 -run 'RetentionSelect|TenYear|SelectOfNothing|KeepsHistory' ./internal/worldsync/`
Expected: FAIL, `undefined: Point`, `undefined: Retention`.

- [ ] **Step 3: Implement**

`internal/worldsync/retention.go` (Apache header first):

```go
package worldsync

import (
	"fmt"
	"strconv"
	"time"
)

// Retention mirrors v1alpha1.RetentionSpec field for field, so that one
// converts to the other.
type Retention struct {
	Last    int32 `json:"last,omitempty"`
	Hourly  int32 `json:"hourly,omitempty"`
	Daily   int32 `json:"daily,omitempty"`
	Weekly  int32 `json:"weekly,omitempty"`
	Monthly int32 `json:"monthly,omitempty"`
	Yearly  int32 `json:"yearly,omitempty"`
}

// KeepsHistory: a single option of 1 keeps exactly the current generation.
func (r Retention) KeepsHistory() bool {
	return int64(r.Last)+int64(r.Hourly)+int64(r.Daily)+int64(r.Weekly)+int64(r.Monthly)+int64(r.Yearly) > 1
}

type Point struct {
	Generation int64
	Taken      time.Time
}

// Select follows Proxmox Backup Server's mark_selections, one option after
// the other; points are newest first and points[0] is the current
// generation, which is kept whatever the options say.
func (r Retention) Select(points []Point) []bool {
	keep := make([]bool, len(points))
	marked := make([]bool, len(points))
	for _, o := range []struct {
		n  int32
		id func(Point) string
	}{
		{r.Last, func(p Point) string { return strconv.FormatInt(p.Generation, 10) }},
		{r.Hourly, func(p Point) string { return p.Taken.UTC().Format("2006-01-02T15") }},
		{r.Daily, func(p Point) string { return p.Taken.UTC().Format("2006-01-02") }},
		{r.Weekly, func(p Point) string {
			y, w := p.Taken.UTC().ISOWeek()
			return fmt.Sprintf("%d-W%02d", y, w)
		}},
		{r.Monthly, func(p Point) string { return p.Taken.UTC().Format("2006-01") }},
		{r.Yearly, func(p Point) string { return p.Taken.UTC().Format("2006") }},
	} {
		markSelections(points, keep, marked, int(o.n), o.id)
	}
	if len(points) > 0 {
		keep[0] = true
	}
	return keep
}

func markSelections(points []Point, keep, marked []bool, n int, id func(Point) string) {
	covered := map[string]bool{}
	for i, p := range points {
		if keep[i] {
			covered[id(p)] = true
		}
	}
	chosen := map[string]bool{}
	for i, p := range points {
		if marked[i] {
			continue
		}
		sel := id(p)
		if covered[sel] {
			continue
		}
		if chosen[sel] {
			marked[i] = true
			continue
		}
		if len(chosen) >= n {
			return
		}
		chosen[sel] = true
		keep[i], marked[i] = true, true
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/worldsync/retention.go internal/worldsync/retention_test.go
git -C /home/paul/git/spawnery commit -m "feat(worldsync): select the generations a retention policy keeps" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: History keys, manifest fields, the policy file

**Files:**
- Modify: `internal/worldsync/layout.go` (constants, after `PacksDir`; new func at the end)
- Modify: `internal/worldsync/manifest.go` (`Manifest`, new `TakenAt`, `readCurrent`)
- Create: `internal/worldsync/history.go`, `internal/worldsync/policy.go`
- Test: `internal/worldsync/history_test.go`, `internal/worldsync/policy_test.go`

**Interfaces:**
- Consumes: `Point`, `Retention` (Task 1); `Store`, `ErrNotFound`, `readObject` (transfer.go), `join` (layout.go).
- Produces:
  ```go
  const HistoryDir = "history/"
  func RetentionKey(base, namespace, group string) string           // <base>/.retention/<ns>/<group>.json
  // Manifest gains: Taken int64 `json:"taken,omitempty"`; RestoredFrom int64 `json:"restoredFrom,omitempty"`
  func (m Manifest) TakenAt(lastModified time.Time) time.Time      // UTC; lastModified when Taken == 0
  func readCurrent(ctx context.Context, st Store, prefix string) ([]byte, Manifest, ObjectInfo, error)
  func HistoryKey(generation int64, taken time.Time) string        // relative to the world prefix
  func ParseHistoryKey(rel string) (Point, bool)
  type HistoryEntry struct { Point; Key string }                   // Key: full store key
  func ListHistory(ctx context.Context, st Store, prefix string) ([]HistoryEntry, error) // newest generation first
  func readHistoryEntry(ctx context.Context, st Store, key string) (Manifest, error)
  func ReadRetention(ctx context.Context, st Store, base, namespace, group, etag string) (r Retention, newETag string, changed bool, err error)
  func WriteRetention(ctx context.Context, st Store, base, namespace, group string, r Retention) error // zero policy deletes
  type Policies struct { Store Store; Base string /* + unexported cache */ }
  func (p *Policies) Sync(ctx context.Context, namespace, group string, r Retention) error
  type countingStore struct { *MemStore; gets, heads, puts, deletes atomic.Int64 } // test helper, policy_test.go
  ```

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/history_test.go`:

```go
package worldsync

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAHistoryKeyCarriesGenerationAndTimeAndSortsByGeneration(t *testing.T) {
	taken := time.Date(2026, 10, 8, 10, 0, 0, 123_000_000, time.UTC)
	k := HistoryKey(7, taken)
	if !strings.HasPrefix(k, "history/00000000000000000007-") || !strings.HasSuffix(k, ".json") {
		t.Fatalf("key = %q", k)
	}
	p, ok := ParseHistoryKey(k)
	if !ok || p.Generation != 7 || !p.Taken.Equal(taken) {
		t.Fatalf("ParseHistoryKey(%q) = %+v, %v", k, p, ok)
	}
	if HistoryKey(9, taken) >= HistoryKey(10, taken) {
		t.Fatal("keys of generations 9 and 10 do not sort by generation")
	}
}

func TestParseHistoryKeyRefusesWhatIsNoEntry(t *testing.T) {
	for _, k := range []string{
		"history/7-1.json",
		"history/x.json",
		"history/00000000000000000007-1.tmp",
		"history/00000000000000000007--5.json",
		"history/00000000000000000000-1.json",
		"objects/00000000000000000007-1.json",
	} {
		if p, ok := ParseHistoryKey(k); ok {
			t.Errorf("ParseHistoryKey(%q) = %+v, want refused", k, p)
		}
	}
}

func TestListHistoryIsNewestFirstAndSkipsStrangers(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	taken := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	for _, k := range []string{HistoryKey(2, taken), HistoryKey(10, taken), HistoryKey(1, taken), "history/README"} {
		if _, err := st.Put(ctx, prefix+k, strings.NewReader("{}"), PutCondition{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var gens []int64
	for _, e := range entries {
		gens = append(gens, e.Generation)
		if !strings.HasPrefix(e.Key, prefix+HistoryDir) {
			t.Errorf("key %q is not the full key", e.Key)
		}
	}
	if len(gens) != 3 || gens[0] != 10 || gens[1] != 2 || gens[2] != 1 {
		t.Fatalf("generations = %v, want [10 2 1]", gens)
	}
}

func TestTakenAtFallsBackToWhenTheManifestWasWritten(t *testing.T) {
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if got := (Manifest{}).TakenAt(written); !got.Equal(written) {
		t.Fatalf("TakenAt of a manifest without taken = %v, want %v", got, written)
	}
	taken := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if got := (Manifest{Taken: taken.UnixMilli()}).TakenAt(written); !got.Equal(taken) {
		t.Fatalf("TakenAt = %v, want %v", got, taken)
	}
}
```

`internal/worldsync/policy_test.go`:

```go
package worldsync

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countingStore struct {
	*MemStore
	gets, heads, puts, deletes atomic.Int64
}

func (c *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	c.gets.Add(1)
	return c.MemStore.Get(ctx, key)
}

func (c *countingStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	c.heads.Add(1)
	return c.MemStore.Head(ctx, key)
}

func (c *countingStore) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	c.puts.Add(1)
	return c.MemStore.Put(ctx, key, body, cond)
}

func (c *countingStore) Delete(ctx context.Context, key string) error {
	c.deletes.Add(1)
	return c.MemStore.Delete(ctx, key)
}

func TestAPolicyRoundTripsAndTheZeroPolicyIsNoFile(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	want := Retention{Last: 12, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 3}
	if err := WriteRetention(ctx, st, "base", "ns", "g", want); err != nil {
		t.Fatal(err)
	}
	if k := RetentionKey("base", "ns", "g"); k != "base/.retention/ns/g.json" {
		t.Fatalf("RetentionKey = %q", k)
	}
	got, etag, changed, err := ReadRetention(ctx, st, "base", "ns", "g", "")
	if err != nil || got != want || etag == "" || !changed {
		t.Fatalf("ReadRetention = %+v, %q, %v, %v", got, etag, changed, err)
	}
	if _, again, changed, err := ReadRetention(ctx, st, "base", "ns", "g", etag); err != nil || changed || again != etag {
		t.Fatalf("a read with the current ETag = %q, %v, %v; want unchanged", again, changed, err)
	}
	if err := WriteRetention(ctx, st, "base", "ns", "g", Retention{}); err != nil {
		t.Fatal(err)
	}
	if len(st.Keys()) != 0 {
		t.Fatalf("the zero policy left %v", st.Keys())
	}
	got, etag, changed, err = ReadRetention(ctx, st, "base", "ns", "g", etag)
	if err != nil || got != (Retention{}) || etag != "" || !changed {
		t.Fatalf("a read after the deletion = %+v, %q, %v, %v; want the zero policy, changed", got, etag, changed, err)
	}
}

func TestAMalformedPolicyIsAnError(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	if _, err := st.Put(ctx, RetentionKey("", "ns", "g"), strings.NewReader("last: 3"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadRetention(ctx, st, "", "ns", "g", ""); err == nil {
		t.Fatal("a policy that is not JSON was read without an error")
	}
}

func TestPoliciesWriteOnlyWhatChanged(t *testing.T) {
	st := &countingStore{MemStore: NewMemStore(time.Now)}
	p := &Policies{Store: st, Base: ""}
	ctx := context.Background()
	for range 2 {
		if err := p.Sync(ctx, "ns", "g", Retention{Last: 3}); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.puts.Load(); got != 1 {
		t.Fatalf("puts = %d after the same policy twice, want 1", got)
	}
	if err := p.Sync(ctx, "ns", "g", Retention{Last: 4}); err != nil {
		t.Fatal(err)
	}
	if got := st.puts.Load(); got != 2 {
		t.Fatalf("puts = %d after a change, want 2", got)
	}
	for range 2 {
		if err := p.Sync(ctx, "ns", "g", Retention{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.deletes.Load(); got != 1 {
		t.Fatalf("deletes = %d after dropping the policy twice, want 1", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/`
Expected: FAIL, `undefined: HistoryKey`, `undefined: WriteRetention`, `undefined: Policies`.

- [ ] **Step 3: Implement**

In `internal/worldsync/layout.go`, add `HistoryDir` to the constant block after `PacksDir`:

```go
	PacksDir     = "packs/"
	HistoryDir   = "history/"
```

and at the end of the file:

```go
func RetentionKey(base, namespace, group string) string {
	return join(base, ".retention/"+namespace+"/"+group+".json")
}
```

In `internal/worldsync/manifest.go`, add `"time"` to the imports, replace the `Manifest` type and append the two functions:

```go
type Manifest struct {
	WorldID    string `json:"worldId"`
	Generation int64  `json:"generation"`
	// Taken is the node's clock in unix milliseconds when it took the
	// snapshot; manifests written before it existed have none.
	Taken        int64       `json:"taken,omitempty"`
	RestoredFrom int64       `json:"restoredFrom,omitempty"`
	Files        []FileEntry `json:"files"`
}

func (m Manifest) TakenAt(lastModified time.Time) time.Time {
	if m.Taken > 0 {
		return time.UnixMilli(m.Taken).UTC()
	}
	return lastModified.UTC()
}

// readCurrent returns the manifest's bytes too, for a copy into history/.
func readCurrent(ctx context.Context, st Store, prefix string) ([]byte, Manifest, ObjectInfo, error) {
	b, info, err := readObject(ctx, st, prefix+ManifestName)
	if err != nil {
		return nil, Manifest{}, ObjectInfo{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, Manifest{}, ObjectInfo{}, fmt.Errorf("decode %s%s: %w", prefix, ManifestName, err)
	}
	return b, m, info, nil
}
```

`internal/worldsync/history.go`:

```go
package worldsync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HistoryKey carries what retention and a listing need, so that neither
// reads the entry.
func HistoryKey(generation int64, taken time.Time) string {
	return fmt.Sprintf("%s%020d-%d.json", HistoryDir, generation, taken.UnixMilli())
}

func ParseHistoryKey(rel string) (Point, bool) {
	name, ok := strings.CutPrefix(rel, HistoryDir)
	if !ok {
		return Point{}, false
	}
	name, ok = strings.CutSuffix(name, ".json")
	if !ok {
		return Point{}, false
	}
	gen, ms, ok := strings.Cut(name, "-")
	if !ok || len(gen) != 20 {
		return Point{}, false
	}
	g, err := strconv.ParseInt(gen, 10, 64)
	if err != nil || g <= 0 {
		return Point{}, false
	}
	t, err := strconv.ParseInt(ms, 10, 64)
	if err != nil || t < 0 {
		return Point{}, false
	}
	return Point{Generation: g, Taken: time.UnixMilli(t).UTC()}, true
}

type HistoryEntry struct {
	Point
	Key string
}

func ListHistory(ctx context.Context, st Store, prefix string) ([]HistoryEntry, error) {
	keys, err := st.List(ctx, prefix+HistoryDir)
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	for _, k := range keys {
		if p, ok := ParseHistoryKey(strings.TrimPrefix(k, prefix)); ok {
			out = append(out, HistoryEntry{Point: p, Key: k})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Generation > out[j].Generation })
	return out, nil
}

func readHistoryEntry(ctx context.Context, st Store, key string) (Manifest, error) {
	b, _, err := readObject(ctx, st, key)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", key, err)
	}
	return m, nil
}
```

`internal/worldsync/policy.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ReadRetention reads a group's policy; a missing file is the zero policy.
// With the ETag of the last read it asks with a HEAD first and answers
// changed false without reading the file again.
func ReadRetention(ctx context.Context, st Store, base, namespace, group, etag string) (r Retention, newETag string, changed bool, err error) {
	key := RetentionKey(base, namespace, group)
	if etag != "" {
		info, err := st.Head(ctx, key)
		if errors.Is(err, ErrNotFound) {
			return Retention{}, "", true, nil
		}
		if err != nil {
			return Retention{}, "", false, err
		}
		if info.ETag == etag {
			return Retention{}, etag, false, nil
		}
	}
	b, info, err := readObject(ctx, st, key)
	if errors.Is(err, ErrNotFound) {
		return Retention{}, "", true, nil
	}
	if err != nil {
		return Retention{}, "", false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Retention{}, "", false, fmt.Errorf("decode %s: %w", key, err)
	}
	return r, info.ETag, true, nil
}

func WriteRetention(ctx context.Context, st Store, base, namespace, group string, r Retention) error {
	key := RetentionKey(base, namespace, group)
	if r == (Retention{}) {
		return st.Delete(ctx, key)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = st.Put(ctx, key, bytes.NewReader(b), PutCondition{})
	return err
}

// Policies remembers what it wrote per group, so that the reconciler's
// passes over an unchanged policy cost no request.
type Policies struct {
	Store Store
	Base  string

	mu      sync.Mutex
	written map[string]Retention
}

func (p *Policies) Sync(ctx context.Context, namespace, group string, r Retention) error {
	k := namespace + "/" + group
	p.mu.Lock()
	last, known := p.written[k]
	p.mu.Unlock()
	if known && last == r {
		return nil
	}
	if err := WriteRetention(ctx, p.Store, p.Base, namespace, group, r); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.written == nil {
		p.written = map[string]Retention{}
	}
	p.written[k] = r
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/`
Expected: PASS, the existing tests included (the manifest's new fields are `omitempty`).

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/worldsync/
git -C /home/paul/git/spawnery commit -m "feat(worldsync): history keys, taken and restoredFrom, the group policy file" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Prune

**Files:**
- Create: `internal/worldsync/prune.go`
- Modify: `internal/worldsync/transfer.go` (`UploadSnapshot`'s `dropUnnamed` call near line 181; delete `func dropUnnamed`)
- Test: `internal/worldsync/prune_test.go`

**Interfaces:**
- Consumes: `Retention.Select`, `Point` (Task 1); `ListHistory`, `HistoryEntry`, `readHistoryEntry`, `HistoryKey`, `countingStore` (Task 2); `uploadParallel` (transfer.go, 16).
- Produces:
  ```go
  type ObjectCache map[string][]string // history entry's full key -> the objects it names
  type PruneRequest struct {
      Current  Manifest   // the manifest now committed
      Replaced *Manifest  // the manifest Current replaced, or nil; its objects are candidates
      Policy   Retention
      Cache    ObjectCache // nil: every kept entry is read; Prune fills and trims it
      Sweep    bool        // also delete what no kept manifest names under objects/ and packs/
  }
  func Prune(ctx context.Context, st Store, prefix string, req PruneRequest) (int, error) // objects and packs deleted
  // test helpers in prune_test.go: manifestOf, putJSON, seedObjects, seedEntry, seedCurrent, entryKey, stored, historyGenerations, fiveGenerations
  ```

Prune: list `history/`; entries with a generation at or above the current one are covered and dropped (the leftover of a crash before a manifest commit); the others plus the current go through `Select`. Then the object names of the current and every kept entry are gathered (from the cache or read, 16 in parallel); a read that fails for any reason but `ErrNotFound` aborts the prune before it deletes anything. Dropped entries are read too, for their names. Then **the dropped entries are deleted first**, and only if all of them went, the candidate objects (the replaced manifest's and the dropped entries') that no kept manifest names. With `Sweep`, `objects/` and `packs/` are listed and every key no kept manifest names goes. The return value counts objects and packs, not entries.

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/prune_test.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// manifestOf is generation n, taken n hours after a fixed start, with one
// large file per object.
func manifestOf(n int64, objects ...string) Manifest {
	m := Manifest{WorldID: "w", Generation: n, Taken: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour).UnixMilli()}
	for i, o := range objects {
		m.Files = append(m.Files, FileEntry{Path: fmt.Sprintf("f%d", i), Size: PackBelow, Object: o})
	}
	return m
}

func putJSON(t *testing.T, st Store, key string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), key, bytes.NewReader(b), PutCondition{}); err != nil {
		t.Fatal(err)
	}
}

func seedObjects(t *testing.T, st Store, m Manifest) {
	t.Helper()
	for _, f := range m.Files {
		if _, err := st.Put(context.Background(), prefix+f.Object, strings.NewReader(f.Object), PutCondition{}); err != nil {
			t.Fatal(err)
		}
	}
}

func entryKey(m Manifest) string { return prefix + HistoryKey(m.Generation, time.UnixMilli(m.Taken)) }

func seedEntry(t *testing.T, st Store, m Manifest) {
	t.Helper()
	seedObjects(t, st, m)
	putJSON(t, st, entryKey(m), m)
}

func seedCurrent(t *testing.T, st Store, m Manifest) {
	t.Helper()
	seedObjects(t, st, m)
	putJSON(t, st, prefix+ManifestName, m)
}

func stored(t *testing.T, st Store, key string) bool {
	t.Helper()
	_, err := st.Head(context.Background(), key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func historyGenerations(t *testing.T, st Store) []int64 {
	t.Helper()
	entries, err := ListHistory(context.Background(), st, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, e := range entries {
		out = append(out, e.Generation)
	}
	return out
}

// fiveGenerations seeds generations 1 to 4 as history and 5 as current,
// each naming objects/shared and an object of its own.
func fiveGenerations(t *testing.T, st Store) Manifest {
	t.Helper()
	for n := int64(1); n <= 4; n++ {
		seedEntry(t, st, manifestOf(n, ObjectsDir+"shared", fmt.Sprintf("%sg%d", ObjectsDir, n)))
	}
	cur := manifestOf(5, ObjectsDir+"shared", ObjectsDir+"g5")
	seedCurrent(t, st, cur)
	return cur
}

func TestPruneWithoutAPolicyDropsWhatOnlyTheReplacedManifestNamed(t *testing.T) {
	st := NewMemStore(time.Now)
	m1 := manifestOf(1, ObjectsDir+"a", ObjectsDir+"b")
	m2 := manifestOf(2, ObjectsDir+"a", ObjectsDir+"c")
	seedObjects(t, st, m1)
	seedCurrent(t, st, m2)
	deleted, err := Prune(context.Background(), st, prefix, PruneRequest{Current: m2, Replaced: &m1})
	if err != nil || deleted != 1 {
		t.Fatalf("deleted = %d, %v; want 1", deleted, err)
	}
	if stored(t, st, prefix+ObjectsDir+"b") || !stored(t, st, prefix+ObjectsDir+"a") || !stored(t, st, prefix+ObjectsDir+"c") {
		t.Fatalf("store = %v; want b gone, a and c kept", st.Keys())
	}
}

func TestPruneKeepsWhatThePolicyKeepsAndWhatItNames(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	deleted, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 3}})
	if err != nil || deleted != 2 {
		t.Fatalf("deleted = %d, %v; want 2", deleted, err)
	}
	if got := historyGenerations(t, st); !slices.Equal(got, []int64{4, 3}) {
		t.Fatalf("history = %v, want [4 3]", got)
	}
	for _, o := range []string{"shared", "g3", "g4", "g5"} {
		if !stored(t, st, prefix+ObjectsDir+o) {
			t.Errorf("%s is gone, but a kept manifest names it", o)
		}
	}
	for _, o := range []string{"g1", "g2"} {
		if stored(t, st, prefix+ObjectsDir+o) {
			t.Errorf("%s survived, but only dropped generations named it", o)
		}
	}
}

func TestAnEntryOfTheCurrentGenerationIsCovered(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	seedEntry(t, st, cur)
	if _, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := historyGenerations(t, st); !slices.Equal(got, []int64{4, 3, 2, 1}) {
		t.Fatalf("history = %v, want [4 3 2 1]: the entry of the current generation goes, the rest stays", got)
	}
	if !stored(t, st, prefix+ObjectsDir+"g5") {
		t.Fatal("the current generation lost an object with its leftover entry")
	}
}

type brokenEntry struct {
	*MemStore
	key string
}

func (f *brokenEntry) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if key == f.key {
		return nil, ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Get(ctx, key)
}

func TestAKeptEntryThatCannotBeReadDeletesNothing(t *testing.T) {
	mem := NewMemStore(time.Now)
	cur := fiveGenerations(t, mem)
	before := mem.Keys()
	st := &brokenEntry{MemStore: mem, key: entryKey(manifestOf(3))}
	if _, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 3}}); err == nil {
		t.Fatal("a prune that could not read a kept entry reported success")
	}
	if after := mem.Keys(); !slices.Equal(after, before) {
		t.Fatalf("keys after = %v, before = %v; nothing may go while a kept entry's names are unknown", after, before)
	}
}

func TestTheSweepDeletesStraysAndNothingAKeptManifestNames(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	strays := []string{ObjectsDir + "stray", PacksDir + "9-0123456789abcdef.tar.gz"}
	for _, k := range strays {
		if _, err := st.Put(context.Background(), prefix+k, strings.NewReader("x"), PutCondition{}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 5}, Sweep: true})
	if err != nil || deleted != 2 {
		t.Fatalf("deleted = %d, %v; want the 2 strays", deleted, err)
	}
	for _, k := range strays {
		if stored(t, st, prefix+k) {
			t.Errorf("%s survived the sweep", k)
		}
	}
	for _, o := range []string{"shared", "g1", "g2", "g3", "g4", "g5"} {
		if !stored(t, st, prefix+ObjectsDir+o) {
			t.Errorf("the sweep deleted %s, which a kept manifest names", o)
		}
	}
}

func TestTheCacheSparesTheReadsOfKeptEntries(t *testing.T) {
	mem := NewMemStore(time.Now)
	cur := fiveGenerations(t, mem)
	st := &countingStore{MemStore: mem}
	cache := ObjectCache{}
	req := PruneRequest{Current: cur, Policy: Retention{Last: 4}, Cache: cache}
	if _, err := Prune(context.Background(), st, prefix, req); err != nil {
		t.Fatal(err)
	}
	if got := st.gets.Load(); got != 4 {
		t.Fatalf("the first prune read %d entries, want 4: three kept, one dropped", got)
	}
	st.gets.Store(0)
	if _, err := Prune(context.Background(), st, prefix, req); err != nil {
		t.Fatal(err)
	}
	if got := st.gets.Load(); got != 0 {
		t.Fatalf("the second prune read %d entries, want 0", got)
	}
	if len(cache) != 3 {
		t.Fatalf("cache holds %d entries, want the 3 kept", len(cache))
	}
}

type deleteLog struct {
	*MemStore
	mu   sync.Mutex
	keys []string
}

func (d *deleteLog) Delete(ctx context.Context, key string) error {
	d.mu.Lock()
	d.keys = append(d.keys, strings.TrimPrefix(key, prefix))
	d.mu.Unlock()
	return d.MemStore.Delete(ctx, key)
}

func TestHistoryEntriesGoBeforeTheObjectsOnlyTheyNamed(t *testing.T) {
	mem := NewMemStore(time.Now)
	cur := fiveGenerations(t, mem)
	st := &deleteLog{MemStore: mem}
	if _, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 2}}); err != nil {
		t.Fatal(err)
	}
	lastEntry, firstObject := -1, len(st.keys)
	for i, k := range st.keys {
		if strings.HasPrefix(k, HistoryDir) {
			lastEntry = i
		}
		if strings.HasPrefix(k, ObjectsDir) && i < firstObject {
			firstObject = i
		}
	}
	if lastEntry == -1 || firstObject == len(st.keys) || lastEntry > firstObject {
		t.Fatalf("deletes in order %v; every entry must go before the first object, or a restore could pick an entry whose objects are gone", st.keys)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/`
Expected: FAIL, `undefined: Prune`, `undefined: PruneRequest`, `undefined: ObjectCache`.

- [ ] **Step 3: Implement**

`internal/worldsync/prune.go`:

```go
package worldsync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type ObjectCache map[string][]string

type PruneRequest struct {
	Current  Manifest
	Replaced *Manifest
	Policy   Retention
	Cache    ObjectCache
	Sweep    bool
}

// Prune runs under the world's lease, right after a manifest commit. The
// sweep is safe only there: no other writer can have uploaded an object
// whose manifest is still to come.
func Prune(ctx context.Context, st Store, prefix string, req PruneRequest) (int, error) {
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		return 0, err
	}
	points := []Point{{Generation: req.Current.Generation, Taken: req.Current.TakenAt(time.Time{})}}
	var older, kept, dropped []HistoryEntry
	for _, e := range entries {
		if e.Generation >= req.Current.Generation {
			dropped = append(dropped, e)
			continue
		}
		older = append(older, e)
		points = append(points, e.Point)
	}
	keep := req.Policy.Select(points)
	for i, e := range older {
		if keep[i+1] {
			kept = append(kept, e)
		} else {
			dropped = append(dropped, e)
		}
	}

	cache := req.Cache
	if cache == nil {
		cache = ObjectCache{}
	}
	keptNames, err := entryObjects(ctx, st, kept, cache)
	if err != nil {
		return 0, err
	}
	droppedNames, err := entryObjects(ctx, st, dropped, cache)
	if err != nil {
		return 0, err
	}
	named := map[string]bool{}
	for _, f := range req.Current.Files {
		named[f.Object] = true
	}
	for _, objs := range keptNames {
		for _, o := range objs {
			named[o] = true
		}
	}
	candidates := map[string]bool{}
	if req.Replaced != nil {
		for _, f := range req.Replaced.Files {
			candidates[f.Object] = true
		}
	}
	for _, objs := range droppedNames {
		for _, o := range objs {
			candidates[o] = true
		}
	}

	// Entries first: an entry whose objects are gone would restore a broken world.
	var errs []error
	for _, e := range dropped {
		if err := st.Delete(ctx, e.Key); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(cache, e.Key)
	}
	if len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	deleted := 0
	for o := range candidates {
		if named[o] {
			continue
		}
		if err := st.Delete(ctx, prefix+o); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	if req.Sweep {
		for _, dir := range []string{ObjectsDir, PacksDir} {
			keys, err := st.List(ctx, prefix+dir)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, k := range keys {
				if named[strings.TrimPrefix(k, prefix)] {
					continue
				}
				if err := st.Delete(ctx, k); err != nil {
					errs = append(errs, err)
					continue
				}
				deleted++
			}
		}
	}
	keptKeys := map[string]bool{}
	for _, e := range kept {
		keptKeys[e.Key] = true
	}
	for k := range cache {
		if !keptKeys[k] {
			delete(cache, k)
		}
	}
	return deleted, errors.Join(errs...)
}

// entryObjects skips an entry that is gone: nothing can restore it, and
// nothing it named is kept by it.
func entryObjects(ctx context.Context, st Store, entries []HistoryEntry, cache ObjectCache) (map[string][]string, error) {
	out := make(map[string][]string, len(entries))
	var missing []HistoryEntry
	for _, e := range entries {
		if objs, ok := cache[e.Key]; ok {
			out[e.Key] = objs
		} else {
			missing = append(missing, e)
		}
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadParallel)
	for _, e := range missing {
		g.Go(func() error {
			m, err := readHistoryEntry(gctx, st, e.Key)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			var objs []string
			for _, f := range m.Files {
				if !seen[f.Object] {
					seen[f.Object] = true
					objs = append(objs, f.Object)
				}
			}
			mu.Lock()
			out[e.Key], cache[e.Key] = objs, objs
			mu.Unlock()
			return nil
		})
	}
	return out, g.Wait()
}
```

In `internal/worldsync/transfer.go`, `UploadSnapshot`, replace

```go
	dropUnnamed(ctx, st, prefix, prev, m)
	return m, info.ETag, nil
```

with

```go
	_, _ = Prune(ctx, st, prefix, PruneRequest{Current: m, Replaced: prev})
	return m, info.ETag, nil
```

and delete `func dropUnnamed` entirely. (`node.go` still calls `dropUnnamed` in `adoptCommitted`; in this task replace that one line with `_, _ = Prune(c, n.cfg.Store, n.prefix(s.World), PruneRequest{Current: m, Replaced: job.prev})`. Task 5 rewrites `adoptCommitted`.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 -race ./internal/worldsync/`
Expected: PASS, including `TestASecondGenerationUploadsOnlyWhatChangedAndCollectsTheRest` and `TestAnUploadCommittedBeforeACrashIsAdopted`, which pin that the zero policy collects what `dropUnnamed` collected.

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/worldsync/
git -C /home/paul/git/spawnery commit -m "feat(worldsync): prune by retention policy instead of dropping the previous generation" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: Snapshot time and the history write in the commit

**Files:**
- Modify: `internal/worldsync/snapshot.go` (`Snap` at line ~49, `TakeSnapshot` at line 130)
- Modify: `internal/worldsync/transfer.go` (`UploadSnapshot`, new `CommitSnapshot`, `putManifest`, `putHistory`)
- Modify callers of `TakeSnapshot`: `internal/worldsync/csi.go:163`, `internal/worldsync/node.go:747`, `internal/worldsync/snapshot_test.go:86,130,147,169`, `internal/worldsync/confine_test.go:186,236`, `internal/worldsync/transfer_test.go:36` (helper `snapshotOf`)
- Test: `internal/worldsync/commit_test.go`

**Interfaces:**
- Consumes: `HistoryKey`, `ListHistory`, `readHistoryEntry`, `Manifest.Taken`, `TakenAt` (Task 2); `Prune`, `PruneRequest`, `historyGenerations` (Task 3).
- Produces:
  ```go
  // Snap gains: Taken int64 `json:"taken,omitempty"` (unix ms, node clock)
  func TakeSnapshot(dataDir, snapDir string, keep prune.Keep, base []FileEntry, seq int64, taken time.Time) (Snap, error)
  func CommitSnapshot(ctx context.Context, st Store, prefix, snapDir string, prev *Manifest, prevETag string, keepPrev bool, newWorldID func() string) (Manifest, string, error)
  func putManifest(ctx context.Context, st Store, prefix string, body []byte, cond PutCondition) (ObjectInfo, error) // 412 whose stored body equals body is success; else ErrConflict
  func UploadSnapshot(ctx, st, prefix, snapDir string, prev *Manifest, prevETag string, newWorldID func() string) (Manifest, string, error) // unchanged signature: CommitSnapshot(keepPrev false) + Prune(zero policy)
  func snapTaken(seq int64) int64 // test helper in transfer_test.go: 1_700_000_000_000 + seq
  ```
  `CommitSnapshot` order: objects and pack (as today); if `keepPrev && prev != nil`, `prev` at `HistoryKey(prev.Generation, prev.TakenAt(<manifest LastModified>))` with `If-None-Match` (412 is fine; the HEAD for `LastModified` only when `prev.Taken == 0`); the manifest with `Taken = snap.Taken` (or now, if the snapshot predates the field), `If-Match`.

- [ ] **Step 1: Write the failing tests**

In `internal/worldsync/transfer_test.go`, change the helper `snapshotOf` and add `snapTaken`:

```go
func snapTaken(seq int64) int64 { return 1_700_000_000_000 + seq }

func snapshotOf(t *testing.T, data string, base []FileEntry, seq int64) string {
	t.Helper()
	snap := t.TempDir()
	if _, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), base, seq, time.UnixMilli(snapTaken(seq))); err != nil {
		t.Fatal(err)
	}
	return snap
}
```

In `snapshot_test.go` (lines 86, 130, 147, 169) and `confine_test.go` (lines 186, 236), append `, time.Unix(1_700_000_000, 0)` as the last argument of each `TakeSnapshot(...)` call; both files already import `time`.

`internal/worldsync/commit_test.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func firstGeneration(t *testing.T, st Store, data string, keepPrev bool) (Manifest, string) {
	t.Helper()
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	m, etag, err := CommitSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", keepPrev, NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 4, time.Unix(2, 0))
	return m, etag
}

func TestACommitPutsThePreviousManifestIntoHistory(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, true)
	m2, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, true, NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Taken != snapTaken(1) || m2.Taken != snapTaken(2) {
		t.Fatalf("taken = %d, %d; want the snapshots' %d, %d", m1.Taken, m2.Taken, snapTaken(1), snapTaken(2))
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil || len(entries) != 1 || entries[0].Generation != 1 || !entries[0].Taken.Equal(time.UnixMilli(m1.Taken)) {
		t.Fatalf("history = %+v, %v; want generation 1 at its snapshot's time", entries, err)
	}
	got, err := readHistoryEntry(ctx, st, entries[0].Key)
	if err != nil || !reflect.DeepEqual(got, m1) {
		t.Fatalf("entry = %+v, %v; want %+v", got, err, m1)
	}
}

func TestAHistoryEntryAnEarlierAttemptWroteIsNoError(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, true)
	key := prefix + HistoryKey(1, time.UnixMilli(m1.Taken))
	if _, err := st.Put(ctx, key, strings.NewReader(`{"earlier":"attempt"}`), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, true, NewWorldID); err != nil {
		t.Fatalf("a commit after an earlier attempt's history write: %v", err)
	}
	b, _, err := readObject(ctx, st, key)
	if err != nil || string(b) != `{"earlier":"attempt"}` {
		t.Fatalf("entry = %q, %v; the write must be If-None-Match", b, err)
	}
}

func TestACommitWithoutHistoryWritesNone(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, false)
	if _, _, err := CommitSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, false, NewWorldID); err != nil {
		t.Fatal(err)
	}
	if got := historyGenerations(t, st); len(got) != 0 {
		t.Fatalf("history = %v, want none", got)
	}
}

func TestAManifestWithoutTakenGoesToHistoryAtItsLastModified(t *testing.T) {
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := written
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, false)
	legacy := m1
	legacy.Taken = 0
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Put(ctx, prefix+ManifestName, bytes.NewReader(b), PutCondition{IfMatch: e1})
	if err != nil {
		t.Fatal(err)
	}
	now = written.Add(time.Hour)
	if _, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, legacy.Files, 2), &legacy, info.ETag, true, NewWorldID); err != nil {
		t.Fatal(err)
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil || len(entries) != 1 || !entries[0].Taken.Equal(written) {
		t.Fatalf("history = %+v, %v; want generation 1 at %v, when its manifest was written", entries, err, written)
	}
}

type failingPut struct {
	*MemStore
	key string
}

func (f *failingPut) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	if key == f.key {
		return ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Put(ctx, key, body, cond)
}

func TestACrashBeforeTheManifestLeavesAnEntryThePruneRemoves(t *testing.T) {
	mem := NewMemStore(time.Now)
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, mem, data, true)
	st := &failingPut{MemStore: mem, key: prefix + ManifestName}
	if _, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, true, NewWorldID); err == nil {
		t.Fatal("a commit whose manifest put failed succeeded")
	}
	if got := historyGenerations(t, mem); !slices.Equal(got, []int64{1}) {
		t.Fatalf("history = %v, want the entry of the still current generation 1", got)
	}
	if _, err := Prune(ctx, mem, prefix, PruneRequest{Current: m1, Policy: Retention{Last: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := historyGenerations(t, mem); len(got) != 0 {
		t.Fatalf("history = %v after the prune, want none", got)
	}
	if err := Download(ctx, mem, prefix, t.TempDir(), m1, 2, -1); err != nil {
		t.Fatalf("generation 1 lost an object to the prune: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/`
Expected: FAIL to compile: `too many arguments in call to TakeSnapshot`, `undefined: CommitSnapshot`.

- [ ] **Step 3: Implement**

`internal/worldsync/snapshot.go`: add `"time"` to the imports; the struct becomes

```go
type Snap struct {
	Seq int64 `json:"seq"`
	// Taken is unix milliseconds on the node's clock.
	Taken int64      `json:"taken,omitempty"`
	Files []SnapFile `json:"files"`
}
```

and `TakeSnapshot` takes `taken time.Time` as its last parameter; its `s := Snap{Seq: seq}` becomes `s := Snap{Seq: seq, Taken: taken.UnixMilli()}`.

`internal/worldsync/csi.go`: add `"time"` to the imports; in `Import`, `TakeSnapshot(dir, snap, k, nil, 1)` becomes `TakeSnapshot(dir, snap, k, nil, 1, time.Now())`.

`internal/worldsync/node.go`, in `func (n *Node) snapshot`: `TakeSnapshot(n.dataDir(s.World), dir, keep, base, seq)` becomes `TakeSnapshot(n.dataDir(s.World), dir, keep, base, seq, n.cfg.Clock())`.

`internal/worldsync/transfer.go`: rename the body of today's `UploadSnapshot` into `CommitSnapshot` with the new signature, and put the thin `UploadSnapshot` above it:

```go
func UploadSnapshot(ctx context.Context, st Store, prefix, snapDir string, prev *Manifest, prevETag string, newWorldID func() string) (Manifest, string, error) {
	m, etag, err := CommitSnapshot(ctx, st, prefix, snapDir, prev, prevETag, false, newWorldID)
	if err != nil {
		return Manifest{}, "", err
	}
	_, _ = Prune(ctx, st, prefix, PruneRequest{Current: m, Replaced: prev})
	return m, etag, nil
}

func CommitSnapshot(ctx context.Context, st Store, prefix, snapDir string, prev *Manifest, prevETag string, keepPrev bool, newWorldID func() string) (Manifest, string, error) {
```

Inside `CommitSnapshot`, keep everything up to and including `if err := g.Wait(); err != nil { … }`, then replace the rest of the function (from `body, err := json.Marshal(m)` to its end) with:

```go
	m.Taken = snap.Taken
	if m.Taken == 0 {
		m.Taken = time.Now().UnixMilli()
	}
	if keepPrev && prev != nil {
		if err := putHistory(ctx, st, prefix, *prev); err != nil {
			return Manifest{}, "", err
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, "", err
	}
	cond := PutCondition{IfNoneMatch: prev == nil, IfMatch: prevETag}
	if prev == nil {
		cond.IfMatch = ""
	}
	info, err := putManifest(ctx, st, prefix, body, cond)
	if err != nil {
		return Manifest{}, "", err
	}
	return m, info.ETag, nil
}

// putManifest: the SDK's retryer re-sends a conditional put after a 5xx or a
// reset connection; when the first attempt had committed, the retry fails
// its own condition.
func putManifest(ctx context.Context, st Store, prefix string, body []byte, cond PutCondition) (ObjectInfo, error) {
	info, err := st.Put(ctx, prefix+ManifestName, bytes.NewReader(body), cond)
	if errors.Is(err, ErrPrecondition) || errors.Is(err, ErrNotFound) {
		stored, storedInfo, rerr := readObject(ctx, st, prefix+ManifestName)
		if rerr != nil || !bytes.Equal(stored, body) {
			return ObjectInfo{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return storedInfo, nil
	}
	return info, err
}

// putHistory: a 412 is an earlier attempt of the same upload that already
// wrote the entry.
func putHistory(ctx context.Context, st Store, prefix string, m Manifest) error {
	var lastModified time.Time
	if m.Taken == 0 {
		info, err := st.Head(ctx, prefix+ManifestName)
		if err != nil {
			return fmt.Errorf("worldsync: when was generation %d written: %w", m.Generation, err)
		}
		lastModified = info.LastModified
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = st.Put(ctx, prefix+HistoryKey(m.Generation, m.TakenAt(lastModified)), bytes.NewReader(b), PutCondition{IfNoneMatch: true})
	if errors.Is(err, ErrPrecondition) {
		return nil
	}
	return err
}
```

(`transfer.go` already imports `bytes`, `encoding/json`, `errors`, `fmt` and `time`.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/ ./cmd/spawnery-worldsync/`
Expected: PASS. `TestAStaleManifestETagIsAConflict` and the 412 adoption test in `transfer_test.go` pin that `putManifest` kept the old behaviour.

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/worldsync/
git -C /home/paul/git/spawnery commit -m "feat(worldsync): record when a snapshot was taken, keep the previous manifest in history" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: The node agent keeps history

**Files:**
- Modify: `internal/worldsync/state.go` (`worldState`, `forgetContent`, new `manifest`)
- Modify: `internal/worldsync/node.go` (`Publish` after `s.LeaseETag = etag`; `startDownload`'s success line; `uploadJob`; `work`; `nextUpload`; `prepareUpload`; `finishUpload`; `adoptCommitted`; delete `dropStrayPacks`; `release`; new `refreshPolicy`, `pruneAfter`, `sweepAtRelease`, `groupOf`)
- Modify: `internal/worldsync/metrics.go` (`prunedObjects`, `Collectors`)
- Test: `internal/worldsync/node_history_test.go`

**Interfaces:**
- Consumes: `ReadRetention`, `WriteRetention`, `RetentionKey` (Task 2); `Prune`, `PruneRequest`, `ObjectCache`, `historyGenerations`, `stored` (Task 3); `CommitSnapshot`, `TakeSnapshot(…, taken)` (Task 4); test harness `newHarness`, `h.publish`, `h.request`, `h.unpublish`, `h.node`, `w = "ns/g/k"` (node_test.go).
- Produces:
  ```go
  // worldState gains, persisted: Taken int64 `json:"taken,omitempty"`; RestoredFrom int64 `json:"restoredFrom,omitempty"`
  // and in memory: policy Retention; policyETag string; policyRead bool; kept ObjectCache; prunes int
  func (s *worldState) manifest() Manifest
  const sweepEvery = 12
  var prunedObjects prometheus.Counter // spawnery_worldsync_pruned_objects_total, in Collectors()
  func setPolicy(t *testing.T, st Store, r Retention)            // test helper, node_history_test.go
  func (h *harness) upload(id, target string, size, seq int)     // test helper
  func packCount(t *testing.T, st Store) int                     // test helper
  ```

The policy is read at publish and right before each upload (after the lease check), with the last ETag. A read that fails keeps the last policy; before the first successful read (`policyRead` false) the upload copies the previous manifest into history and prune is skipped, so a policy the node cannot read never shrinks history. The prune after an upload runs outside the world's lock but inside the upload's window (`s.uploading` set, so `Publish` waits) and under the upload's deadline, which ends before the lease confirmed at the upload's start can go stale. Every 12th upload of a world sweeps; so does the release of its lease, after a renewal proves the lease is still this node's.

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/node_history_test.go`:

```go
package worldsync

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func setPolicy(t *testing.T, st Store, r Retention) {
	t.Helper()
	if err := WriteRetention(context.Background(), st, "", "ns", "g", r); err != nil {
		t.Fatal(err)
	}
}

// upload writes level.dat at size and has node id upload it as snapshot seq.
func (h *harness) upload(id, target string, size, seq int) {
	h.t.Helper()
	writeFile(h.t, filepath.Join(target, "worlds/world/level.dat"), size, h.now)
	h.request(target, strconv.Itoa(seq))
	h.node(id).Settle(context.Background())
}

func packCount(t *testing.T, st Store) int {
	t.Helper()
	keys, err := st.List(context.Background(), prefix+PacksDir)
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

func TestUploadsKeepTheGenerationsThePolicyKeeps(t *testing.T) {
	h := newHarness(t)
	setPolicy(t, h.st, Retention{Last: 3})
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		h.upload("a", target, 4+i, i)
	}
	m, _, err := ReadManifest(context.Background(), h.st, prefix)
	if err != nil || m.Generation != 4 {
		t.Fatalf("manifest = %+v, %v; want generation 4", m, err)
	}
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{3, 2}) {
		t.Fatalf("history = %v, want [3 2]: last 3 is the current and two more", got)
	}
	if got := packCount(t, h.st); got != 3 {
		t.Fatalf("packs = %d, want the 3 of the kept generations", got)
	}
}

func TestWithoutAPolicyAWorldKeepsOnlyItsCurrentGeneration(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if got := historyGenerations(t, h.st); len(got) != 0 {
		t.Fatalf("history = %v, want none", got)
	}
	if got := packCount(t, h.st); got != 1 {
		t.Fatalf("packs = %d, want 1", got)
	}
}

type failingKeys struct {
	*MemStore
	match func(key string) bool
}

func (f *failingKeys) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if f.match(key) {
		return nil, ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Get(ctx, key)
}

func (f *failingKeys) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if f.match(key) {
		return ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Head(ctx, key)
}

func TestAPolicyThatCannotBeReadKeepsHistoryAndDeletesNothing(t *testing.T) {
	h := newHarness(t)
	h.store = &failingKeys{MemStore: h.st, match: func(key string) bool { return strings.HasPrefix(key, ".retention/") }}
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{1}) {
		t.Fatalf("history = %v, want [1]: an unread policy may not drop a generation", got)
	}
	if got := packCount(t, h.st); got != 2 {
		t.Fatalf("packs = %d, want 2: nothing is deleted on a policy never read", got)
	}
}

func TestAMalformedPolicyKeepsWhatWasReadBefore(t *testing.T) {
	h := newHarness(t)
	setPolicy(t, h.st, Retention{Last: 3})
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if _, err := h.st.Put(context.Background(), RetentionKey("", "ns", "g"), strings.NewReader("last: 0"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	h.upload("a", target, 7, 3)
	h.upload("a", target, 8, 4)
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{3, 2}) {
		t.Fatalf("history = %v, want [3 2] under the last policy read", got)
	}
}

func TestAPolicyChangeReachesTheNextUpload(t *testing.T) {
	h := newHarness(t)
	setPolicy(t, h.st, Retention{Last: 5})
	target, _ := h.publish("a", w, "p1")
	for i := 1; i <= 3; i++ {
		h.upload("a", target, 4+i, i)
	}
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("history = %v, want [2 1]", got)
	}
	setPolicy(t, h.st, Retention{})
	h.upload("a", target, 8, 4)
	if got := historyGenerations(t, h.st); len(got) != 0 {
		t.Fatalf("history = %v after the policy was dropped, want none", got)
	}
	if got := packCount(t, h.st); got != 1 {
		t.Fatalf("packs = %d, want 1", got)
	}
}

func TestEveryTwelfthPruneSweepsTheWorld(t *testing.T) {
	h := newHarness(t)
	stray := prefix + ObjectsDir + strings.Repeat("f", 64)
	if _, err := h.st.Put(context.Background(), stray, strings.NewReader("left by a failed attempt"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	target, _ := h.publish("a", w, "p1")
	for i := 1; i <= 11; i++ {
		h.upload("a", target, 4+i, i)
		if !stored(t, h.st, stray) {
			t.Fatalf("upload %d swept the world; the sweep is every twelfth", i)
		}
	}
	h.upload("a", target, 16, 12)
	if stored(t, h.st, stray) {
		t.Fatal("the twelfth upload did not sweep the stray object")
	}
}

func TestTheLeasesReleaseSweepsTheWorld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	stray := prefix + PacksDir + "9-0123456789abcdef.tar.gz"
	if _, err := h.st.Put(ctx, stray, strings.NewReader("x"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(ctx)
	if stored(t, h.st, stray) {
		t.Fatal("the release did not sweep the stray pack")
	}
	if l, _, err := ReadLease(ctx, h.st, prefix); err != nil || l.Node != "" {
		t.Fatalf("lease = %+v, %v; want released after the sweep", l, err)
	}
}

func TestPrunedObjectsAreCounted(t *testing.T) {
	before := testutil.ToFloat64(prunedObjects)
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if got := testutil.ToFloat64(prunedObjects); got != before+1 {
		t.Fatalf("pruned objects = %v, want %v: the pack of generation 1", got, before+1)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/`
Expected: FAIL: `undefined: prunedObjects`; once that compiles, the history tests fail because the node still commits without history.

- [ ] **Step 3: Implement**

`internal/worldsync/metrics.go`: add to the `var` block

```go
	prunedObjects = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_worldsync_pruned_objects_total", Help: "Objects and packs that prunes deleted.",
	})
```

and append `prunedObjects` to the slice `Collectors()` returns.

`internal/worldsync/state.go`: in `worldState`, after `Files []FileEntry \`json:"files"\``:

```go
	Taken        int64 `json:"taken,omitempty"`
	RestoredFrom int64 `json:"restoredFrom,omitempty"`
```

and after `retryDelay time.Duration`:

```go
	// policy is the group's retention as last read; until policyRead, no
	// read has succeeded and nothing may be pruned.
	policy     Retention
	policyETag string
	policyRead bool
	kept       ObjectCache
	prunes     int
```

`forgetContent` becomes

```go
func (s *worldState) forgetContent() {
	s.WorldID, s.Generation, s.ManifestETag, s.Files = "", 0, "", nil
	s.Taken, s.RestoredFrom, s.kept = 0, 0, nil
	s.Pending, s.FinalPending, s.Incomplete = nil, false, false
}

func (s *worldState) manifest() Manifest {
	return Manifest{WorldID: s.WorldID, Generation: s.Generation, Taken: s.Taken, RestoredFrom: s.RestoredFrom, Files: s.Files}
}
```

`internal/worldsync/node.go`:

1. `uploadJob` becomes

```go
type uploadJob struct {
	seq         int64
	dir         string
	prev        *Manifest
	prevETag    string
	policy      Retention
	policyKnown bool
	cache       ObjectCache
	sweep       bool
}

func (j *uploadJob) keepPrev() bool { return !j.policyKnown || j.policy.KeepsHistory() }

const sweepEvery = 12
```

2. In `Publish`, replace

```go
	s.LeaseETag = etag

	fetch, metag, err := n.settleContent(ctx, s)
```

with

```go
	s.LeaseETag = etag
	s.kept = nil
	n.refreshPolicy(ctx, s)

	fetch, metag, err := n.settleContent(ctx, s)
```

3. In `startDownload`'s goroutine, after `s.WorldID, s.Generation, s.ManifestETag, s.Files, s.Incomplete = m.WorldID, m.Generation, etag, m.Files, false` add

```go
		s.Taken, s.RestoredFrom = m.Taken, m.RestoredFrom
```

4. In `work`, replace

```go
		m, etag, err := UploadSnapshot(uctx, n.cfg.Store, n.prefix(s.World), job.dir, job.prev, job.prevETag, NewWorldID)
		cancel()
		if err == nil {
			uploadSeconds.Observe(time.Since(started).Seconds())
		}
```

with

```go
		m, etag, err := CommitSnapshot(uctx, n.cfg.Store, n.prefix(s.World), job.dir, job.prev, job.prevETag, job.keepPrev(), NewWorldID)
		if err == nil {
			uploadSeconds.Observe(time.Since(started).Seconds())
			n.pruneAfter(uctx, s.World, job, m, job.sweep)
		}
		cancel()
```

5. In `nextUpload`, replace `	job, err := n.prepareUpload(s)` with

```go
	n.refreshPolicy(ctx, s)
	job, err := n.prepareUpload(s)
```

6. In `prepareUpload`, replace

```go
	job := &uploadJob{seq: seq, dir: dir}
	if s.WorldID != "" {
		job.prev = &Manifest{WorldID: s.WorldID, Generation: s.Generation, Files: s.Files}
		job.prevETag = s.ManifestETag
	}
	return job, nil
```

with

```go
	if s.kept == nil {
		s.kept = ObjectCache{}
	}
	s.prunes++
	job := &uploadJob{seq: seq, dir: dir, policy: s.policy, policyKnown: s.policyRead, cache: s.kept, sweep: s.prunes%sweepEvery == 0}
	if s.WorldID != "" {
		prev := s.manifest()
		job.prev = &prev
		job.prevETag = s.ManifestETag
	}
	return job, nil
```

7. In `finishUpload`, after `s.WorldID, s.Generation, s.ManifestETag, s.Files = m.WorldID, m.Generation, etag, m.Files` add

```go
	s.Taken, s.RestoredFrom = m.Taken, m.RestoredFrom
```

8. In `adoptCommitted`, replace everything from `	c, cancel := n.call(ctx)` to the end of the function with

```go
	n.cfg.Log.Info("adopted an upload that committed before its answer arrived", "world", s.World, "generation", m.Generation)
	pc, cancel := context.WithTimeout(ctx, n.uploadTimeout)
	defer cancel()
	// Swept: the retry that conflicted wrote its pack under a fresh name.
	n.pruneAfter(pc, s.World, job, m, true)
	return m, etag, nil
}
```

and delete `dropStrayPacks` with its comment.

9. In `release`, after `	n.dropScratch(s)` add `	n.sweepAtRelease(ctx, s)`.

10. Add the new methods (after `release`):

```go
func groupOf(world string) (namespace, group string) {
	parts := strings.SplitN(world, "/", 3)
	return parts[0], parts[1]
}

// refreshPolicy keeps the last policy read when a read fails.
func (n *Node) refreshPolicy(ctx context.Context, s *worldState) {
	ns, group := groupOf(s.World)
	c, cancel := n.call(ctx)
	defer cancel()
	r, etag, changed, err := ReadRetention(c, n.cfg.Store, n.cfg.Base, ns, group, s.policyETag)
	if err != nil {
		n.cfg.Log.Error(err, "could not read the group's retention; keeping the one read before", "world", s.World)
		return
	}
	if changed {
		s.policy, s.policyETag = r, etag
	}
	s.policyRead = true
}

func (n *Node) pruneAfter(ctx context.Context, world string, job *uploadJob, m Manifest, sweep bool) {
	if !job.policyKnown {
		return
	}
	deleted, err := Prune(ctx, n.cfg.Store, n.prefix(world), PruneRequest{
		Current: m, Replaced: job.prev, Policy: job.policy, Cache: job.cache, Sweep: sweep,
	})
	prunedObjects.Add(float64(deleted))
	if err != nil {
		n.cfg.Log.Error(err, "prune failed; the next one catches up", "world", world)
	}
}

// sweepAtRelease renews first: a lease another node took over must not
// sweep away objects that node uploaded.
func (n *Node) sweepAtRelease(ctx context.Context, s *worldState) {
	if s.WorldID == "" {
		return
	}
	if err := n.renewLease(ctx, s); err != nil {
		return
	}
	n.refreshPolicy(ctx, s)
	if !s.policyRead {
		return
	}
	if s.kept == nil {
		s.kept = ObjectCache{}
	}
	c, cancel := context.WithTimeout(ctx, n.uploadTimeout)
	defer cancel()
	deleted, err := Prune(c, n.cfg.Store, n.prefix(s.World), PruneRequest{
		Current: s.manifest(), Policy: s.policy, Cache: s.kept, Sweep: true,
	})
	prunedObjects.Add(float64(deleted))
	if err != nil {
		n.cfg.Log.Error(err, "the sweep at the lease's release failed; the next one catches up", "world", s.World)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 -race ./internal/worldsync/`
Expected: PASS, the whole package, `-race` included (the cache is touched by the upload worker outside the lock). If one of the gate tests in `node_test.go` (`TestAHungStoreCallTimesOut`, `TestPendingSnapshotsStayCountedWhileTheStoreHangs`, `TestAHungStoreCallHoldsUpNoOtherWorld`) now hangs or counts differently, the new store calls (policy reads, the renewal before the release sweep) met its gate: narrow that gate's `match` to the keys it was written for, and say so in the commit body.
Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery manifests metrics-docs-test`
Expected: exit 0; `docs/reference/metrics-and-alerts.md` lists `spawnery_worldsync_pruned_objects_total`.

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/worldsync/ docs/reference/metrics-and-alerts.md
git -C /home/paul/git/spawnery commit -m "feat(worldsync): the node agent keeps what the group's retention keeps and sweeps strays" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Restore and restore points in worldsync

**Files:**
- Create: `internal/worldsync/restore.go`
- Test: `internal/worldsync/restore_test.go`

**Interfaces:**
- Consumes: `readCurrent`, `TakenAt`, `ListHistory`, `HistoryKey`, `readHistoryEntry` (Task 2); `Prune` (Task 3); `putManifest` (Task 4); `TakeLease`, `ReleaseLease`, `StaleAfter`, `HeldError` (lease.go); `BucketWorlds` (deletion.go); test helpers `fiveGenerations`, `seedEntry`, `stored`, `historyGenerations` (Task 3), `setPolicy`, `h.upload` (Task 5), `committedButRefused`, `downloadsCounted`, `h.orphans`, `h.waitFile` (node_test.go).
- Produces:
  ```go
  const OperatorLeaseNode = "spawnery-operator"
  var ErrNoGeneration, ErrCurrentGeneration error
  type RestorePoint struct { Generation int64; Taken time.Time; Current bool }
  type Restored struct { Generation, RestoredFrom int64; RestoredTaken time.Time; PruneErr error }
  func ListRestorePoints(ctx context.Context, st Store, prefix string) ([]RestorePoint, error)      // ErrNotFound without a manifest
  func RestoreWorld(ctx context.Context, st Store, prefix string, generation int64, policy Retention, now time.Time) (Restored, error)
      // errors: ErrNotFound, *HeldError, ErrCurrentGeneration, ErrNoGeneration, ErrConflict, store errors
  func (b BucketWorlds) RestorePoints(ctx context.Context, world string) ([]RestorePoint, error)
  func (b BucketWorlds) Restore(ctx context.Context, world string, generation int64, policy Retention) (Restored, error)
  ```

`RestoreWorld` follows spec §4.5 steps 3 to 7 (steps 1, 2 and 8 are the operator's, Task 9): a HEAD on the manifest first, so a missing world gets `ErrNotFound` without a lease being created; the lease as `spawnery-operator`; the current manifest with its bytes and ETag; the entry of `generation` (only one older than the current counts); the current bytes at their history key (`If-None-Match`, 412 fine); the new manifest (same `worldId`, generation current+1, `taken` now, `restoredFrom`, the entry's files) through `putManifest` with `If-Match`; a prune without sweep; the lease released, also when the request's context was cancelled.

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/restore_test.go`:

```go
package worldsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTheRestorePointsAreTheCurrentAndItsOlderEntriesNewestFirst(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	seedEntry(t, st, cur) // left by a crash between the history write and the manifest put
	points, err := ListRestorePoints(context.Background(), st, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var gens []int64
	for i, p := range points {
		gens = append(gens, p.Generation)
		if p.Current != (i == 0) {
			t.Errorf("point %d current = %v", p.Generation, p.Current)
		}
	}
	if !slices.Equal(gens, []int64{5, 4, 3, 2, 1}) {
		t.Fatalf("generations = %v, want [5 4 3 2 1] with the current listed once", gens)
	}
	if !points[0].Taken.Equal(time.UnixMilli(cur.Taken)) {
		t.Fatalf("current taken = %v, want %v", points[0].Taken, time.UnixMilli(cur.Taken))
	}
}

func TestARestoreRefusesTheCurrentAnUnknownGenerationAndAMissingWorld(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	fiveGenerations(t, st)
	if _, err := RestoreWorld(ctx, st, prefix, 5, Retention{Last: 10}, time.Now()); !errors.Is(err, ErrCurrentGeneration) {
		t.Fatalf("restore of the current: err = %v, want ErrCurrentGeneration", err)
	}
	if _, err := RestoreWorld(ctx, st, prefix, 99, Retention{Last: 10}, time.Now()); !errors.Is(err, ErrNoGeneration) {
		t.Fatalf("restore of generation 99: err = %v, want ErrNoGeneration", err)
	}
	const none = "ns/g/none/"
	if _, err := RestoreWorld(ctx, st, none, 1, Retention{Last: 10}, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore of a missing world: err = %v, want ErrNotFound", err)
	}
	if stored(t, st, none+LeaseName) {
		t.Fatal("a restore of a missing world left a lease in the bucket")
	}
	m, _, err := ReadManifest(ctx, st, prefix)
	if err != nil || m.Generation != 5 {
		t.Fatalf("manifest = %+v, %v; a refused restore changed it", m, err)
	}
	if l, _, err := ReadLease(ctx, st, prefix); err != nil || l.Node != "" {
		t.Fatalf("lease = %+v, %v; want released", l, err)
	}
}

func TestARestoreWhoseCommitWasAnswered412IsARestore(t *testing.T) {
	mem := NewMemStore(time.Now)
	fiveGenerations(t, mem)
	st := &committedButRefused{MemStore: mem, match: func(key string) bool { return strings.HasSuffix(key, ManifestName) }}
	st.armed.Store(true)
	got, err := RestoreWorld(context.Background(), st, prefix, 2, Retention{Last: 10}, time.Now())
	if err != nil || got.Generation != 6 || got.RestoredFrom != 2 {
		t.Fatalf("restore = %+v, %v; want generation 6 from 2", got, err)
	}
	if st.refused.Load() == 0 {
		t.Fatal("the store refused nothing")
	}
}

func TestARestoreMakesAnOldGenerationCurrentUnderANewNumber(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setPolicy(t, h.st, Retention{Last: 10})
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	h.unpublish("a", target)
	h.node("a").Settle(ctx) // generation 3, the final snapshot, and the release
	before, _, err := ReadManifest(ctx, h.st, prefix)
	if err != nil || before.Generation != 3 {
		t.Fatalf("manifest = %+v, %v; want generation 3", before, err)
	}

	got, err := BucketWorlds{Store: h.st}.Restore(ctx, w, 1, Retention{Last: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 4 || got.RestoredFrom != 1 || got.PruneErr != nil {
		t.Fatalf("restore = %+v, want generation 4 from 1", got)
	}
	m, _, err := ReadManifest(ctx, h.st, prefix)
	if err != nil || m.Generation != 4 || m.RestoredFrom != 1 || m.WorldID != before.WorldID || len(m.Files) != 1 || m.Files[0].Size != 5 {
		t.Fatalf("manifest = %+v, %v; want generation 4 holding generation 1's level.dat of 5 bytes", m, err)
	}
	points, err := BucketWorlds{Store: h.st}.RestorePoints(ctx, w)
	if err != nil || len(points) != 4 || points[0].Generation != 4 || points[1].Generation != 3 {
		t.Fatalf("restore points = %+v, %v; want 4 (current), 3, 2, 1", points, err)
	}

	downloads := downloadsCounted(h.node("a"))
	target2, err := h.publish("a", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	h.waitFile(target2, ReadyFile)
	if got := downloadsCounted(h.node("a")); got != downloads+1 {
		t.Fatalf("downloads = %d, want %d: the node's copy of generation 3 was taken for current", got, downloads+1)
	}
	if got := h.orphans("a"); got != 0 {
		t.Fatalf("orphans = %d after a restore", got)
	}
	st, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat"))
	if err != nil || st.Size() != 5 {
		t.Fatalf("level.dat = %v, %v; want the restored 5 bytes", st, err)
	}
}

func TestARestoreIsRefusedWhileANodeHoldsTheLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setPolicy(t, h.st, Retention{Last: 10})
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	_, err := RestoreWorld(ctx, h.st, prefix, 1, Retention{Last: 10}, h.now)
	var held *HeldError
	if !errors.As(err, &held) || held.Node != "a" {
		t.Fatalf("err = %v, want HeldError{a}", err)
	}
	if m, _, err := ReadManifest(ctx, h.st, prefix); err != nil || m.Generation != 2 {
		t.Fatalf("manifest = %+v, %v; a refused restore changed it", m, err)
	}
}

func TestAPublishWaitsForARestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	etag, err := TakeLease(ctx, h.st, prefix, Lease{Node: OperatorLeaseNode, RenewedAt: h.now}, StaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.publish("b", w, "p1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("publish during a restore: err = %v, want ErrUnavailable", err)
	}
	if err := ReleaseLease(ctx, h.st, prefix, etag); err != nil {
		t.Fatal(err)
	}
	if _, err := h.publish("b", w, "p1"); err != nil {
		t.Fatalf("publish after the restore: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/worldsync/`
Expected: FAIL, `undefined: RestoreWorld`, `undefined: ListRestorePoints`, `undefined: OperatorLeaseNode`.

- [ ] **Step 3: Implement**

`internal/worldsync/restore.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
)

const OperatorLeaseNode = "spawnery-operator"

var (
	ErrNoGeneration      = errors.New("worldsync: the world keeps no such generation")
	ErrCurrentGeneration = errors.New("worldsync: that generation is the current one")
)

type RestorePoint struct {
	Generation int64
	Taken      time.Time
	Current    bool
}

type Restored struct {
	Generation    int64
	RestoredFrom  int64
	RestoredTaken time.Time
	// PruneErr: the restore committed and the prune after it failed; the
	// world's next prune catches up.
	PruneErr error
}

// ListRestorePoints leaves out an entry at or above the current generation:
// it is a crash's copy of the current one.
func ListRestorePoints(ctx context.Context, st Store, prefix string) ([]RestorePoint, error) {
	_, cur, info, err := readCurrent(ctx, st, prefix)
	if err != nil {
		return nil, err
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		return nil, err
	}
	points := []RestorePoint{{Generation: cur.Generation, Taken: cur.TakenAt(info.LastModified), Current: true}}
	for _, e := range entries {
		if e.Generation < cur.Generation {
			points = append(points, RestorePoint{Generation: e.Generation, Taken: e.Taken})
		}
	}
	return points, nil
}

// RestoreWorld writes the old files as generation current+1: reusing the
// old number would let a node holding another copy under it count that copy
// as current.
func RestoreWorld(ctx context.Context, st Store, prefix string, generation int64, policy Retention, now time.Time) (Restored, error) {
	if _, err := st.Head(ctx, prefix+ManifestName); err != nil {
		return Restored{}, err
	}
	leaseETag, err := TakeLease(ctx, st, prefix, Lease{Node: OperatorLeaseNode, Pod: "restore", RenewedAt: now}, StaleAfter)
	if err != nil {
		return Restored{}, err
	}
	defer func() { _ = ReleaseLease(context.WithoutCancel(ctx), st, prefix, leaseETag) }()

	raw, cur, info, err := readCurrent(ctx, st, prefix)
	if err != nil {
		return Restored{}, err
	}
	if generation == cur.Generation {
		return Restored{}, ErrCurrentGeneration
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		return Restored{}, err
	}
	var from *HistoryEntry
	for i := range entries {
		if entries[i].Generation == generation && generation < cur.Generation {
			from = &entries[i]
			break
		}
	}
	if from == nil {
		return Restored{}, ErrNoGeneration
	}
	old, err := readHistoryEntry(ctx, st, from.Key)
	if errors.Is(err, ErrNotFound) {
		return Restored{}, ErrNoGeneration
	}
	if err != nil {
		return Restored{}, err
	}

	_, err = st.Put(ctx, prefix+HistoryKey(cur.Generation, cur.TakenAt(info.LastModified)), bytes.NewReader(raw), PutCondition{IfNoneMatch: true})
	if err != nil && !errors.Is(err, ErrPrecondition) {
		return Restored{}, err
	}
	next := Manifest{WorldID: cur.WorldID, Generation: cur.Generation + 1, Taken: now.UnixMilli(), RestoredFrom: generation, Files: old.Files}
	body, err := json.Marshal(next)
	if err != nil {
		return Restored{}, err
	}
	if _, err := putManifest(ctx, st, prefix, body, PutCondition{IfMatch: info.ETag}); err != nil {
		return Restored{}, err
	}
	_, perr := Prune(ctx, st, prefix, PruneRequest{Current: next, Policy: policy})
	return Restored{Generation: next.Generation, RestoredFrom: generation, RestoredTaken: from.Taken, PruneErr: perr}, nil
}

func (b BucketWorlds) RestorePoints(ctx context.Context, world string) ([]RestorePoint, error) {
	return ListRestorePoints(ctx, b.Store, WorldPrefix(b.Base, world))
}

func (b BucketWorlds) Restore(ctx context.Context, world string, generation int64, policy Retention) (Restored, error) {
	return RestoreWorld(ctx, b.Store, WorldPrefix(b.Base, world), generation, policy, time.Now())
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 -race ./internal/worldsync/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/worldsync/
git -C /home/paul/git/spawnery commit -m "feat(worldsync): list restore points and restore a generation under the world's lease" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: `storage.retention` in the CRD

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (new `RetentionSpec` above `StorageSpec`; a `StorageSpec` marker beside the two at lines 171-172; field `Retention` after `Replace`; `WorldRetention` after `UsesObjectStore` at ~575)
- Generated: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/spawnery.cloud_servergroups.yaml`, `charts/spawnery/templates/crds.yaml`, `docs/reference/crds.md`
- Test: `api/v1alpha1/servergroup_envtest_test.go`, `api/v1alpha1/servergroup_types_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks; `worldsync.Retention` (Task 1) must stay convertible from `RetentionSpec`.
- Produces:
  ```go
  type RetentionSpec struct { Last, Hourly, Daily, Weekly, Monthly, Yearly int32 } // json last, hourly, daily, weekly, monthly, yearly, omitempty, Minimum=0
  StorageSpec.Retention *RetentionSpec `json:"retention,omitempty"`
  func (g *ServerGroup) WorldRetention() RetentionSpec // zero unless UsesObjectStore and Retention != nil
  ```
  CEL message, verbatim: `spec.storage.retention needs spec.storage.backend ObjectStore`.

- [ ] **Step 1: Write the failing tests**

Append to `api/v1alpha1/servergroup_envtest_test.go`:

```go
func objectStoreGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	g := onDemandGroup(ns, name)
	g.Spec.Storage.Backend = spawneryv1alpha1.StorageBackendObjectStore
	g.Spec.Storage.Keep = []string{"world"}
	return g
}

func TestRetentionNeedsTheObjectStore(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	const msg = "spec.storage.retention needs spec.storage.backend ObjectStore"

	o := onDemandGroup(ns, "retention-on-claims")
	o.Spec.Storage.Keep = []string{"world"}
	o.Spec.Storage.Retention = &spawneryv1alpha1.RetentionSpec{Last: 3}
	if err := c.Create(ctx, o); err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("retention on claims: err = %v, want %q", err, msg)
	}

	g := objectStoreGroup(ns, "retention-in-the-store")
	g.Spec.Storage.Retention = &spawneryv1alpha1.RetentionSpec{Last: 12, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 3}
	if err := c.Create(ctx, g); err != nil {
		t.Fatalf("retention in the object store: %v", err)
	}
	g.Spec.Storage.Backend = spawneryv1alpha1.StorageBackendClaim
	if err := c.Update(ctx, g); err == nil || !strings.Contains(err.Error(), msg) {
		t.Fatalf("switching back to Claim with retention set: err = %v, want %q", err, msg)
	}
}

func TestRetentionRefusesANegativeCount(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	g := objectStoreGroup(ns, "negative-retention")
	g.Spec.Storage.Retention = &spawneryv1alpha1.RetentionSpec{Daily: -1}
	if err := c.Create(ctx, g); err == nil || !strings.Contains(err.Error(), "should be greater than or equal to 0") {
		t.Fatalf("daily -1: err = %v, want a minimum of 0", err)
	}
}

func TestRetentionOptionsDefaultToZero(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	g := objectStoreGroup(ns, "sparse-retention")
	g.Spec.Storage.Retention = &spawneryv1alpha1.RetentionSpec{Daily: 7}
	if err := c.Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	var got spawneryv1alpha1.ServerGroup
	if err := c.Get(ctx, client.ObjectKeyFromObject(g), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Storage.Retention == nil || *got.Spec.Storage.Retention != (spawneryv1alpha1.RetentionSpec{Daily: 7}) {
		t.Fatalf("retention = %+v, want daily 7 and every other option 0", got.Spec.Storage.Retention)
	}
}
```

Append to `api/v1alpha1/servergroup_types_test.go` (package `v1alpha1`):

```go
func TestWorldRetentionIsTheObjectStoresOnly(t *testing.T) {
	r := &RetentionSpec{Last: 12, Daily: 7}
	g := &ServerGroup{Spec: ServerGroupSpec{Type: ServerGroupOnDemand, Storage: &StorageSpec{Keep: []string{"world"}, Retention: r}}}
	if got := g.WorldRetention(); got != (RetentionSpec{}) {
		t.Fatalf("a group on claims has world retention %+v", got)
	}
	g.Spec.Storage.Backend = StorageBackendObjectStore
	if got := g.WorldRetention(); got != *r {
		t.Fatalf("WorldRetention = %+v, want %+v", got, *r)
	}
	g.Spec.Storage.Retention = nil
	if got := g.WorldRetention(); got != (RetentionSpec{}) {
		t.Fatalf("WorldRetention without retention = %+v, want zero", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 -run 'Retention' ./api/v1alpha1/`
Expected: FAIL, `undefined: spawneryv1alpha1.RetentionSpec`.

- [ ] **Step 3: Implement**

In `api/v1alpha1/servergroup_types.go`, above the markers of `StorageSpec`:

```go
// RetentionSpec keeps older generations of an ObjectStore world, with the
// prune options of Proxmox Backup Server.
type RetentionSpec struct {
	// Last keeps the newest generations; the current one counts as one.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Last int32 `json:"last,omitempty"`
	// Hourly keeps the newest generation of each of that many hours.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Hourly int32 `json:"hourly,omitempty"`
	// Daily keeps the newest generation of each of that many days.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Daily int32 `json:"daily,omitempty"`
	// Weekly keeps the newest generation of each of that many ISO weeks.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Weekly int32 `json:"weekly,omitempty"`
	// Monthly keeps the newest generation of each of that many months.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Monthly int32 `json:"monthly,omitempty"`
	// Yearly keeps the newest generation of each of that many years.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Yearly int32 `json:"yearly,omitempty"`
}
```

Beside the two `XValidation` markers above `type StorageSpec struct`:

```go
// +kubebuilder:validation:XValidation:rule="!has(self.retention) || (has(self.backend) && self.backend == 'ObjectStore')",message="spec.storage.retention needs spec.storage.backend ObjectStore"
```

In `StorageSpec`, after `Replace`:

```go

	// Retention keeps older generations of each member's world restorable,
	// with the prune options of Proxmox Backup Server, applied in the order
	// last, hourly, daily, weekly, monthly, yearly. Each counts only periods
	// that hold a generation and skips a period an earlier option already
	// kept one from. Periods are UTC. The current generation is always kept
	// and counts as one of last. Unset, or all zero, a world keeps only its
	// current generation. A change reaches each world at its next upload and
	// restarts no member. Needs backend ObjectStore.
	// +optional
	Retention *RetentionSpec `json:"retention,omitempty"`
```

After `UsesObjectStore`:

```go
// WorldRetention converts to worldsync.Retention field for field.
func (g *ServerGroup) WorldRetention() RetentionSpec {
	if !g.UsesObjectStore() || g.Spec.Storage.Retention == nil {
		return RetentionSpec{}
	}
	return *g.Spec.Storage.Retention
}
```

Then: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery manifests generate`

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./api/v1alpha1/`
Expected: PASS.
Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery crd-docs-test`
Expected: exit 0 (`docs/reference/crds.md` shows `storage.retention` and its six fields).

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add api/ config/ charts/spawnery/templates/crds.yaml docs/reference/crds.md
git -C /home/paul/git/spawnery commit -m "feat(api): storage.retention for ObjectStore groups" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: The operator publishes each group's policy

**Files:**
- Create: `internal/controller/retention.go`
- Modify: `internal/controller/servergroup_controller.go` (struct field; the top of `Reconcile`; import `worldsync`)
- Modify: `internal/controller/setup.go` (`Options.Retention`; `newServerGroupReconciler`)
- Modify: `cmd/spawnery-operator/main.go` (~393-411: build `Policies`; ~446: pass it)
- Test: `internal/controller/retention_envtest_test.go`

**Interfaces:**
- Consumes: `worldsync.Retention`, `worldsync.Policies` (Tasks 1, 2); `ServerGroup.WorldRetention` (Task 7); test fixture `newFixture`, `groupReconciler`, `f.createObjectStoreGroup`, `f.createOnDemandGroup` (controller tests).
- Produces:
  ```go
  type RetentionPublisher interface {
      Sync(ctx context.Context, namespace, group string, r worldsync.Retention) error
  }
  // Options.Retention RetentionPublisher; ServerGroupReconciler.Retention RetentionPublisher (nil: world sync off)
  ```

Every reconcile publishes: the group's `WorldRetention()` while it exists, the zero policy (which deletes the file) when it is gone or being deleted. `Policies` remembers what it wrote, so the steady state costs no request. An error is logged and the next pass retries; sizing goes on regardless. A group deleted while the operator is down leaves its file, which nothing reads any more.

- [ ] **Step 1: Write the failing tests**

`internal/controller/retention_envtest_test.go`:

```go
package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
	"github.com/spawnery/spawnery/internal/worldsync"
)

type recordingRetention struct {
	mu     sync.Mutex
	synced map[string]worldsync.Retention
}

func (r *recordingRetention) Sync(_ context.Context, namespace, group string, p worldsync.Retention) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.synced == nil {
		r.synced = map[string]worldsync.Retention{}
	}
	r.synced[namespace+"/"+group] = p
	return nil
}

func (r *recordingRetention) get(key string) (worldsync.Retention, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.synced[key]
	return p, ok
}

func reconcileGroupNamed(t *testing.T, f *fixture, r *ServerGroupReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: name}}); err != nil {
		t.Fatalf("reconcile group %s: %v", name, err)
	}
}

func TestTheGroupsRetentionIsPublishedAndWithdrawn(t *testing.T) {
	f := newFixture(t)
	published := &recordingRetention{}
	r := groupReconciler(f)
	r.Retention = published
	key := f.ns + "/worlds"

	g := f.createObjectStoreGroup(t, "worlds")
	// The reconciler writes the status, so every edit starts from a fresh read.
	setRetention := func(policy *spawneryv1alpha1.RetentionSpec) {
		t.Helper()
		if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: g.Name}, g); err != nil {
			t.Fatal(err)
		}
		g.Spec.Storage.Retention = policy
		if err := f.c.Update(f.ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	setRetention(&spawneryv1alpha1.RetentionSpec{Last: 12, Daily: 7})
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{Last: 12, Daily: 7}) {
		t.Fatalf("published %+v (%v), want last 12, daily 7", got, ok)
	}

	setRetention(nil)
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{}) {
		t.Fatalf("published %+v (%v) after retention was dropped, want the zero policy", got, ok)
	}

	setRetention(&spawneryv1alpha1.RetentionSpec{Last: 3})
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{Last: 3}) {
		t.Fatalf("published %+v (%v), want last 3", got, ok)
	}
	if err := f.c.Delete(f.ctx, g); err != nil {
		t.Fatal(err)
	}
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(key); !ok || got != (worldsync.Retention{}) {
		t.Fatalf("published %+v (%v) after the group was deleted, want the zero policy", got, ok)
	}
}

func TestAGroupOnClaimsPublishesNoRetention(t *testing.T) {
	f := newFixture(t)
	published := &recordingRetention{}
	r := groupReconciler(f)
	r.Retention = published
	g := f.createOnDemandGroup(t, "claims", 5)
	reconcileGroupNamed(t, f, r, g.Name)
	if got, ok := published.get(f.ns + "/claims"); !ok || got != (worldsync.Retention{}) {
		t.Fatalf("published %+v (%v), want the zero policy", got, ok)
	}
}

func TestNewServerGroupReconcilerCarriesTheRetentionPublisher(t *testing.T) {
	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:         testenv.Scheme(t),
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	published := &recordingRetention{}
	got := newServerGroupReconciler(mgr, Options{Clock: time.Now, Retention: published}).Retention
	if got != RetentionPublisher(published) {
		t.Fatalf("Retention = %v; the operator's policy writer never reaches the reconciler", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 -run 'Retention' ./internal/controller/`
Expected: FAIL, `r.Retention undefined`, `unknown field Retention in struct literal of type Options`.

- [ ] **Step 3: Implement**

`internal/controller/retention.go`:

```go
package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/worldsync"
)

// RetentionPublisher puts a group's world retention where the node agents
// read it; the zero policy removes it.
type RetentionPublisher interface {
	Sync(ctx context.Context, namespace, group string, r worldsync.Retention) error
}

func (r *ServerGroupReconciler) publishRetention(ctx context.Context, namespace, group string, policy worldsync.Retention) {
	if r.Retention == nil {
		return
	}
	if err := r.Retention.Sync(ctx, namespace, group, policy); err != nil {
		log.FromContext(ctx).Error(err, "could not publish the group's world retention; the next pass tries again", "group", group)
	}
}
```

`internal/controller/servergroup_controller.go`: add to `ServerGroupReconciler`, after `ClaimReader`:

```go

	// Retention is nil when the operator runs without --world-sync.
	Retention RetentionPublisher
```

add the import `"github.com/spawnery/spawnery/internal/worldsync"`, and replace the head of `Reconcile`

```go
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		if apierrors.IsNotFound(err) {
			// No ServerGroup finalizer exists, so most deletions are only seen as
			// NotFound, and observe never runs again to expire the reservations.
			r.Expectations.forget(req.Namespace + "/" + req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// Owned Servers cascade and drain through their own finalizers.
		r.Expectations.forget(group.Namespace + "/" + group.Name)
		return ctrl.Result{}, nil
	}
```

with

```go
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		if apierrors.IsNotFound(err) {
			// No ServerGroup finalizer exists, so most deletions are only seen as
			// NotFound, and observe never runs again to expire the reservations.
			r.Expectations.forget(req.Namespace + "/" + req.Name)
			r.publishRetention(ctx, req.Namespace, req.Name, worldsync.Retention{})
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// Owned Servers cascade and drain through their own finalizers.
		r.Expectations.forget(group.Namespace + "/" + group.Name)
		r.publishRetention(ctx, group.Namespace, group.Name, worldsync.Retention{})
		return ctrl.Result{}, nil
	}
	r.publishRetention(ctx, group.Namespace, group.Name, worldsync.Retention(group.WorldRetention()))
```

`internal/controller/setup.go`: in `Options`, after `WorldSyncInterval`:

```go
	// Retention publishes each group's storage.retention for the node
	// agents; nil without world sync.
	Retention RetentionPublisher
```

and in `newServerGroupReconciler` add `Retention: opts.Retention,` after `ClaimReader: mgr.GetAPIReader(),`.

`cmd/spawnery-operator/main.go`: replace `	var worlds agentserver.WorldDeleter` with

```go
	var worlds agentserver.WorldDeleter
	// An interface left nil, not a nil *Policies, when world sync is off.
	var retention controller.RetentionPublisher
```

after `		worlds = worldsync.BucketWorlds{Store: st, Base: base}` add `		retention = &worldsync.Policies{Store: st, Base: base}`, and in the `controller.Options{…}` literal add `Retention: retention,` after `WorldSyncInterval: worldSyncInterval,`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/controller/ ./cmd/spawnery-operator/`
Expected: PASS (envtest; several minutes on the dev VM).

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/controller/ cmd/spawnery-operator/main.go
git -C /home/paul/git/spawnery commit -m "feat(controller): publish each group's world retention to the bucket" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Protocol and the operator's answers

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto` (`CloudRequest` oneof after `execute = 15`; `CloudResponse` oneof after `execute = 16`; new messages after `DeleteServerResult`)
- Generated: `internal/agentpb/*.pb.go`, `agent/common/src/proto/java/**` (`make proto`)
- Create: `internal/agentserver/worldhistory.go`
- Modify: `internal/agentserver/writer.go` (`ClusterWriter`, `KubeWriter.History`; import `worldsync`), `internal/agentserver/requests.go` (`answerCloudRequest` switch, ~:142), `internal/agentserver/metrics.go` (`WorldRestores`), `cmd/spawnery-operator/main.go` (`History`)
- Test: `internal/agentserver/worldhistory_test.go`

**Interfaces:**
- Consumes: `worldsync.RestorePoint`, `worldsync.Restored`, `worldsync.Retention`, `worldsync.HeldError`, `worldsync.ErrNotFound`, `worldsync.ErrNoGeneration`, `worldsync.ErrCurrentGeneration`, `worldsync.ErrConflict`, `BucketWorlds.RestorePoints/Restore` (Task 6); `WorldRetention` (Task 7); test helpers `fakeClient`, `fakeWorlds`, `objectStoreGroup` (deleteserver_test.go).
- Produces:
  ```go
  // proto: ListRestorePointsRequest{group=1,key=2}, RestorePoint{generation=1,taken_unix_millis=2,current=3},
  //        ListRestorePointsResult{repeated RestorePoint points=1}, RestoreWorldRequest{group=1,key=2,generation=3},
  //        RestoreWorldResult{generation=1,restored_from=2};
  //        CloudRequest: list_restore_points=16, restore_world=17; CloudResponse: list_restore_points=17, restore_world=18
  type WorldHistory interface {
      RestorePoints(ctx context.Context, world string) ([]worldsync.RestorePoint, error)
      Restore(ctx context.Context, world string, generation int64, policy worldsync.Retention) (worldsync.Restored, error)
  }
  // KubeWriter.History WorldHistory (nil without --world-sync)
  // ClusterWriter gains:
  ListRestorePoints(ctx context.Context, namespace, group, key string) ([]worldsync.RestorePoint, error)
  RestoreWorld(ctx context.Context, namespace, group, key string, generation int64) (worldsync.Restored, error)
  var ErrNotObjectStore, ErrMemberRunning, ErrNoWorld, ErrNoGeneration, ErrCurrentGeneration, ErrWorldBusy error
  var WorldRestores *prometheus.CounterVec // spawnery_world_restores_total{result}
  ```

Reasons, from spec §3.2 with one refinement: `NOT_FOUND` for no such group, no world, no such generation; `REFUSED` for a group that is not on-demand or not `ObjectStore`, a bad key, a running member, a world marked for deletion, the current generation; `UNAVAILABLE` without world sync, for a held lease or a lost manifest race, and (refinement) for a member whose `Server` is still being deleted. Listing refuses neither a running member nor a marked world. The restore counter: `restored`; `refused` for `NOT_FOUND` and `REFUSED`; `unavailable`; `failed` for an error none of the above names, which the plugin hears as `UNAVAILABLE`. The event `WorldRestored` (action `RestoreWorld`) goes on the ServerGroup with the note `world <key> restored to generation <n> of <taken, RFC 3339 UTC>`.

- [ ] **Step 1: The protocol**

In `proto/spawnery/agent/v1alpha1/agent.proto`, add to `CloudRequest`'s oneof after `ExecuteRequest execute = 15;`:

```proto
    ListRestorePointsRequest list_restore_points = 16;
    RestoreWorldRequest restore_world = 17;
```

to `CloudResponse`'s oneof after `ExecuteResult execute = 16;`:

```proto
    ListRestorePointsResult list_restore_points = 17;
    RestoreWorldResult restore_world = 18;
```

and after `message DeleteServerResult { … }`:

```proto
// ListRestorePointsRequest asks for the generations of an on-demand member's
// world that a RestoreWorldRequest can go back to. Only for a group whose
// worlds live in the object store.
message ListRestorePointsRequest {
  string group = 1;
  string key = 2;
}

// RestorePoint is one kept generation of a world.
message RestorePoint {
  int64 generation = 1;
  // When the node took the snapshot, not when it reached the object store.
  int64 taken_unix_millis = 2;
  // The world as it stands; a restore cannot pick it.
  bool current = 3;
}

// ListRestorePointsResult: the current generation first, then the older
// ones, newest first.
message ListRestorePointsResult {
  repeated RestorePoint points = 1;
}

// RestoreWorldRequest makes an older generation the member's current world,
// as a new generation, while the member is stopped.
message RestoreWorldRequest {
  string group = 1;
  string key = 2;
  int64 generation = 3;
}

// RestoreWorldResult names the new current generation and the one whose
// content it holds.
message RestoreWorldResult {
  int64 generation = 1;
  int64 restored_from = 2;
}
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery proto`
Then: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery build ./...`
Expected: both exit 0; `git -C /home/paul/git/spawnery status --short` lists the proto, `internal/agentpb/agent.pb.go` and new files under `agent/common/src/proto/java/cloud/spawnery/agent/pb/`.

- [ ] **Step 2: Write the failing tests**

`internal/agentserver/worldhistory_test.go`:

```go
package agentserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/worldsync"
)

var historyCaller = grpcauth.Identity{Namespace: "minecraft", PodName: "gateway-0", PodUID: "proxy-history", Role: agent.RoleProxy}

type fakeHistory struct {
	points   []worldsync.RestorePoint
	restored worldsync.Restored
	err      error
	asked    []int64
	policy   worldsync.Retention
}

func (f *fakeHistory) RestorePoints(context.Context, string) ([]worldsync.RestorePoint, error) {
	return f.points, f.err
}

func (f *fakeHistory) Restore(_ context.Context, _ string, generation int64, policy worldsync.Retention) (worldsync.Restored, error) {
	f.asked = append(f.asked, generation)
	f.policy = policy
	return f.restored, f.err
}

func historyServer(t *testing.T, worlds *fakeWorlds, history *fakeHistory, objs ...client.Object) (*Server, *events.FakeRecorder) {
	t.Helper()
	c := fakeClient(t, objs...)
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now}
	if worlds != nil {
		w.Worlds = worlds
	}
	if history != nil {
		w.History = history
	}
	rec := events.NewFakeRecorder(16)
	return &Server{opts: Options{Writer: w, State: netstate.Source{Reader: c}, Recorder: rec}, requestRate: newRequestLimiter(time.Now)}, rec
}

func askRestore(s *Server, generation int64) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), historyCaller, &agentpb.CloudRequest{
		Id: 7, Request: &agentpb.CloudRequest_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldRequest{
			Group: "private-servers", Key: "c0ffee", Generation: generation,
		}},
	})
}

func askRestorePoints(s *Server) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), historyCaller, &agentpb.CloudRequest{
		Id: 8, Request: &agentpb.CloudRequest_ListRestorePoints{ListRestorePoints: &agentpb.ListRestorePointsRequest{
			Group: "private-servers", Key: "c0ffee",
		}},
	})
}

func historyMember(p phase.Phase, stopping bool) *spawneryv1alpha1.Server {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-c0ffee", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"}, Key: "c0ffee"},
		Status:     spawneryv1alpha1.ServerStatus{Phase: string(p)},
	}
	if stopping {
		now := metav1.Now()
		srv.Finalizers = []string{"spawnery.cloud/test"}
		srv.DeletionTimestamp = &now
	}
	return srv
}

func claimGroup() *spawneryv1alpha1.ServerGroup {
	g := objectStoreGroup()
	g.Spec.Storage = &spawneryv1alpha1.StorageSpec{Keep: []string{"world"}}
	return g
}

func TestRestoreWorldAnswersEveryReasonOfTheAPI(t *testing.T) {
	store := []client.Object{objectStoreGroup()}
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		worlds  *fakeWorlds
		history *fakeHistory
		want    agentpb.RequestError_Reason
		result  string
	}{
		{"no such group", nil, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_NOT_FOUND, "refused"},
		{"a group on claims", []client.Object{claimGroup()}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_REFUSED, "refused"},
		{"world sync is off", store, nil, nil, agentpb.RequestError_UNAVAILABLE, "unavailable"},
		{"the member runs", []client.Object{objectStoreGroup(), historyMember(phase.Ready, false)}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_REFUSED, "refused"},
		{"the member is stopping", []client.Object{objectStoreGroup(), historyMember(phase.Ready, true)}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_UNAVAILABLE, "unavailable"},
		{"the world is being deleted", store, &fakeWorlds{pending: true}, &fakeHistory{}, agentpb.RequestError_REFUSED, "refused"},
		{"a node holds the lease", store, &fakeWorlds{}, &fakeHistory{err: &worldsync.HeldError{Node: "node-a"}}, agentpb.RequestError_UNAVAILABLE, "unavailable"},
		{"another restore committed first", store, &fakeWorlds{}, &fakeHistory{err: fmt.Errorf("%w: 412", worldsync.ErrConflict)}, agentpb.RequestError_UNAVAILABLE, "unavailable"},
		{"the key has no world", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrNotFound}, agentpb.RequestError_NOT_FOUND, "refused"},
		{"no such generation", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrNoGeneration}, agentpb.RequestError_NOT_FOUND, "refused"},
		{"the current generation", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrCurrentGeneration}, agentpb.RequestError_REFUSED, "refused"},
		{"the store fails", store, &fakeWorlds{}, &fakeHistory{err: errors.New("store down")}, agentpb.RequestError_UNAVAILABLE, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := historyServer(t, tc.worlds, tc.history, tc.objs...)
			before := testutil.ToFloat64(WorldRestores.WithLabelValues(tc.result))
			resp := askRestore(s, 3)
			if got := resp.GetError().GetReason(); got != tc.want {
				t.Fatalf("reason = %v (%q), want %v", got, resp.GetError().GetMessage(), tc.want)
			}
			if got := testutil.ToFloat64(WorldRestores.WithLabelValues(tc.result)); got != before+1 {
				t.Fatalf("restores{result=%q} = %v, want %v", tc.result, got, before+1)
			}
		})
	}
}

func TestARestoreAnswersTheNewGenerationAndIsRecordedOnTheGroup(t *testing.T) {
	g := objectStoreGroup()
	g.Spec.Storage.Retention = &spawneryv1alpha1.RetentionSpec{Last: 12, Daily: 7}
	history := &fakeHistory{restored: worldsync.Restored{Generation: 9, RestoredFrom: 3, RestoredTaken: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)}}
	s, rec := historyServer(t, &fakeWorlds{}, history, g, historyMember(phase.Failed, false))
	before := testutil.ToFloat64(WorldRestores.WithLabelValues("restored"))

	got := askRestore(s, 3).GetRestoreWorld()
	if got == nil || got.GetGeneration() != 9 || got.GetRestoredFrom() != 3 {
		t.Fatalf("result = %+v, want generation 9 from 3", got)
	}
	if len(history.asked) != 1 || history.asked[0] != 3 || history.policy != (worldsync.Retention{Last: 12, Daily: 7}) {
		t.Fatalf("asked %v with %+v; want generation 3 under the group's retention", history.asked, history.policy)
	}
	if after := testutil.ToFloat64(WorldRestores.WithLabelValues("restored")); after != before+1 {
		t.Fatalf("restores{result=restored} = %v, want %v", after, before+1)
	}
	select {
	case ev := <-rec.Events:
		for _, want := range []string{"WorldRestored", "world c0ffee restored to generation 3 of 2026-10-08T10:00:00Z"} {
			if !strings.Contains(ev, want) {
				t.Errorf("event %q does not say %q", ev, want)
			}
		}
	default:
		t.Fatal("no event was recorded")
	}
}

func TestRestorePointsAreAnsweredNewestFirstWithTheirTimes(t *testing.T) {
	taken := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	history := &fakeHistory{points: []worldsync.RestorePoint{
		{Generation: 9, Taken: taken, Current: true},
		{Generation: 7, Taken: taken.Add(-time.Hour)},
	}}
	s, _ := historyServer(t, &fakeWorlds{}, history, objectStoreGroup(), historyMember(phase.Ready, false))
	points := askRestorePoints(s).GetListRestorePoints().GetPoints()
	if len(points) != 2 ||
		points[0].GetGeneration() != 9 || !points[0].GetCurrent() || points[0].GetTakenUnixMillis() != taken.UnixMilli() ||
		points[1].GetGeneration() != 7 || points[1].GetCurrent() || points[1].GetTakenUnixMillis() != taken.Add(-time.Hour).UnixMilli() {
		t.Fatalf("points = %v; a running member's restore points are listed too", points)
	}
}

func TestRestorePointsRefusals(t *testing.T) {
	store := []client.Object{objectStoreGroup()}
	for _, tc := range []struct {
		name    string
		objs    []client.Object
		worlds  *fakeWorlds
		history *fakeHistory
		want    agentpb.RequestError_Reason
	}{
		{"no such group", nil, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_NOT_FOUND},
		{"a group on claims", []client.Object{claimGroup()}, &fakeWorlds{}, &fakeHistory{}, agentpb.RequestError_REFUSED},
		{"world sync is off", store, nil, nil, agentpb.RequestError_UNAVAILABLE},
		{"the key has no world", store, &fakeWorlds{}, &fakeHistory{err: worldsync.ErrNotFound}, agentpb.RequestError_NOT_FOUND},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := historyServer(t, tc.worlds, tc.history, tc.objs...)
			if got := askRestorePoints(s).GetError().GetReason(); got != tc.want {
				t.Fatalf("reason = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 -run 'Restore' ./internal/agentserver/`
Expected: FAIL, `undefined: WorldRestores`, `w.History undefined`.

- [ ] **Step 4: Implement**

`internal/agentserver/metrics.go`: add

```go
var WorldRestores = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_world_restores_total",
		Help: "World restores plugins asked for, by result: restored, refused, unavailable or failed.",
	},
	[]string{"result"},
)
```

register it in `init` (`metrics.Registry.MustRegister(…, WorldRestores)`) and create its four series there, beside the `ConnectionsRefused` ones:

```go
	for _, r := range []string{"restored", "refused", "unavailable", "failed"} {
		WorldRestores.WithLabelValues(r)
	}
```

`internal/agentserver/writer.go`: add the import `"github.com/spawnery/spawnery/internal/worldsync"`; add to `ClusterWriter`, after `DeleteServer`:

```go
	// ListRestorePoints lists the kept generations of a member's world,
	// newest first. It returns ErrNoSuchGroup, ErrGroupNotOnDemand,
	// ErrNotObjectStore, instance.ErrBadKey, ErrWorldSyncOff and ErrNoWorld.
	ListRestorePoints(ctx context.Context, namespace, group, key string) ([]worldsync.RestorePoint, error)
	// RestoreWorld makes generation the member's current world as a new
	// generation. Beside ListRestorePoints' errors it returns
	// ErrMemberRunning, ErrInstanceStopping, ErrWorldDeleting,
	// ErrNoGeneration, ErrCurrentGeneration and ErrWorldBusy.
	RestoreWorld(ctx context.Context, namespace, group, key string, generation int64) (worldsync.Restored, error)
```

and to `KubeWriter`, after `Worlds WorldDeleter`:

```go
	// Nil when the operator runs without --world-sync.
	History WorldHistory
```

`internal/agentserver/worldhistory.go`:

```go
package agentserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/worldsync"
)

var (
	ErrNotObjectStore    = errors.New("that group keeps its worlds on claims, which keep no history")
	ErrMemberRunning     = errors.New("that member is running")
	ErrNoWorld           = errors.New("that key has no world")
	ErrNoGeneration      = errors.New("that world keeps no such generation")
	ErrCurrentGeneration = errors.New("that generation is the world's current one")
	ErrWorldBusy         = errors.New("that world is being written")
)

// WorldHistory is the operator's hold on the older generations of the
// worlds in a bucket. Worlds are named "<namespace>/<group>/<key>".
type WorldHistory interface {
	RestorePoints(ctx context.Context, world string) ([]worldsync.RestorePoint, error)
	Restore(ctx context.Context, world string, generation int64, policy worldsync.Retention) (worldsync.Restored, error)
}

// restoreTimeout ends a restore long before the lease it holds goes stale.
const restoreTimeout = time.Minute

func (w KubeWriter) historyGroup(ctx context.Context, namespace, group, key string) (*spawneryv1alpha1.ServerGroup, string, error) {
	name, err := instance.Name(group, key)
	if err != nil {
		return nil, "", err
	}
	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", ErrNoSuchGroup
		}
		return nil, "", err
	}
	if !g.IsOnDemand() {
		return nil, "", ErrGroupNotOnDemand
	}
	if !g.UsesObjectStore() {
		return nil, "", ErrNotObjectStore
	}
	if w.Worlds == nil || w.History == nil {
		return nil, "", ErrWorldSyncOff
	}
	return &g, name, nil
}

func (w KubeWriter) ListRestorePoints(ctx context.Context, namespace, group, key string) ([]worldsync.RestorePoint, error) {
	if _, _, err := w.historyGroup(ctx, namespace, group, key); err != nil {
		return nil, err
	}
	points, err := w.History.RestorePoints(ctx, namespace+"/"+group+"/"+key)
	if errors.Is(err, worldsync.ErrNotFound) {
		return nil, ErrNoWorld
	}
	return points, err
}

func (w KubeWriter) RestoreWorld(ctx context.Context, namespace, group, key string, generation int64) (worldsync.Restored, error) {
	g, name, err := w.historyGroup(ctx, namespace, group, key)
	if err != nil {
		return worldsync.Restored{}, err
	}
	var srv spawneryv1alpha1.Server
	err = w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv)
	switch {
	case err == nil && srv.Spec.GroupRef.Name == group && srv.Spec.Key == key:
		if !srv.DeletionTimestamp.IsZero() {
			return worldsync.Restored{}, ErrInstanceStopping
		}
		if !phase.Terminal(phase.Phase(srv.Status.Phase)) {
			return worldsync.Restored{}, ErrMemberRunning
		}
	case err != nil && !apierrors.IsNotFound(err):
		return worldsync.Restored{}, err
	}
	world := namespace + "/" + group + "/" + key
	pending, err := w.Worlds.DeletionPending(ctx, world)
	if err != nil {
		return worldsync.Restored{}, err
	}
	if pending {
		return worldsync.Restored{}, ErrWorldDeleting
	}
	rctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	restored, err := w.History.Restore(rctx, world, generation, worldsync.Retention(g.WorldRetention()))
	var held *worldsync.HeldError
	switch {
	case errors.As(err, &held), errors.Is(err, worldsync.ErrConflict):
		return worldsync.Restored{}, ErrWorldBusy
	case errors.Is(err, worldsync.ErrNotFound):
		return worldsync.Restored{}, ErrNoWorld
	case errors.Is(err, worldsync.ErrNoGeneration):
		return worldsync.Restored{}, ErrNoGeneration
	case errors.Is(err, worldsync.ErrCurrentGeneration):
		return worldsync.Restored{}, ErrCurrentGeneration
	}
	return restored, err
}

func historyRefusal(err error) (agentpb.RequestError_Reason, string, bool) {
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return agentpb.RequestError_NOT_FOUND, "no group by that name is on this network", true
	case errors.Is(err, ErrNoWorld):
		return agentpb.RequestError_NOT_FOUND, "that key has no world in the object store", true
	case errors.Is(err, ErrNoGeneration):
		return agentpb.RequestError_NOT_FOUND, "that world keeps no generation of that number", true
	case errors.Is(err, ErrGroupNotOnDemand):
		return agentpb.RequestError_REFUSED, "that group is not on-demand, so its members have no world of their own", true
	case errors.Is(err, ErrNotObjectStore):
		return agentpb.RequestError_REFUSED, "that group keeps its worlds on claims, which keep no history", true
	case errors.Is(err, instance.ErrBadKey):
		return agentpb.RequestError_REFUSED, err.Error(), true
	case errors.Is(err, ErrMemberRunning):
		return agentpb.RequestError_REFUSED, "that member is running; a world goes back only while its server is stopped", true
	case errors.Is(err, ErrWorldDeleting):
		return agentpb.RequestError_REFUSED, "that world is being deleted", true
	case errors.Is(err, ErrCurrentGeneration):
		return agentpb.RequestError_REFUSED, "that generation is already the world's current one", true
	case errors.Is(err, ErrWorldSyncOff):
		return agentpb.RequestError_REFUSED, "that group keeps its worlds in an object store, and this operator runs without --world-sync", true
	case errors.Is(err, ErrInstanceStopping):
		return agentpb.RequestError_UNAVAILABLE, "that member is still stopping; ask again once it is gone", true
	case errors.Is(err, ErrWorldBusy):
		return agentpb.RequestError_UNAVAILABLE, "that world is being written, by the upload after a stop or by another restore; ask again in a few seconds", true
	}
	return agentpb.RequestError_REASON_UNSPECIFIED, "", false
}

func (s *Server) answerListRestorePoints(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ListRestorePointsRequest,
) *agentpb.CloudResponse {
	points, err := s.opts.Writer.ListRestorePoints(ctx, id.Namespace, req.GetGroup(), req.GetKey())
	if err != nil {
		if reason, message, known := historyRefusal(err); known {
			return refuse(reqID, reason, message)
		}
		logger.V(1).Info("could not list restore points", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read that world's history just now")
	}
	out := make([]*agentpb.RestorePoint, 0, len(points))
	for _, p := range points {
		out = append(out, &agentpb.RestorePoint{Generation: p.Generation, TakenUnixMillis: p.Taken.UnixMilli(), Current: p.Current})
	}
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_ListRestorePoints{ListRestorePoints: &agentpb.ListRestorePointsResult{Points: out}},
	}
}

func (s *Server) answerRestoreWorld(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.RestoreWorldRequest,
) *agentpb.CloudResponse {
	restored, err := s.opts.Writer.RestoreWorld(ctx, id.Namespace, req.GetGroup(), req.GetKey(), req.GetGeneration())
	if err != nil {
		reason, message, known := historyRefusal(err)
		switch {
		case !known:
			WorldRestores.WithLabelValues("failed").Inc()
			logger.Info("could not restore a world", "group", req.GetGroup(), "key", req.GetKey(),
				"generation", req.GetGeneration(), "reason", err.Error())
			return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not restore that world just now")
		case reason == agentpb.RequestError_UNAVAILABLE:
			WorldRestores.WithLabelValues("unavailable").Inc()
		default:
			WorldRestores.WithLabelValues("refused").Inc()
		}
		return refuse(reqID, reason, message)
	}
	WorldRestores.WithLabelValues("restored").Inc()
	logger.Info("restored a world", "group", req.GetGroup(), "key", req.GetKey(),
		"generation", restored.Generation, "restoredFrom", restored.RestoredFrom, "pod", id.PodName)
	if restored.PruneErr != nil {
		logger.Error(restored.PruneErr, "the prune after a restore failed; the world's next prune catches up",
			"group", req.GetGroup(), "key", req.GetKey())
	}
	s.recordOnGroup(ctx, id.Namespace, req.GetGroup(), corev1.EventTypeNormal, "WorldRestored", "RestoreWorld",
		fmt.Sprintf("world %s restored to generation %d of %s",
			req.GetKey(), restored.RestoredFrom, restored.RestoredTaken.UTC().Format(time.RFC3339)))
	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldResult{
			Generation: restored.Generation, RestoredFrom: restored.RestoredFrom,
		}},
	}
}

// recordOnGroup reports nothing: a lost event must not fail a restore that
// was carried out.
func (s *Server) recordOnGroup(ctx context.Context, namespace, group, eventType, reason, action, note string) {
	if s.opts.Recorder == nil {
		return
	}
	var g spawneryv1alpha1.ServerGroup
	if err := s.opts.State.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		return
	}
	s.opts.Recorder.Eventf(&g, nil, eventType, reason, action, "%s", note)
}
```

`internal/agentserver/requests.go`, in `answerCloudRequest`, before `default:`:

```go
	case req.GetListRestorePoints() != nil:
		return s.answerListRestorePoints(ctx, logger, id, req.GetId(), req.GetListRestorePoints())
	case req.GetRestoreWorld() != nil:
		return s.answerRestoreWorld(ctx, logger, id, req.GetId(), req.GetRestoreWorld())
```

`cmd/spawnery-operator/main.go`: add `	var history agentserver.WorldHistory` beside `var worlds agentserver.WorldDeleter`; replace `		worlds = worldsync.BucketWorlds{Store: st, Base: base}` with

```go
		bucket := worldsync.BucketWorlds{Store: st, Base: base}
		worlds, history = bucket, bucket
```

and in the `KubeWriter` literal add `History: history` after `Worlds: worlds`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -p 1 -count=1 ./internal/agentserver/ ./cmd/spawnery-operator/ ./internal/docsgen/metrics/`
Expected: PASS.
Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery manifests`
Then: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery metrics-docs-test`
Expected: exit 0; `docs/reference/metrics-and-alerts.md` now lists `spawnery_world_restores_total`.

- [ ] **Step 6: Commit**

```bash
git -C /home/paul/git/spawnery add proto/ internal/agentpb/ agent/common/src/proto/java/ internal/agentserver/ cmd/spawnery-operator/main.go docs/reference/metrics-and-alerts.md
git -C /home/paul/git/spawnery commit -m "feat(agentserver): list restore points and restore a member's world" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: The plugin API

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/RestorePoint.java`, `agent/api/src/main/java/cloud/spawnery/agent/api/RestoredWorld.java`
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java` (after `deleteServer`, line ~321)
- Modify: `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java` (after `deleteServer`)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudConnector.kt` (after `deleteServer` ~:166; `answer` ~:269; imports)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/MirrorApi.kt` (after `deleteServer` ~:90; imports)
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudConnectorTest.kt`, `agent/api/src/test/java/cloud/spawnery/agent/api/ValueTypesTest.java`

**Interfaces:**
- Consumes: the generated `cloud.spawnery.agent.pb.ListRestorePointsRequest`, `RestoreWorldRequest`, `ListRestorePointsResult`, `RestoreWorldResult`, `RestorePoint` (Task 9).
- Produces:
  ```java
  public record RestorePoint(long generation, Instant taken, boolean current) {} // taken non-null
  public record RestoredWorld(long generation, long restoredFrom) {}
  CompletionStage<List<RestorePoint>> listRestorePoints(String group, String key);
  CompletionStage<RestoredWorld> restoreWorld(String group, String key, long generation);
  ```
  The spec writes `CompletableFuture`; every call of `SpawneryApi` returns `CompletionStage`, and so do these.

The spec names `RecordCompatibilityTest` for the records; that test pins old constructors of grown records, and these records have none, so their tests go into `ValueTypesTest`.

- [ ] **Step 1: Write the failing tests**

Add to `CloudConnectorTest.kt`'s imports:

```kotlin
import cloud.spawnery.agent.api.RestorePoint
import cloud.spawnery.agent.api.RestoredWorld
import cloud.spawnery.agent.pb.ListRestorePointsResult
import cloud.spawnery.agent.pb.RestorePoint as PbRestorePoint
import cloud.spawnery.agent.pb.RestoreWorldResult
import java.time.Instant
```

and to the class:

```kotlin
    @Test
    fun `restore points are asked for by group and key and read back newest first`() {
        val connector = connector()
        val stage = connector.listRestorePoints("private-servers", "c0ffee")

        val sent = requested.single().listRestorePoints
        assertEquals("private-servers", sent.group)
        assertEquals("c0ffee", sent.key)
        answer(connector) {
            setListRestorePoints(
                ListRestorePointsResult.newBuilder()
                    .addPoints(PbRestorePoint.newBuilder().setGeneration(9).setTakenUnixMillis(1_791_460_800_000).setCurrent(true))
                    .addPoints(PbRestorePoint.newBuilder().setGeneration(7).setTakenUnixMillis(1_791_457_200_000)),
            )
        }

        assertEquals(
            listOf(
                RestorePoint(9, Instant.ofEpochMilli(1_791_460_800_000), true),
                RestorePoint(7, Instant.ofEpochMilli(1_791_457_200_000), false),
            ),
            stage.toCompletableFuture().get(1, TimeUnit.SECONDS),
        )
    }

    @Test
    fun `a restore sends the generation and reads the new one back`() {
        val connector = connector()
        val stage = connector.restoreWorld("private-servers", "c0ffee", 7)

        val sent = requested.single().restoreWorld
        assertEquals("private-servers", sent.group)
        assertEquals("c0ffee", sent.key)
        assertEquals(7L, sent.generation)
        answer(connector) { setRestoreWorld(RestoreWorldResult.newBuilder().setGeneration(10).setRestoredFrom(7)) }

        assertEquals(RestoredWorld(10, 7), stage.toCompletableFuture().get(1, TimeUnit.SECONDS))
    }

    @Test
    fun `a restore while the world is written fails with UNAVAILABLE and the operator's words`() {
        val connector = connector()
        val stage = connector.restoreWorld("private-servers", "c0ffee", 7)
        answer(connector) {
            setError(
                RequestError.newBuilder()
                    .setReason(RequestError.Reason.UNAVAILABLE)
                    .setMessage("that world is being written"),
            )
        }

        val failure = assertFailsWith<ExecutionException> { stage.toCompletableFuture().get(1, TimeUnit.SECONDS) }
        assertTrue(failure.cause is IllegalStateException, "${failure.cause}")
        assertEquals("UNAVAILABLE: that world is being written", failure.cause!!.message)
    }
```

Add to `ValueTypesTest.java`:

```java
    @Test
    void aRestorePointWithoutATimeIsRefusedWhereItIsBuilt() {
        assertThrows(NullPointerException.class, () -> new RestorePoint(3, null, false));
    }

    @Test
    void twoDescriptionsOfTheSameGenerationAreEqual() {
        var taken = java.time.Instant.ofEpochMilli(1_791_460_800_000L);
        assertEquals(new RestorePoint(3, taken, false), new RestorePoint(3, taken, false));
        assertEquals(new RestoredWorld(10, 3), new RestoredWorld(10, 3));
    }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `git -C /home/paul/git/spawnery add -A agent && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery agent`
Expected: FAIL: the build stops in `:common:compileTestKotlin` / `:api:compileTestJava` with `Unresolved reference: listRestorePoints` and `cannot find symbol: class RestorePoint`.

- [ ] **Step 3: Implement**

`agent/api/src/main/java/cloud/spawnery/agent/api/RestorePoint.java` (Apache header as in `StartedServer.java`):

```java
package cloud.spawnery.agent.api;

import java.time.Instant;
import java.util.Objects;

/**
 * One generation of a private server's world that
 * {@link SpawneryApi#restoreWorld} can go back to.
 *
 * @param generation the generation's number; a higher one is newer
 * @param taken when the node took the snapshot, not when it reached the
 *     object store
 * @param current whether this is the world as it stands, which a restore
 *     cannot pick
 */
public record RestorePoint(long generation, Instant taken, boolean current) {
    public RestorePoint {
        Objects.requireNonNull(taken, "taken");
    }
}
```

`agent/api/src/main/java/cloud/spawnery/agent/api/RestoredWorld.java`:

```java
package cloud.spawnery.agent.api;

/**
 * What a {@link SpawneryApi#restoreWorld} did.
 *
 * @param generation the world's new current generation, which holds the
 *     restored content; the generation that was current before stays a
 *     restore point
 * @param restoredFrom the generation whose content it holds
 */
public record RestoredWorld(long generation, long restoredFrom) {}
```

`SpawneryApi.java`, after `CompletionStage<Void> deleteServer(String group, String key);`:

```java

    /**
     * Lists the generations of a private server's world that
     * {@link #restoreWorld} can go back to, newest first. The first is the
     * world as it stands and says {@link RestorePoint#current()}.
     *
     * <p>Only for a group whose worlds live in the object store
     * ({@code storage.backend: ObjectStore}). Which older generations stay is
     * the group's {@code storage.retention}; without one the list holds the
     * current generation alone. A member that runs adds a generation about
     * every five minutes, and one more when it stops.
     *
     * <p>It fails with {@code NOT_FOUND} for a group this network does not
     * have or a key without a world, with {@code REFUSED} for a group that is
     * not on-demand or keeps its worlds on claims, and with
     * {@code UNAVAILABLE} when the operator runs without world sync. The
     * failure has the shape {@link #startServer} describes.
     */
    CompletionStage<List<RestorePoint>> listRestorePoints(String group, String key);

    /**
     * Makes an older generation of a private server's world its current
     * world, as a new generation. The generation that was current stays a
     * restore point, so a restore can itself be undone until the group's
     * retention drops it. The next {@link #startServer} of the key starts on
     * the restored world.
     *
     * <p>Who may restore, and what a restore means for the players, is your
     * plugin's decision; the operator asks nobody.
     *
     * <p>Only while the member is stopped. It fails with:
     * <ul>
     *   <li>{@code NOT_FOUND} for a group this network does not have, a key
     *       without a world, or a generation the world does not keep.</li>
     *   <li>{@code REFUSED} for a group that is not on-demand or keeps its
     *       worlds on claims, a member that is running, a world that is being
     *       deleted, and the current generation.</li>
     *   <li>{@code UNAVAILABLE} when the operator runs without world sync,
     *       while the member is still stopping, and while its world is being
     *       written. Right after a stop that is the final upload of the
     *       world: ask again a few seconds later.</li>
     * </ul>
     * The failure has the shape {@link #startServer} describes, the timeout
     * and the renewed stream included. Asking again after either is safe: a
     * restore that was carried out made a new generation, and asking again
     * for the same old one only makes another.
     */
    CompletionStage<RestoredWorld> restoreWorld(String group, String key, long generation);
```

`FakeApi.java`, after `deleteServer`:

```java
    @Override
    public CompletionStage<List<RestorePoint>> listRestorePoints(String group, String key) {
        return CompletableFuture.failedFuture(new UnsupportedOperationException("fake"));
    }

    @Override
    public CompletionStage<RestoredWorld> restoreWorld(String group, String key, long generation) {
        return CompletableFuture.failedFuture(new UnsupportedOperationException("fake"));
    }
```

`CloudConnector.kt`: add the imports `cloud.spawnery.agent.api.RestorePoint`, `cloud.spawnery.agent.api.RestoredWorld`, `cloud.spawnery.agent.pb.ListRestorePointsRequest`, `cloud.spawnery.agent.pb.RestoreWorldRequest`; after `deleteServer`:

```kotlin
    fun listRestorePoints(group: String, key: String): CompletionStage<List<RestorePoint>> =
        requests.start<List<RestorePoint>> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setListRestorePoints(ListRestorePointsRequest.newBuilder().setGroup(group).setKey(key))
                    .build(),
            )
        }

    fun restoreWorld(group: String, key: String, generation: Long): CompletionStage<RestoredWorld> =
        requests.start<RestoredWorld> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setRestoreWorld(
                        RestoreWorldRequest.newBuilder().setGroup(group).setKey(key).setGeneration(generation),
                    )
                    .build(),
            )
        }
```

and in `answer`, after the `response.hasDeleteServer()` branch:

```kotlin
            response.hasListRestorePoints() -> requests.complete(
                response.id,
                response.listRestorePoints.pointsList.map {
                    RestorePoint(it.generation, Instant.ofEpochMilli(it.takenUnixMillis), it.current)
                },
            )
            response.hasRestoreWorld() -> requests.complete(
                response.id,
                RestoredWorld(response.restoreWorld.generation, response.restoreWorld.restoredFrom),
            )
```

`MirrorApi.kt`: add the imports `cloud.spawnery.agent.api.RestorePoint` and `cloud.spawnery.agent.api.RestoredWorld`; after `deleteServer`:

```kotlin
    override fun listRestorePoints(group: String, key: String): CompletionStage<List<RestorePoint>> =
        connector.listRestorePoints(group, key)

    override fun restoreWorld(group: String, key: String, generation: Long): CompletionStage<RestoredWorld> =
        connector.restoreWorld(group, key, generation)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `git -C /home/paul/git/spawnery add -A agent && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery agent`
Expected: exit 0: both agents build, JUnit green, `PackagingInvariantTest` included (the new signatures use only `java.*` and this package).

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add agent/
git -C /home/paul/git/spawnery commit -m "feat(agent): listRestorePoints and restoreWorld in the plugin API" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: e2e on kind with MinIO

**Files:**
- Create: `test/e2e/worldhistory_test.go`
- Modify: `test/e2e/manifests/worldsync.yaml` (the `private-servers` group's `storage`, line ~105)
- Modify: `hack/e2e-worldsync.sh` (`-timeout 40m` on line ~158 → `60m`)

**Interfaces:**
- Consumes: the proto messages (Task 9); e2e helpers `applyManifest`, `aProxyPodOf`, `startServer`, `stopServer`, `deleteServer`, `waitReady`, `eventually`, `kubectlExec`, `openProxySession`, `(*proxySession).ask`, `k8s`, `ctx`, and the constants `worldSyncGate`, `worldSyncManifest`, `worldSyncNamespace`, `worldSyncGroup`, `worldSyncProxyGroup` (test/e2e).
- Produces: `TestAnObjectStoreWorldGoesBackToAnEarlierGeneration`, which `hack/e2e-worldsync.sh`'s `-run TestAnObjectStoreWorld` already selects.

The test runs a member twice with a file of its own written into the world each run, restores the generation the second run started from, checks that the second run's file is gone, restores the generation that restore replaced, and finds both files again. Two runs instead of a snapshot request keep the test free of the agent's own snapshot timer: every generation of the second run before its file was written holds only the first file.

- [ ] **Step 1: Write the test**

In `test/e2e/manifests/worldsync.yaml`, under the `private-servers` group's `storage:`, after the `keep:` list:

```yaml
    retention:
      last: 50
```

In `hack/e2e-worldsync.sh`, change `-timeout 40m` to `-timeout 60m` (four more starts of a real server).

`test/e2e/worldhistory_test.go` (build tag and Apache header as in `worldsync_test.go`):

```go
//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/instance"
)

const worldHistoryKey = "5ca1ab1e"

func TestAnObjectStoreWorldGoesBackToAnEarlierGeneration(t *testing.T) {
	if os.Getenv(worldSyncGate) != "1" {
		t.Skipf("set %s=1 to run the object-store path; hack/e2e-worldsync.sh does", worldSyncGate)
	}
	applyManifest(t, worldSyncManifest)
	server, err := instance.Name(worldSyncGroup, worldHistoryKey)
	if err != nil {
		t.Fatalf("compose the member name: %v", err)
	}
	proxy := aProxyPodOf(t, worldSyncNamespace, worldSyncProxyGroup)

	runMember(t, proxy, server, "the first run")
	writeWorldFile(t, server, "spawnery-e2e-first", "first")
	stopAndWaitGone(t, proxy, server)

	runMember(t, proxy, server, "the second run")
	if got := readWorldFile(t, server, "spawnery-e2e-first"); got != "first" {
		t.Fatalf("the second run found %q in the first run's file, want \"first\"", got)
	}
	found := restorePoints(t, proxy)[0]
	if !found.GetCurrent() {
		t.Fatalf("the first restore point %v is not the current one", found)
	}
	writeWorldFile(t, server, "spawnery-e2e-second", "second")
	stopAndWaitGone(t, proxy, server)

	back := restoreWhenFree(t, proxy, found.GetGeneration())
	if back.GetRestoredFrom() != found.GetGeneration() {
		t.Fatalf("restored from %d, asked for %d", back.GetRestoredFrom(), found.GetGeneration())
	}
	points := restorePoints(t, proxy)
	if points[0].GetGeneration() != back.GetGeneration() || !points[0].GetCurrent() {
		t.Fatalf("restore points %v do not start with the restore's generation %d", points, back.GetGeneration())
	}
	if !hasGeneration(points, back.GetGeneration()-1) || !hasGeneration(points, found.GetGeneration()) {
		t.Fatalf("restore points %v lack the generation the restore replaced or the one it restored", points)
	}

	runMember(t, proxy, server, "the run after the restore")
	if got := readWorldFile(t, server, "spawnery-e2e-first"); got != "first" {
		t.Fatalf("after the restore the first run's file holds %q, want \"first\"", got)
	}
	if worldFileExists(t, server, "spawnery-e2e-second") {
		t.Fatalf("after restoring generation %d the second run's file is still there: the start did not get the restored world", found.GetGeneration())
	}
	stopAndWaitGone(t, proxy, server)

	forward := restoreWhenFree(t, proxy, back.GetGeneration()-1)
	t.Logf("restored generation %d as %d", forward.GetRestoredFrom(), forward.GetGeneration())
	runMember(t, proxy, server, "the run after the second restore")
	if got := readWorldFile(t, server, "spawnery-e2e-second"); got != "second" {
		t.Fatalf("after undoing the restore the second run's file holds %q, want \"second\"", got)
	}
	if got := readWorldFile(t, server, "spawnery-e2e-first"); got != "first" {
		t.Fatalf("after undoing the restore the first run's file holds %q, want \"first\"", got)
	}

	deleteServer(t, proxy, worldSyncGroup, worldHistoryKey)
}

func runMember(t *testing.T, proxy *corev1.Pod, server, which string) {
	t.Helper()
	started := startServer(t, proxy, worldSyncGroup, worldHistoryKey)
	if started.GetServer() != server || started.GetAlreadyRunning() {
		t.Fatalf("%s: the start answered %v, want a new %s", which, started, server)
	}
	waitReady(t, worldSyncNamespace, server, which)
}

func stopAndWaitGone(t *testing.T, proxy *corev1.Pod, server string) {
	t.Helper()
	stopServer(t, proxy, server)
	eventually(t, 5*time.Minute, "the stopped server to be gone", func() (bool, string) {
		var srv spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKey{Namespace: worldSyncNamespace, Name: server}, &srv)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase " + srv.Status.Phase
	})
	eventually(t, 2*time.Minute, "the stopped server's pod to be gone", func() (bool, string) {
		var pod corev1.Pod
		err := k8s.Get(ctx, client.ObjectKey{Namespace: worldSyncNamespace, Name: server}, &pod)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase " + string(pod.Status.Phase)
	})
}

func askOnce(t *testing.T, proxy *corev1.Pod, req *agentpb.CloudRequest) *agentpb.CloudResponse {
	t.Helper()
	s := openProxySession(t, proxy)
	defer s.close()
	return s.ask(t, req)
}

func restorePoints(t *testing.T, proxy *corev1.Pod) []*agentpb.RestorePoint {
	t.Helper()
	resp := askOnce(t, proxy, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_ListRestorePoints{
		ListRestorePoints: &agentpb.ListRestorePointsRequest{Group: worldSyncGroup, Key: worldHistoryKey},
	}})
	points := resp.GetListRestorePoints().GetPoints()
	if len(points) == 0 {
		t.Fatalf("listing the restore points was answered %v", resp.GetError())
	}
	return points
}

// restoreWhenFree asks again while the answer is UNAVAILABLE: right after a
// stop the node still uploads the world under its lease.
func restoreWhenFree(t *testing.T, proxy *corev1.Pod, generation int64) *agentpb.RestoreWorldResult {
	t.Helper()
	var result *agentpb.RestoreWorldResult
	eventually(t, 3*time.Minute, fmt.Sprintf("a restore of generation %d", generation), func() (bool, string) {
		resp := askOnce(t, proxy, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_RestoreWorld{
			RestoreWorld: &agentpb.RestoreWorldRequest{Group: worldSyncGroup, Key: worldHistoryKey, Generation: generation},
		}})
		if r := resp.GetRestoreWorld(); r != nil {
			result = r
			return true, ""
		}
		if resp.GetError().GetReason() == agentpb.RequestError_UNAVAILABLE {
			return false, resp.GetError().GetMessage()
		}
		t.Fatalf("restoring generation %d was answered %v", generation, resp.GetError())
		return false, ""
	})
	return result
}

func hasGeneration(points []*agentpb.RestorePoint, generation int64) bool {
	for _, p := range points {
		if p.GetGeneration() == generation {
			return true
		}
	}
	return false
}

func writeWorldFile(t *testing.T, server, name, content string) {
	t.Helper()
	out, err := kubectlExec(t, worldSyncNamespace, server, fmt.Sprintf("set -e; test -d /data/world; printf %%s %q > /data/world/%s", content, name))
	if err != nil {
		t.Fatalf("write %s into %s's world: %v\n%s", name, server, err, out)
	}
}

func readWorldFile(t *testing.T, server, name string) string {
	t.Helper()
	out, err := kubectlExec(t, worldSyncNamespace, server, "cat /data/world/"+name)
	if err != nil {
		t.Fatalf("read %s out of %s's world: %v\n%s", name, server, err, out)
	}
	return strings.TrimSpace(out)
}

func worldFileExists(t *testing.T, server, name string) bool {
	t.Helper()
	out, err := kubectlExec(t, worldSyncNamespace, server, "if test -e /data/world/"+name+"; then echo yes; else echo no; fi")
	if err != nil {
		t.Fatalf("look for %s in %s's world: %v\n%s", name, server, err, out)
	}
	return strings.TrimSpace(out) == "yes"
}
```

- [ ] **Step 2: Check that it compiles** (any machine)

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery vet -tags e2e ./test/...`
Expected: exit 0.

- [ ] **Step 3: Commit**

```bash
git -C /home/paul/git/spawnery add test/e2e/ hack/e2e-worldsync.sh
git -C /home/paul/git/spawnery commit -m "test(e2e): a world goes back to an earlier generation and forward again" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 4: Run it on `paul-desktop`**

Only on `paul-desktop` (`hostname`); on the dev VM hand this step to Paul with the command below and the expected lines.

Run: `systemd-run --scope --user --property=Delegate=yes -- nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c env KIND_EXPERIMENTAL_PROVIDER=podman make -C /home/paul/git/spawnery e2e-worldsync`
Expected: `--- PASS: TestAnObjectStoreWorldTravelsBetweenNodes` and `--- PASS: TestAnObjectStoreWorldGoesBackToAnEarlierGeneration`, the second logging `restored generation N as M`. Paste the tail of the output into the task report. A failure is fixed in the task that owns the code and Step 4 runs again.

---

### Task 12: Docs, dashboards, known issues

**Files:**
- Modify: `docs/guides/object-store-worlds.md` (new section after "## Deleting a world"; "## Watching it"; "## Limits")
- Modify: `docs/plugin-api/what-a-plugin-can-do.md` (after the `deleteServer` paragraph, line ~102-106)
- Modify: `docs/guides/on-demand-servers.md` (the reasons table, lines ~74-84)
- Modify: `docs/reference/known-issues.md` (delete the section "## Upload attempts that fail leave objects in the bucket", line 84 to the end of the file)
- Modify: `charts/spawnery/dashboards/worldsync.json`, `charts/spawnery/dashboards/network.json`
- Test: `internal/docsgen/metrics` (`TestTheDashboardsQueryOnlyRegisteredMetrics`), `hack/docs-length.sh`, `make docs`

**Interfaces:**
- Consumes: metric names from Tasks 5 and 9; the API names from Task 10; anchor `#older-generations` of the new guide section.
- Produces: nothing code depends on.

All prose below goes through the `humanizer` skill (embedded mode) before it is written into the files; the facts, names and numbers stay.

- [ ] **Step 1: The guide**

In `docs/guides/object-store-worlds.md`, insert before `## Moving existing worlds in`:

````markdown
## Older generations

Each upload of a member's world makes a new generation: about every five
minutes while the member runs, and once more when it stops. By default the
bucket keeps only the newest. `storage.retention` keeps older ones, with the
prune options of Proxmox Backup Server:

```yaml
spec:
  storage:
    backend: ObjectStore
    keep: [world]
    retention:
      last: 12    # the newest 12, the current one included
      hourly: 24  # then the newest of each of 24 more hours
      daily: 7
      weekly: 4
      monthly: 3
```

The options apply in the order `last`, `hourly`, `daily`, `weekly`,
`monthly`, `yearly`. Each counts only periods that hold a generation, skips a
period an earlier option already kept a generation from, and keeps the
newest generation of each further period. Weeks are ISO weeks and every
period is UTC. A member nobody plays makes no generations, so its history
stays as it is.

A kept generation costs what its upload cost: the region files that changed
and one pack of the small files. On a pregenerated world of about 2.75 GB,
two players changed 8 region files, 41.7 MB, within an hour; a member nobody
played changed one file of 0.7 MB.

The operator writes the policy to `.retention/<namespace>/<group>.json` in
the bucket, and the node agent reads it before each upload, so a change
reaches each world at its next upload and restarts no member. Removing
`retention` drops a world's history at its next upload. The API refuses
`retention` without `backend: ObjectStore`, so a group goes back to `Claim`
only after dropping it.

A plugin lists a member's generations with `listRestorePoints(group, key)`
and makes one of them current with `restoreWorld(group, key, generation)`
while the member is stopped; see
[what a plugin can do](../plugin-api/what-a-plugin-can-do.md). The restore
writes the old content as a new generation, so the generation that was
current stays a restore point until the retention drops it. The next start
downloads the whole world, also on a node that holds most of it. Each
restore records the event `WorldRestored` on the group and counts in
`spawnery_world_restores_total`.
````

In "## Watching it", change `download and upload times and counts, retries, lease` to `download and upload times and counts, objects pruned, retries, lease`.

In "## Limits", replace the bullet

```markdown
- Objects and packs written by an upload attempt that failed stay in the
  bucket until the world is deleted.
```

with

```markdown
- What an upload attempt that failed wrote stays in the bucket until the
  world's next sweep: every twelfth upload, and when a node releases the
  world.
- A policy change does not prune worlds nobody plays.
- The history holds what the snapshots held. Play after the last snapshot
  before a crash is in no generation, and a deleted world cannot be
  restored.
```

and the closing sentence's file list becomes `` `docs/superpowers/specs/2026-10-06-object-store-worlds-design.md` and `docs/superpowers/specs/2026-10-08-world-history-design.md` ``.

- [ ] **Step 2: The plugin pages**

In `docs/plugin-api/what-a-plugin-can-do.md`, after the paragraph that starts `` `deleteServer(group, key)` **deletes a private server for good** ``:

```markdown
`listRestorePoints(group, key)` and `restoreWorld(group, key, generation)`
**take a private server's world back to an older generation**, for a group
whose worlds live in the object store and keep a history
([worlds in an object store](../guides/object-store-worlds.md#older-generations)).
The list is newest first, and its first entry is the world as it stands. A
restore needs the member stopped and writes the old content as a new
generation, so it can itself be undone. Who may restore is your plugin's
decision; the operator asks nobody. Right after a stop the world's final
upload may still run, and the restore answers `UNAVAILABLE` until it is
done: ask again a few seconds later.
```

and append to the paragraph that starts `Each fails with a reason.` the sentence: `` `listRestorePoints` and `restoreWorld` answer `NOT_FOUND`, `REFUSED` and `UNAVAILABLE` as the table there lists. ``

In `docs/guides/on-demand-servers.md`, append to the reasons table, after the row `` | `startServer` | `UNAVAILABLE` | also: the key's world is still being deleted. | ``:

```markdown
| `listRestorePoints`, `restoreWorld` | `NOT_FOUND` | no such group, or the key has no world; for `restoreWorld` also a generation the world does not keep. |
| `listRestorePoints`, `restoreWorld` | `REFUSED` | the group is not `OnDemand`, or keeps its worlds on claims. |
| `restoreWorld` | `REFUSED` | also: the member is running, its world is being deleted, or the generation is the current one. |
| `restoreWorld` | `UNAVAILABLE` | the member is still stopping, or its final upload or another restore still writes the world. Ask again. |
| `listRestorePoints`, `restoreWorld` | `UNAVAILABLE` | the operator runs without world sync. |
```

- [ ] **Step 3: Known issues**

In `docs/reference/known-issues.md`, delete the section `## Upload attempts that fail leave objects in the bucket` with its paragraph, from line 84 to the end of the file; the sweep closes it.

- [ ] **Step 4: The dashboards**

The dashboards round-trip byte for byte through Python's `json` with `indent=2`, so edit them with it:

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c python3 - <<'EOF'
import json

ds = {"type": "prometheus", "uid": "${datasource}"}

p = "/home/paul/git/spawnery/charts/spawnery/dashboards/worldsync.json"
d = json.load(open(p))
for panel in d["panels"]:
    if panel["gridPos"]["y"] >= 22:
        panel["gridPos"]["y"] += 8
pruned = {
    "type": "timeseries",
    "title": "Pruned objects",
    "id": 21,
    "datasource": ds,
    "gridPos": {"h": 8, "w": 24, "x": 0, "y": 22},
    "targets": [{
        "datasource": ds,
        "expr": "sum by (node) (increase(spawnery_worldsync_pruned_objects_total{namespace=\"$namespace\",node=~\"$node\"}[$__interval]))",
        "refId": "A",
        "legendFormat": "{{node}}",
    }],
    "fieldConfig": {"defaults": {"unit": "none", "custom": {"drawStyle": "bars", "fillOpacity": 60, "showPoints": "never", "spanNulls": False}}, "overrides": []},
    "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi", "sort": "desc"}},
    "interval": "5m",
    "description": "Objects and packs that prunes deleted: generations the retention dropped, and what failed uploads left behind.",
}
at = next(i for i, x in enumerate(d["panels"]) if x["id"] == 14) + 1
d["panels"].insert(at, pruned)
open(p, "w").write(json.dumps(d, indent=2) + "\n")

p = "/home/paul/git/spawnery/charts/spawnery/dashboards/network.json"
d = json.load(open(p))
d["panels"].append({
    "type": "timeseries",
    "title": "World restores",
    "id": 19,
    "datasource": ds,
    "gridPos": {"h": 8, "w": 24, "x": 0, "y": 60},
    "targets": [{
        "datasource": ds,
        "expr": "sum by (result) (increase(spawnery_world_restores_total[$__interval]))",
        "refId": "A",
        "legendFormat": "{{result}}",
    }],
    "fieldConfig": {"defaults": {"unit": "none", "custom": {"drawStyle": "bars", "fillOpacity": 60, "stacking": {"mode": "normal"}}}, "overrides": []},
    "options": {"legend": {"displayMode": "table", "placement": "right", "calcs": ["sum"]}},
    "interval": "5m",
    "description": "Restores plugins asked for, by result. The operator's metric carries no network label.",
})
open(p, "w").write(json.dumps(d, indent=2) + "\n")
EOF
```

Check: `git -C /home/paul/git/spawnery diff --stat charts/spawnery/dashboards/` shows only those two files, with the worldsync diff touching the new panel and the `"y"` values of the Health row and its panels.

- [ ] **Step 5: Verify**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go -C /home/paul/git/spawnery test -count=1 ./internal/docsgen/metrics/`
Expected: PASS (`TestTheDashboardsQueryOnlyRegisteredMetrics` finds both new metrics registered).
Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery docs-length-lint chart-worldsync-test docs`
Expected: exit 0 (`make docs` is `mkdocs --strict`, the only link checker, so the new anchor `#older-generations` is checked here).

- [ ] **Step 6: Commit**

```bash
git -C /home/paul/git/spawnery add docs/guides/ docs/plugin-api/ docs/reference/known-issues.md charts/spawnery/dashboards/
git -C /home/paul/git/spawnery commit -m "docs(worldsync): world history and restores, dashboard panels, a known issue closed" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 13: Version 0.25.0, full verification, PR

**Files:**
- Modify: `flake.nix` (`imageVersion = "0.24.3"` → `"0.25.0"`, line 127; `operatorVersion = "0.24.8"` → `"0.25.0"`, line 134)
- Modify: every pin of the chart and operator at `0.24.8`: `README.md`, `charts/spawnery/Chart.yaml` (`version`, `appVersion`), `charts/spawnery/README.md`, `charts/spawnery/values.yaml` (`image.tag`), `docs/getting-started/index.md`, `docs/tutorial/index.md`, `docs/guides/object-store-worlds.md` (helm `--version` and `spawnery-worldsync:` tag)
- Modify: every game image and API pin at `0.24.3`: `agent/api/README.md`, `config/samples/network.yaml`, `config/samples/ondemand.yaml`, `docs/guides/{expose-strategies,object-store-worlds,on-demand-servers,persistent-worlds,scaling-and-boosts,scheduling}.md`, `docs/plugin-api/index.md`, `docs/tutorial/index.md`, `docs/tutorial/network.yaml`, `test/e2e/manifests/ondemand.yaml`, `test/e2e/manifests/worldsync.yaml`
- Generated: `docs/reference/chart-values.md` (`make manifests`)
- Modify: `docs/superpowers/specs/2026-10-08-world-history-design.md` (status line)

The release commit `8e2ddc9` (0.24.8) moved only `operatorVersion` and the chart pins; `4e76500` (0.24.3) moved `imageVersion` and the game image pins. This release changes `agent/` (new API), so both move. The phrase "game images 0.24.3 or later" in `servergroup_types.go`, the CRDs and the guide is a minimum, not a pin, and stays.

- [ ] **Step 1: Bump**

```bash
sed -i 's/imageVersion = "0.24.3";/imageVersion = "0.25.0";/; s/operatorVersion = "0.24.8";/operatorVersion = "0.25.0";/' /home/paul/git/spawnery/flake.nix
grep -rlF '0.24.8' /home/paul/git/spawnery --include='*.md' --include='*.yaml' --exclude-dir=superpowers --exclude-dir=.git --exclude-dir=build | xargs sed -i 's/0\.24\.8/0.25.0/g'
grep -rlE '(purpur:26\.3|velocity:4\.2\.0)-0\.24\.3|spawnery-api:0\.24\.3' /home/paul/git/spawnery --exclude-dir=superpowers --exclude-dir=.git --exclude-dir=build | xargs sed -i -E 's/((purpur:26\.3|velocity:4\.2\.0)-|spawnery-api:)0\.24\.3/\10.25.0/g'
```

Set the spec's status line to `**Status:** implemented (branch feat/world-history)`, and correct the spec wherever the implementation decided differently (the PBS wording in §3.1, `CompletionStage`, the reason for a stopping member, the restore counter's `NOT_FOUND`, the tests named in §7).

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery manifests`
Then: `grep -rnE '0\.24\.8|-0\.24\.3|api:0\.24\.3' /home/paul/git/spawnery --exclude-dir=superpowers --exclude-dir=.git --exclude-dir=build --exclude=deps.json`
Expected: no output.
Then: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c bash /home/paul/git/spawnery/hack/image-tag-pins-agree.sh`
Expected: exit 0.

- [ ] **Step 2: Full verification**

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c env GOFLAGS=-p=1 make -C /home/paul/git/spawnery test
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery lint
git -C /home/paul/git/spawnery add -A agent && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make -C /home/paul/git/spawnery agent
nix --extra-experimental-features 'nix-command flakes' build /home/paul/git/spawnery#operator-image /home/paul/git/spawnery#worldsync-image /home/paul/git/spawnery#purpur-image --no-link
```

Expected: every command exits 0 (`GOFLAGS=-p=1` only on the dev VM). Paste the tail of each into the task report. Read the output, not only the exit code.

- [ ] **Step 3: Commit**

```bash
git -C /home/paul/git/spawnery add -A flake.nix README.md charts/ docs/ config/ agent/api/README.md test/e2e/manifests/
git -C /home/paul/git/spawnery commit -m "chore: 0.25.0, world history for object store worlds" -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 4: Push and open the PR**

```bash
git -C /home/paul/git/spawnery push -u origin feat/world-history
```

Open the PR against `master` with `gh pr create --repo spawnery/spawnery --base master --head feat/world-history --title "feat: world history for object store worlds" --body-file <file>`; write the body (what it does, the e2e result from Task 11, the decisions listed in Step 1's spec corrections) through the `humanizer` skill, ending with `🤖 Generated with [Claude Code](https://claude.com/claude-code)`. Merging, the `v0.25.0` tag and publishing wait for Paul's word.

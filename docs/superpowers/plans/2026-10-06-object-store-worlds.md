# On-demand worlds in an object store: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `spec.storage.backend: ObjectStore` gives an `OnDemand` member's `/data` from a node-local directory that `spawnery-worldsync` fills from an S3 bucket and writes back, so a start skips the volume attach and a world can run on any node.

**Architecture:** A new package `internal/worldsync` holds everything that talks to the bucket (store, manifest, snapshot, upload, download, lease, deletion) behind a small `Store` interface with an in-memory fake for tests. `cmd/spawnery-worldsync` wraps it as a CSI node plugin for inline ephemeral volumes plus an `import` command. The operator renders the CSI volume instead of a claim and marks worlds for deletion; the Paper agent waits for the download and asks for snapshots through files in `/data/.spawnery-worldsync/`.

**Tech Stack:** Go 1.26, controller-runtime, aws-sdk-go-v2 (S3), CSI spec v1 Go bindings, gRPC; Kotlin/Paper for the agent; Helm chart; Nix for builds and images; kind + MinIO for e2e.

**Spec:** `docs/superpowers/specs/2026-10-06-object-store-worlds-design.md`

## Global Constraints

- Every command runs in the dev shell: `nix develop -c <command>`. On the dev VM (8 cores, 12 GB) run envtest packages with `-p 1`.
- New Go files carry the Apache header from `hack/boilerplate.go.txt` (`Copyright paul_wtf.`).
- Commits are Conventional Commits with a scope (`feat(worldsync): …`), body wrapped at 72 columns, ending with `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`. Commits are gpg-signed; on the dev VM probe first with `echo probe | gpg --clearsign --pinentry-mode error -u "$(git config user.signingkey)" > /dev/null` and ask Paul to unlock if it fails.
- After changing API types or markers: `nix develop -c make manifests generate` and commit the generated files.
- After any `go.mod` change: `nix build .#spawnery-operator --no-link`, take the `got:` hash, replace all `vendorHash` occurrences in `flake.nix` (there are five today, six after Task 15).
- CSI driver name: `worldsync.spawnery.cloud`. Control directory: `.spawnery-worldsync` at the root of `/data`.
- ETags are handled **without** quotes everywhere: stripped on read, sent bare in `If-Match` (Hetzner refuses the quoted form).
- Lease stale after 10 minutes, renewed every 30 s. Download parallelism 32, upload parallelism 16. Files below 64 KiB go into the generation's pack; files of 64 KiB or more are stored one object each, by SHA-256.
- Pack format: `packs/<generation>.tar.gz` (stdlib `archive/tar` + `compress/gzip`; no new compression dependency).
- Deletion index: `<base>/.deletions/<namespace>/<group>/<key>`; world prefix `<base>/<namespace>/<group>/<key>/`.
- Env names: `SPAWNERY_WORLD_SYNC=1`, `SPAWNERY_WORLD_SYNC_INTERVAL` (Go duration, e.g. `5m0s`). Node agent and import read `WORLDSYNC_ENDPOINT`, `WORLDSYNC_REGION`, `WORLDSYNC_BUCKET`, `WORLDSYNC_PREFIX`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`.
- Release numbers: operator, images and chart move to `0.24.0` (minor: new CRD field, new behaviour). Tagging and publishing happen only on Paul's explicit word.
- No private network names anywhere in this repository (code, tests, docs, commits).

## Review Focus

1. **A pod asking for a world of another namespace.** Anyone who can create a pod may name the CSI driver with any `world` attribute; the node agent must refuse a world whose namespace is not the pod's (`csi.storage.k8s.io/pod.namespace`). Test in Task 9.
2. **The same world published again on the same node before the old pod's unpublish arrived.** The kubelet may tear down the old pod's volume after the new pod's publish. The node agent treats a publish for a world that is still published under another target as the old target's unpublish first. Test in Task 8.
3. **Paper writes a region file while the snapshot copies it.** A chunk unload can write with autosave off. The copy rechecks size and mtime and retries; a file that never settles fails the snapshot instead of uploading a torn file. Test in Task 4.
4. **A cache left from a deleted world under a reused key.** A world deleted and created again must not inherit the old local copy: the `worldId` differs, so the publish wipes the keep paths. Test in Task 8.
5. **The store refuses or times out mid-upload.** A failed object or pack upload must not write a manifest naming objects that are not there, and the next attempt must start cleanly from the same snapshot. Test in Task 5.

---

## File structure

| path | responsibility |
|---|---|
| `internal/prune/keep.go` | exported keep matcher (`ParseKeep`, `Keep.Holds`, `Keep.Toward`); prune skips the control directory |
| `internal/worldsync/store.go` | `Store` interface, `ObjectInfo`, `PutCondition`, errors |
| `internal/worldsync/memstore.go` | in-memory `Store` for tests (conditional writes, outage switch) |
| `internal/worldsync/s3store.go` | `Store` over aws-sdk-go-v2 |
| `internal/worldsync/layout.go` | key layout, constants |
| `internal/worldsync/manifest.go` | `Manifest`, `FileEntry`, read/write |
| `internal/worldsync/snapshot.go` | `Scan`, `TakeSnapshot` (local copy with recheck) |
| `internal/worldsync/transfer.go` | `UploadSnapshot`, `Download` |
| `internal/worldsync/lease.go` | `TakeLease`, `RenewLease`, `ReleaseLease`, `ReadLease` |
| `internal/worldsync/deletion.go` | `MarkDeleted`, `DeletionPending`, `Sweeper` |
| `internal/worldsync/state.go` | per-world `state.json` on the node |
| `internal/worldsync/node.go` | `Node`: publish, unpublish, snapshot requests, uploads, renewals, eviction |
| `internal/worldsync/csi.go` | CSI Identity and Node gRPC services over `Node` |
| `internal/worldsync/mount_linux.go` | real `Mounter` (bind mount) |
| `internal/worldsync/metrics.go` | Prometheus metrics of the node agent |
| `cmd/spawnery-worldsync/main.go` | `node` and `import` subcommands |
| `api/v1alpha1/servergroup_types.go` | `StorageSpec.Backend`, CEL rule, `UsesClaim()` |
| `internal/podspec/server.go`, `worldsync.go` | CSI volume, env, `WithWorldSyncInterval` |
| `internal/controller/server_controller.go`, `setup.go` | no claims for ObjectStore, refusal when world sync is off |
| `internal/agentserver/writer.go` | `DeleteServer` for ObjectStore |
| `cmd/spawnery-operator/main.go` | `--world-sync`, `--world-sync-snapshot-interval`, store, sweeper |
| `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/WorldSync.kt` | wait and snapshot protocol |
| `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt` | `onLoad` wait, snapshot timer |
| `charts/spawnery/templates/worldsync.yaml`, `values.yaml`, `deployment.yaml` | chart |
| `nix/worldsync-image.nix`, `flake.nix`, `hack/publish.sh`, `.github/workflows/release.yml` | build and publish |
| `hack/e2e-worldsync.sh`, `test/e2e/worldsync_test.go`, `test/e2e/manifests/worldsync*.yaml` | e2e |
| `docs/guides/object-store-worlds.md`, `mkdocs.yml` | guide |

---

### Task 1: Keep matcher for reuse, prune spares the control directory

**Files:**
- Create: `internal/prune/keep.go`
- Modify: `internal/prune/prune.go` (function `plan`, the `lost+found` branch near line 158)
- Test: `internal/prune/keep_test.go`, `internal/prune/prune_test.go`

**Interfaces:**
- Produces: `prune.ParseKeep(entries []string) (prune.Keep, error)`, `func (k Keep) Holds(rel string) bool` (rel is at or below a matched path), `func (k Keep) Toward(rel string) bool` (rel is a directory below which an entry could still match), `prune.ControlDir = ".spawnery-worldsync"`.

- [ ] **Step 1: Write the failing tests**

`internal/prune/keep_test.go`:

```go
package prune

import "testing"

func TestKeepHoldsWhatAnEntryMatchesAndBelow(t *testing.T) {
	k, err := ParseKeep([]string{"worlds/world", "plugins/*/data"})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"worlds/world":                 true,
		"worlds/world/level.dat":       true,
		"worlds/world2":                false,
		"worlds":                       false,
		"plugins/Example/data/a.yml":   true,
		"plugins/Example/config.yml":   false,
		"logs/latest.log":              false,
	} {
		if got := k.Holds(rel); got != want {
			t.Errorf("Holds(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestKeepTowardIsTheWayDownToAnEntry(t *testing.T) {
	k, err := ParseKeep([]string{"worlds/world", "plugins/*/data"})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"worlds":          true,
		"plugins":         true,
		"plugins/Example": true,
		"logs":            false,
		"worlds/world":    false, // matched, not on the way: Holds answers for it
	} {
		if got := k.Toward(rel); got != want {
			t.Errorf("Toward(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestParseKeepRefusesWhatPruneRefuses(t *testing.T) {
	if _, err := ParseKeep([]string{"../escape"}); err == nil {
		t.Fatal("a .. segment was accepted")
	}
}
```

In `internal/prune/prune_test.go` add (follow the file's existing helpers for building a directory tree and an empty mountinfo; if there is none, use `t.TempDir()` and write an empty file for mountinfo):

```go
func TestPruneLeavesTheWorldSyncControlDirectory(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ControlDir, "ready"), "")
	mustWrite(t, filepath.Join(dir, "logs", "latest.log"), "x")
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	mustWrite(t, mountinfo, "")

	if err := Run(dir, []string{"worlds/world"}, nil, mountinfo, nil, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ControlDir, "ready")); err != nil {
		t.Fatalf("the control directory was pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Fatalf("logs survived a prune that keeps only worlds/world")
	}
}
```

Add a `mustWrite(t, path, content)` helper (MkdirAll the parent, WriteFile 0o644) to the test file if none exists.

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop -c go test ./internal/prune/ -count=1`
Expected: FAIL, `undefined: ParseKeep` and `undefined: ControlDir`.

- [ ] **Step 3: Implement**

`internal/prune/keep.go`:

```go
package prune

import "strings"

// ControlDir is where spawnery-worldsync and the game server's agent leave
// files for each other. It sits at the root of /data and is never pruned:
// the node agent may have written "ready" before the entrypoint runs.
const ControlDir = ".spawnery-worldsync"

// Keep is a parsed spec.storage.keep list, for callers outside the prune
// that need to know what a group keeps.
type Keep struct{ pats [][]string }

func ParseKeep(entries []string) (Keep, error) {
	pats, err := parseEntries("spec.storage.keep", entries)
	if err != nil {
		return Keep{}, err
	}
	return Keep{pats: pats}, nil
}

// Holds reports whether rel, slash-separated and relative to /data, is or
// lies below a path an entry matches.
func (k Keep) Holds(rel string) bool {
	return kept(k.pats, strings.Split(rel, "/"))
}

// Toward reports whether rel is a directory that an entry could still
// match below, without being matched itself.
func (k Keep) Toward(rel string) bool {
	segs := strings.Split(rel, "/")
	return !kept(k.pats, segs) && onTheWay(k.pats, nil, segs)
}
```

In `internal/prune/prune.go`, function `plan`, next to the existing `lost+found` skip at the root:

```go
		if name == ControlDir && len(rel) == 0 {
			continue
		}
```

(Match the surrounding loop's variable names; the `lost+found` branch shows the form.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop -c go test ./internal/prune/ ./cmd/spawnery-config/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/prune/
git commit -m "feat(prune): export the keep matcher, spare the world sync control dir"
```

---

### Task 2: The `Store` interface and its in-memory fake

**Files:**
- Create: `internal/worldsync/store.go`, `internal/worldsync/memstore.go`, `internal/worldsync/layout.go`
- Test: `internal/worldsync/memstore_test.go`

**Interfaces:**
- Produces:
  ```go
  var ErrNotFound, ErrPrecondition error
  type ObjectInfo struct { ETag string; Size int64; Date time.Time }
  type PutCondition struct { IfNoneMatch bool; IfMatch string }
  type Store interface {
      Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
      Head(ctx context.Context, key string) (ObjectInfo, error)
      Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error)
      Delete(ctx context.Context, key string) error
      List(ctx context.Context, prefix string) ([]string, error)
  }
  func NewMemStore(clock func() time.Time) *MemStore
  func (m *MemStore) SetOutage(err error)   // nil ends it
  func (m *MemStore) Keys() []string
  func WorldPrefix(base, world string) string        // "<base>/<world>/" with base possibly ""
  func DeletionKey(base, world string) string        // "<base>/.deletions/<world>"
  func DeletionPrefix(base string) string            // "<base>/.deletions/"
  const ManifestName = "manifest.json"; const LeaseName = "lease.json"
  const PackBelow = 64 << 10
  ```

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/memstore_test.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func put(t *testing.T, s Store, key, body string, cond PutCondition) (ObjectInfo, error) {
	t.Helper()
	return s.Put(context.Background(), key, bytes.NewReader([]byte(body)), cond)
}

func TestMemStoreCreateOnlyOnce(t *testing.T) {
	s := NewMemStore(time.Now)
	if _, err := put(t, s, "a", "1", PutCondition{IfNoneMatch: true}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := put(t, s, "a", "2", PutCondition{IfNoneMatch: true}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second create: err = %v, want ErrPrecondition", err)
	}
}

func TestMemStoreIfMatchComparesBareETags(t *testing.T) {
	s := NewMemStore(time.Now)
	first, _ := put(t, s, "a", "1", PutCondition{})
	if first.ETag == "" || first.ETag[0] == '"' {
		t.Fatalf("ETag %q is empty or quoted", first.ETag)
	}
	if _, err := put(t, s, "a", "2", PutCondition{IfMatch: "deadbeef"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale If-Match: err = %v, want ErrPrecondition", err)
	}
	if _, err := put(t, s, "a", "2", PutCondition{IfMatch: first.ETag}); err != nil {
		t.Fatalf("current If-Match: %v", err)
	}
	rc, _, err := s.Get(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "2" {
		t.Fatalf("content = %q, want 2", got)
	}
}

func TestMemStoreMissingIsNotFoundAndDeleteIsIdempotent(t *testing.T) {
	s := NewMemStore(time.Now)
	if _, err := s.Head(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(context.Background(), "x"); err != nil {
		t.Fatalf("Delete of nothing: %v", err)
	}
}

func TestMemStoreListsByPrefixInOrder(t *testing.T) {
	s := NewMemStore(time.Now)
	for _, k := range []string{"w/b", "w/a", "x/a"} {
		put(t, s, k, "", PutCondition{})
	}
	got, _ := s.List(context.Background(), "w/")
	if len(got) != 2 || got[0] != "w/a" || got[1] != "w/b" {
		t.Fatalf("List = %v", got)
	}
}

func TestMemStoreOutageFailsEveryCall(t *testing.T) {
	s := NewMemStore(time.Now)
	boom := errors.New("unreachable")
	s.SetOutage(boom)
	if _, err := put(t, s, "a", "1", PutCondition{}); !errors.Is(err, boom) {
		t.Fatalf("Put during outage: %v", err)
	}
	s.SetOutage(nil)
	if _, err := put(t, s, "a", "1", PutCondition{}); err != nil {
		t.Fatalf("Put after outage: %v", err)
	}
}

func TestLayout(t *testing.T) {
	if got := WorldPrefix("", "ns/g/k"); got != "ns/g/k/" {
		t.Errorf("WorldPrefix = %q", got)
	}
	if got := WorldPrefix("worlds", "ns/g/k"); got != "worlds/ns/g/k/" {
		t.Errorf("WorldPrefix = %q", got)
	}
	if got := DeletionKey("", "ns/g/k"); got != ".deletions/ns/g/k" {
		t.Errorf("DeletionKey = %q", got)
	}
	if got := DeletionPrefix("worlds"); got != "worlds/.deletions/" {
		t.Errorf("DeletionPrefix = %q", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `nix develop -c go test ./internal/worldsync/ -count=1`
Expected: FAIL (package does not compile: undefined names).

- [ ] **Step 3: Implement**

`internal/worldsync/store.go`:

```go
// Package worldsync keeps the worlds of an OnDemand group in an S3 bucket
// with a copy on the node that runs them; see
// docs/superpowers/specs/2026-10-06-object-store-worlds-design.md.
package worldsync

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound     = errors.New("worldsync: no such object")
	ErrPrecondition = errors.New("worldsync: precondition failed")
)

// ObjectInfo describes an object as one response saw it. ETag carries no
// quotes. Date is the store's clock at that response, the only clock all
// nodes share.
type ObjectInfo struct {
	ETag string
	Size int64
	Date time.Time
}

// PutCondition: IfNoneMatch creates only; a non-empty IfMatch replaces only
// the object with that ETag.
type PutCondition struct {
	IfNoneMatch bool
	IfMatch     string
}

type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error)
	// Delete of a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List returns full keys under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
}
```

`internal/worldsync/layout.go`:

```go
package worldsync

import "strings"

const (
	ManifestName = "manifest.json"
	LeaseName    = "lease.json"
	ObjectsDir   = "objects/"
	PacksDir     = "packs/"
	// PackBelow: smaller files travel in the generation's pack. The store
	// bills at least 64 kB per object.
	PackBelow = 64 << 10
)

func join(base, rest string) string {
	if base == "" {
		return rest
	}
	return strings.TrimSuffix(base, "/") + "/" + rest
}

// WorldPrefix is where one world's objects live; world is
// "<namespace>/<group>/<key>".
func WorldPrefix(base, world string) string { return join(base, world) + "/" }

func DeletionPrefix(base string) string { return join(base, ".deletions/") }

func DeletionKey(base, world string) string { return join(base, ".deletions/"+world) }
```

`internal/worldsync/memstore.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is a Store in memory, for tests. Its ETags are bare MD5 hex, as
// a single-part S3 upload's are.
type MemStore struct {
	mu      sync.Mutex
	clock   func() time.Time
	objects map[string][]byte
	outage  error
}

func NewMemStore(clock func() time.Time) *MemStore {
	return &MemStore{clock: clock, objects: map[string][]byte{}}
}

func (m *MemStore) SetOutage(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outage = err
}

func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func etagOf(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func (m *MemStore) info(b []byte) ObjectInfo {
	return ObjectInfo{ETag: etagOf(b), Size: int64(len(b)), Date: m.clock()}
}

func (m *MemStore) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return nil, ObjectInfo{}, m.outage
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), m.info(b), nil
}

func (m *MemStore) Head(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return ObjectInfo{}, m.outage
	}
	b, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return m.info(b), nil
}

func (m *MemStore) Put(_ context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return ObjectInfo{}, m.outage
	}
	old, exists := m.objects[key]
	if cond.IfNoneMatch && exists {
		return ObjectInfo{}, ErrPrecondition
	}
	if cond.IfMatch != "" && (!exists || etagOf(old) != cond.IfMatch) {
		return ObjectInfo{}, ErrPrecondition
	}
	m.objects[key] = b
	return m.info(b), nil
}

func (m *MemStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return m.outage
	}
	delete(m.objects, key)
	return nil
}

func (m *MemStore) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return nil, m.outage
	}
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}
```

- [ ] **Step 4: Run tests**

Run: `nix develop -c go test ./internal/worldsync/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worldsync/
git commit -m "feat(worldsync): store interface and an in-memory store"
```

---

### Task 3: S3 store over aws-sdk-go-v2

**Files:**
- Create: `internal/worldsync/s3store.go`
- Test: `internal/worldsync/s3store_test.go`
- Modify: `go.mod`, `go.sum`, `flake.nix` (all `vendorHash`)

**Interfaces:**
- Consumes: `Store`, `ObjectInfo`, `PutCondition`, `ErrNotFound`, `ErrPrecondition` (Task 2).
- Produces:
  ```go
  type S3Config struct { Endpoint, Region, Bucket, AccessKey, SecretKey string }
  func S3ConfigFromEnv(getenv func(string) string) (S3Config, string /*prefix*/, error)
  func NewS3Store(cfg S3Config) (*S3Store, error)
  ```

- [ ] **Step 1: Add the dependencies**

```bash
nix develop -c go get github.com/aws/aws-sdk-go-v2@latest github.com/aws/aws-sdk-go-v2/service/s3@latest github.com/aws/aws-sdk-go-v2/credentials@latest github.com/aws/smithy-go@latest
```

- [ ] **Step 2: Write the failing test**

`internal/worldsync/s3store_test.go` runs the store against an `httptest.Server` that records requests:

```go
package worldsync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, path, ifMatch, ifNoneMatch string
}

func fakeS3(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*S3Store, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recorded{r.Method, r.URL.Path, r.Header.Get("If-Match"), r.Header.Get("If-None-Match")})
		mu.Unlock()
		w.Header().Set("Date", "Tue, 06 Oct 2026 12:00:00 GMT")
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	s, err := NewS3Store(S3Config{Endpoint: srv.URL, Region: "fsn1", Bucket: "worlds", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	return s, &reqs
}

func TestS3PutSendsABareIfMatch(t *testing.T) {
	s, reqs := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"0123abcd"`)
	})
	info, err := s.Put(context.Background(), "k", bytes.NewReader([]byte("x")), PutCondition{IfMatch: "cafe"})
	if err != nil {
		t.Fatal(err)
	}
	if (*reqs)[0].ifMatch != "cafe" {
		t.Fatalf("If-Match = %q, want the bare ETag", (*reqs)[0].ifMatch)
	}
	if info.ETag != "0123abcd" {
		t.Fatalf("ETag = %q, want it without quotes", info.ETag)
	}
	if want := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC); !info.Date.Equal(want) {
		t.Fatalf("Date = %v, want the response's Date header", info.Date)
	}
}

func TestS3PutCreateOnlySendsIfNoneMatchStar(t *testing.T) {
	s, reqs := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"e"`)
	})
	if _, err := s.Put(context.Background(), "k", bytes.NewReader(nil), PutCondition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if (*reqs)[0].ifNoneMatch != "*" {
		t.Fatalf("If-None-Match = %q", (*reqs)[0].ifNoneMatch)
	}
}

func TestS3MapsStatusCodes(t *testing.T) {
	s, _ := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusPreconditionFailed)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
		}
	})
	if _, err := s.Put(context.Background(), "k", bytes.NewReader(nil), PutCondition{IfMatch: "x"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("412: err = %v", err)
	}
	if _, err := s.Head(context.Background(), "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 Head: err = %v", err)
	}
	if _, _, err := s.Get(context.Background(), "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 Get: err = %v", err)
	}
}

func TestS3ConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"WORLDSYNC_ENDPOINT": "https://s3.example", "WORLDSYNC_REGION": "r",
		"WORLDSYNC_BUCKET": "b", "WORLDSYNC_PREFIX": "p",
		"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "s",
	}
	cfg, prefix, err := S3ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || cfg.Bucket != "b" || prefix != "p" {
		t.Fatalf("cfg=%+v prefix=%q err=%v", cfg, prefix, err)
	}
	delete(env, "WORLDSYNC_BUCKET")
	if _, _, err := S3ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "WORLDSYNC_BUCKET") {
		t.Fatalf("missing bucket: err = %v", err)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `nix develop -c go test ./internal/worldsync/ -run S3 -count=1`
Expected: FAIL, `undefined: NewS3Store`.

- [ ] **Step 4: Implement**

`internal/worldsync/s3store.go`:

```go
package worldsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type S3Config struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey string
}

func S3ConfigFromEnv(getenv func(string) string) (S3Config, string, error) {
	cfg := S3Config{
		Endpoint:  getenv("WORLDSYNC_ENDPOINT"),
		Region:    getenv("WORLDSYNC_REGION"),
		Bucket:    getenv("WORLDSYNC_BUCKET"),
		AccessKey: getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: getenv("AWS_SECRET_ACCESS_KEY"),
	}
	for name, v := range map[string]string{
		"WORLDSYNC_ENDPOINT": cfg.Endpoint, "WORLDSYNC_REGION": cfg.Region,
		"WORLDSYNC_BUCKET": cfg.Bucket, "AWS_ACCESS_KEY_ID": cfg.AccessKey,
		"AWS_SECRET_ACCESS_KEY": cfg.SecretKey,
	} {
		if v == "" {
			return S3Config{}, "", fmt.Errorf("%s is not set", name)
		}
	}
	return cfg, getenv("WORLDSYNC_PREFIX"), nil
}

type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		// Stores outside AWS reject or ignore the CRC headers newer SDKs add.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &S3Store{client: client, bucket: cfg.Bucket}, nil
}

func bare(etag *string) string {
	if etag == nil {
		return ""
	}
	return strings.Trim(*etag, `"`)
}

func dateOf(md middleware.Metadata) time.Time {
	raw, ok := awshttp.GetRawResponse(md).(*smithyhttp.Response)
	if !ok || raw == nil {
		return time.Time{}
	}
	d, err := http.ParseTime(raw.Header.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return d
}

func mapErr(err error) error {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		switch re.HTTPStatusCode() {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %v", ErrNotFound, err)
		case http.StatusPreconditionFailed, http.StatusConflict:
			return fmt.Errorf("%w: %v", ErrPrecondition, err)
		}
	}
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, ObjectInfo{}, mapErr(err)
	}
	return out.Body, ObjectInfo{ETag: bare(out.ETag), Size: aws.ToInt64(out.ContentLength), Date: dateOf(out.ResultMetadata)}, nil
}

func (s *S3Store) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return ObjectInfo{}, mapErr(err)
	}
	return ObjectInfo{ETag: bare(out.ETag), Size: aws.ToInt64(out.ContentLength), Date: dateOf(out.ResultMetadata)}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: body}
	if cond.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	if cond.IfMatch != "" {
		// Bare on purpose: Hetzner answers 412 to the quoted form even when
		// it matches (tried 2026-10-06).
		in.IfMatch = aws.String(cond.IfMatch)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		return ObjectInfo{}, mapErr(err)
	}
	return ObjectInfo{ETag: bare(out.ETag), Date: dateOf(out.ResultMetadata)}, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil && !errors.Is(mapErr(err), ErrNotFound) {
		return mapErr(err)
	}
	return nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}
```

If the SDK version in use names a field differently (e.g. `RequestChecksumCalculation`), follow the compiler; the behaviour to keep is "only when required".

- [ ] **Step 5: Run tests, then fix the vendorHash**

Run: `nix develop -c go test ./internal/worldsync/ -count=1` → PASS.
Then: `nix build .#spawnery-operator --no-link 2>&1 | grep got:` and replace every `vendorHash` value in `flake.nix` with the printed hash; `nix build .#spawnery-operator --no-link` must then succeed.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum flake.nix internal/worldsync/
git commit -m "feat(worldsync): S3 store with bare ETags in If-Match"
```

---

### Task 4: Manifest, scan and the local snapshot

**Files:**
- Create: `internal/worldsync/manifest.go`, `internal/worldsync/snapshot.go`
- Test: `internal/worldsync/snapshot_test.go`

**Interfaces:**
- Consumes: `prune.Keep`, `prune.ControlDir` (Task 1), `Store`, `ManifestName` (Task 2).
- Produces:
  ```go
  type FileEntry struct { Path string; Size int64; Mode uint32; MTime int64; Object string }
  type Manifest struct { WorldID string; Generation int64; Files []FileEntry }
  func ReadManifest(ctx context.Context, st Store, prefix string) (Manifest, string /*etag*/, error) // ErrNotFound when none
  type LocalFile struct { Path string; Size int64; Mode uint32; MTime int64 }
  func Scan(dir string, keep prune.Keep) ([]LocalFile, error)
  type SnapFile struct { FileEntry; Copied bool }
  type Snap struct { Seq int64; Files []SnapFile }
  func TakeSnapshot(dataDir, snapDir string, keep prune.Keep, base []FileEntry, seq int64) (Snap, error)
  func ReadSnap(snapDir string) (Snap, error)
  var ErrUnsettled error   // a file kept changing during its copy
  ```
  JSON field names: `worldId`, `generation`, `files`, `path`, `size`, `mode`, `mtime` (Unix nanoseconds), `object`, `seq`, `copied`.

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/snapshot_test.go`:

```go
package worldsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/prune"
)

func writeFile(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func keepOf(t *testing.T, entries ...string) prune.Keep {
	t.Helper()
	k, err := prune.ParseKeep(entries)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestScanFindsOnlyKeptRegularFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(dir, "worlds/world/level.dat"), 10, now)
	writeFile(t, filepath.Join(dir, "worlds/world/region/r.0.0.mca"), 100_000, now)
	writeFile(t, filepath.Join(dir, "logs/latest.log"), 5, now)
	writeFile(t, filepath.Join(dir, prune.ControlDir, "ready"), 0, now)

	files, err := Scan(dir, keepOf(t, "worlds/world"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "worlds/world/level.dat" || files[1].Path != "worlds/world/region/r.0.0.mca" {
		t.Fatalf("Scan = %+v", files)
	}
	if files[1].MTime != now.UnixNano() {
		t.Fatalf("mtime = %d, want %d", files[1].MTime, now.UnixNano())
	}
}

func TestSnapshotCopiesChangedLargeFilesAndEverySmallOne(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	then := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 10, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/same.mca"), PackBelow, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/new.mca"), PackBelow+1, then)
	base := []FileEntry{
		{Path: "worlds/world/level.dat", Size: 10, MTime: then.UnixNano(), Object: "packs/1.tar.gz"},
		{Path: "worlds/world/region/same.mca", Size: PackBelow, MTime: then.UnixNano(), Object: "objects/aaa"},
		{Path: "worlds/world/region/gone.mca", Size: PackBelow, MTime: then.UnixNano(), Object: "objects/bbb"},
	}

	s, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), base, 7)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]SnapFile{}
	for _, f := range s.Files {
		byPath[f.Path] = f
	}
	if !byPath["worlds/world/level.dat"].Copied {
		t.Error("a small file was not copied; small files always travel in the pack")
	}
	if f := byPath["worlds/world/region/same.mca"]; f.Copied || f.Object != "objects/aaa" {
		t.Errorf("an unchanged large file: %+v, want it carried over by reference", f)
	}
	if !byPath["worlds/world/region/new.mca"].Copied {
		t.Error("a new large file was not copied")
	}
	if _, ok := byPath["worlds/world/region/gone.mca"]; ok {
		t.Error("a deleted file is still in the snapshot")
	}
	if _, err := os.Stat(filepath.Join(snap, "worlds/world/region/new.mca")); err != nil {
		t.Errorf("the copy is not in the snapshot directory: %v", err)
	}
	again, err := ReadSnap(snap)
	if err != nil || again.Seq != 7 || len(again.Files) != len(s.Files) {
		t.Fatalf("ReadSnap = %+v, %v", again, err)
	}
}

// Review Focus 3: a file that changes during every copy attempt fails the
// snapshot instead of uploading a torn file.
func TestSnapshotRefusesAFileThatNeverSettles(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	path := filepath.Join(data, "worlds/world/region/hot.mca")
	writeFile(t, path, PackBelow+1, time.Unix(1, 0))
	tick := int64(2)
	copyHook = func(p string) {
		if p == path {
			writeFile(t, path, PackBelow+1, time.Unix(tick, 0))
			tick++
		}
	}
	t.Cleanup(func() { copyHook = nil })

	if _, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), nil, 1); !errors.Is(err, ErrUnsettled) {
		t.Fatalf("err = %v, want ErrUnsettled", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `nix develop -c go test ./internal/worldsync/ -run 'Scan|Snapshot' -count=1`
Expected: FAIL, undefined `Scan`, `TakeSnapshot`, `copyHook`.

- [ ] **Step 3: Implement**

`internal/worldsync/manifest.go`:

```go
package worldsync

import (
	"context"
	"encoding/json"
	"fmt"
)

type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	MTime  int64  `json:"mtime"`
	Object string `json:"object"`
}

type Manifest struct {
	WorldID    string      `json:"worldId"`
	Generation int64       `json:"generation"`
	Files      []FileEntry `json:"files"`
}

func ReadManifest(ctx context.Context, st Store, prefix string) (Manifest, string, error) {
	rc, info, err := st.Get(ctx, prefix+ManifestName)
	if err != nil {
		return Manifest{}, "", err
	}
	defer rc.Close()
	var m Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return Manifest{}, "", fmt.Errorf("decode %s%s: %w", prefix, ManifestName, err)
	}
	return m, info.ETag, nil
}
```

`internal/worldsync/snapshot.go`:

```go
package worldsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/spawnery/spawnery/internal/prune"
)

var ErrUnsettled = errors.New("worldsync: a file kept changing while it was copied")

const snapFile = "snapshot.json"

type LocalFile struct {
	Path  string
	Size  int64
	Mode  uint32
	MTime int64
}

type SnapFile struct {
	FileEntry
	// Copied: the content is in the snapshot directory. Otherwise Object names
	// where it already is.
	Copied bool `json:"copied"`
}

type Snap struct {
	Seq   int64      `json:"seq"`
	Files []SnapFile `json:"files"`
}

// copyHook runs before each copy attempt; tests use it to change a file.
var copyHook func(path string)

func Scan(dir string, keep prune.Keep) ([]LocalFile, error) {
	var out []LocalFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == prune.ControlDir {
			return fs.SkipDir
		}
		if d.IsDir() {
			if keep.Holds(rel) || keep.Toward(rel) {
				return nil
			}
			return fs.SkipDir
		}
		if !d.Type().IsRegular() || !keep.Holds(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, LocalFile{Path: rel, Size: info.Size(), Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func statOf(path string) (int64, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	return info.Size(), info.ModTime().UnixNano(), nil
}

// copyStable copies src to dst and checks that size and mtime did not move
// during the copy, up to three attempts. It returns the stat it copied at.
func copyStable(src, dst string) (int64, int64, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if copyHook != nil {
			copyHook(src)
		}
		size, mtime, err := statOf(src)
		if err != nil {
			return 0, 0, err
		}
		if err := copyFile(src, dst); err != nil {
			return 0, 0, err
		}
		size2, mtime2, err := statOf(src)
		if err != nil {
			return 0, 0, err
		}
		if size == size2 && mtime == mtime2 {
			return size, mtime, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: %s", ErrUnsettled, src)
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// TakeSnapshot copies what an upload needs out of dataDir into snapDir: every
// file below PackBelow, and every larger file whose size or mtime differs
// from base. Unchanged large files are carried over by reference.
func TakeSnapshot(dataDir, snapDir string, keep prune.Keep, base []FileEntry, seq int64) (Snap, error) {
	files, err := Scan(dataDir, keep)
	if err != nil {
		return Snap{}, err
	}
	prev := make(map[string]FileEntry, len(base))
	for _, b := range base {
		prev[b.Path] = b
	}
	s := Snap{Seq: seq}
	for _, f := range files {
		b, ok := prev[f.Path]
		if ok && f.Size >= PackBelow && b.Size == f.Size && b.MTime == f.MTime {
			s.Files = append(s.Files, SnapFile{FileEntry: b})
			continue
		}
		size, mtime, err := copyStable(filepath.Join(dataDir, f.Path), filepath.Join(snapDir, f.Path))
		if err != nil {
			return Snap{}, err
		}
		s.Files = append(s.Files, SnapFile{FileEntry: FileEntry{Path: f.Path, Size: size, Mode: f.Mode, MTime: mtime}, Copied: true})
	}
	if err := writeJSON(filepath.Join(snapDir, snapFile), s); err != nil {
		return Snap{}, err
	}
	return s, nil
}

func ReadSnap(snapDir string) (Snap, error) {
	var s Snap
	return s, readJSON(filepath.Join(snapDir, snapFile), &s)
}

// writeJSON replaces path atomically.
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
```

- [ ] **Step 4: Run tests**

Run: `nix develop -c go test ./internal/worldsync/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worldsync/
git commit -m "feat(worldsync): manifest, scan and local snapshots"
```

---

### Task 5: Upload and download

**Files:**
- Create: `internal/worldsync/transfer.go`
- Test: `internal/worldsync/transfer_test.go`
- Modify: `go.mod` (make `golang.org/x/sync` a direct dependency; vendorHash only if `go mod tidy` changes `go.sum`)

**Interfaces:**
- Consumes: Tasks 2 and 4.
- Produces:
  ```go
  var ErrConflict error // the manifest moved under us: another writer
  func UploadSnapshot(ctx context.Context, st Store, prefix, snapDir string, prev *Manifest, prevETag string, newWorldID func() string) (Manifest, string, error)
  func Download(ctx context.Context, st Store, prefix, dataDir string, m Manifest, parallel int) error
  func NewWorldID() string   // 16 random bytes, hex
  ```

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/transfer_test.go`:

```go
package worldsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const prefix = "ns/g/k/"

func snapshotOf(t *testing.T, data string, base []FileEntry, seq int64) string {
	t.Helper()
	snap := t.TempDir()
	if _, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), base, seq); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestARoundTripRestoresContentModeAndMTime(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	then := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 10, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/r.0.0.mca"), PackBelow+5, then)

	m, etag, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", func() string { return "w1" })
	if err != nil {
		t.Fatal(err)
	}
	if m.Generation != 1 || m.WorldID != "w1" || etag == "" {
		t.Fatalf("manifest = %+v etag=%q", m, etag)
	}

	out := t.TempDir()
	if err := Download(context.Background(), st, prefix, out, m, 4); err != nil {
		t.Fatal(err)
	}
	files, err := Scan(out, keepOf(t, "worlds/world"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].Size != PackBelow+5 || files[1].MTime != then.UnixNano() {
		t.Fatalf("restored = %+v", files)
	}
}

func TestASecondGenerationUploadsOnlyWhatChangedAndCollectsTheRest(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	then := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/b.mca"), PackBelow+1, then)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, then)
	m1, e1, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(data, "worlds/world/region/b.mca"))
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+2, then.Add(time.Minute))
	m2, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Generation != 2 || m2.WorldID != m1.WorldID {
		t.Fatalf("m2 = %+v", m2)
	}
	var objects, packs int
	for _, k := range st.Keys() {
		switch {
		case strings.HasPrefix(k, prefix+ObjectsDir):
			objects++
		case strings.HasPrefix(k, prefix+PacksDir):
			packs++
		}
	}
	if objects != 1 || packs != 1 {
		t.Fatalf("after gen 2: %d objects, %d packs (%v); the old ones should be collected", objects, packs, st.Keys())
	}
}

func TestAStaleManifestETagIsAConflict(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	m1, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, "stale", NewWorldID); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a first upload over an existing world: err = %v, want ErrConflict", err)
	}
}

// Review Focus 5: an outage during the objects must leave the old manifest in
// place, and the same snapshot must upload cleanly afterwards.
func TestAnOutageMidUploadLeavesTheOldManifestAndRetriesClean(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+1, time.Unix(1, 0))
	m1, e1, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+9, time.Unix(2, 0))
	snap := snapshotOf(t, data, m1.Files, 2)

	st.SetOutage(errors.New("down"))
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snap, &m1, e1, NewWorldID); err == nil {
		t.Fatal("an upload during an outage succeeded")
	}
	st.SetOutage(nil)
	got, _, err := ReadManifest(context.Background(), st, prefix)
	if err != nil || got.Generation != 1 {
		t.Fatalf("manifest after a failed upload = %+v, %v; want generation 1 untouched", got, err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snap, &m1, e1, NewWorldID); err != nil {
		t.Fatalf("the retry of the same snapshot: %v", err)
	}
}

func TestDownloadRefusesAPathOutsideTheDirectory(t *testing.T) {
	st := NewMemStore(time.Now)
	m := Manifest{WorldID: "w", Generation: 1, Files: []FileEntry{{Path: "../escape", Size: 1, Object: "objects/x"}}}
	if err := Download(context.Background(), st, prefix, t.TempDir(), m, 1); err == nil {
		t.Fatal("a manifest path with .. was accepted")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `nix develop -c go test ./internal/worldsync/ -run 'RoundTrip|Generation|Conflict|Outage|Download' -count=1`
Expected: FAIL, undefined `UploadSnapshot`.

- [ ] **Step 3: Implement**

`internal/worldsync/transfer.go`:

```go
package worldsync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
)

var ErrConflict = errors.New("worldsync: the world's manifest changed under this writer")

const uploadParallel = 16

func NewWorldID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func UploadSnapshot(ctx context.Context, st Store, prefix, snapDir string, prev *Manifest, prevETag string, newWorldID func() string) (Manifest, string, error) {
	snap, err := ReadSnap(snapDir)
	if err != nil {
		return Manifest{}, "", err
	}
	m := Manifest{Generation: 1}
	if prev != nil {
		m.WorldID, m.Generation = prev.WorldID, prev.Generation+1
	} else {
		m.WorldID = newWorldID()
	}
	pack := PacksDir + strconv.FormatInt(m.Generation, 10) + ".tar.gz"

	var packBuf bytes.Buffer
	gz := gzip.NewWriter(&packBuf)
	tw := tar.NewWriter(gz)
	packed := 0
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadParallel)
	m.Files = make([]FileEntry, len(snap.Files))
	for i, f := range snap.Files {
		e := f.FileEntry
		switch {
		case !f.Copied:
		case e.Size < PackBelow:
			b, err := os.ReadFile(filepath.Join(snapDir, e.Path))
			if err != nil {
				return Manifest{}, "", err
			}
			if err := tw.WriteHeader(&tar.Header{Name: e.Path, Mode: int64(e.Mode), Size: int64(len(b)), ModTime: time.Unix(0, e.MTime)}); err != nil {
				return Manifest{}, "", err
			}
			if _, err := tw.Write(b); err != nil {
				return Manifest{}, "", err
			}
			e.Object = pack
			packed++
		default:
			sum, err := hashFile(filepath.Join(snapDir, e.Path))
			if err != nil {
				return Manifest{}, "", err
			}
			e.Object = ObjectsDir + sum
			key, src := prefix+e.Object, filepath.Join(snapDir, e.Path)
			g.Go(func() error {
				if _, err := st.Head(gctx, key); err == nil {
					return nil
				} else if !errors.Is(err, ErrNotFound) {
					return err
				}
				f, err := os.Open(src)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = st.Put(gctx, key, f, PutCondition{})
				return err
			})
		}
		m.Files[i] = e
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, "", err
	}
	if err := gz.Close(); err != nil {
		return Manifest{}, "", err
	}
	if packed > 0 {
		g.Go(func() error {
			_, err := st.Put(gctx, prefix+pack, bytes.NewReader(packBuf.Bytes()), PutCondition{})
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return Manifest{}, "", err
	}

	body, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, "", err
	}
	cond := PutCondition{IfNoneMatch: prev == nil, IfMatch: prevETag}
	if prev == nil {
		cond.IfMatch = ""
	}
	info, err := st.Put(ctx, prefix+ManifestName, bytes.NewReader(body), cond)
	if errors.Is(err, ErrPrecondition) {
		return Manifest{}, "", fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err != nil {
		return Manifest{}, "", err
	}

	if prev != nil {
		keep := map[string]bool{}
		for _, e := range m.Files {
			keep[e.Object] = true
		}
		for _, e := range prev.Files {
			if !keep[e.Object] {
				keep[e.Object] = true // delete each once
				_ = st.Delete(ctx, prefix+e.Object)
			}
		}
	}
	return m, info.ETag, nil
}

func localPath(dir, rel string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("worldsync: manifest path %q leaves the world", rel)
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
}

// place writes r to path through a temporary file, then sets mode and mtime.
func place(path string, r io.Reader, mode uint32, mtime int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".worldsync-part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if mode != 0 {
		if err := os.Chmod(tmp, os.FileMode(mode)); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	t := time.Unix(0, mtime)
	return os.Chtimes(path, t, t)
}

func Download(ctx context.Context, st Store, prefix, dataDir string, m Manifest, parallel int) error {
	byPath := map[string]FileEntry{}
	packs := map[string]bool{}
	for _, e := range m.Files {
		if _, err := localPath(dataDir, e.Path); err != nil {
			return err
		}
		byPath[e.Path] = e
		if e.Size < PackBelow {
			packs[e.Object] = true
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for pack := range packs {
		g.Go(func() error { return extractPack(gctx, st, prefix+pack, dataDir, byPath) })
	}
	for _, e := range m.Files {
		if e.Size < PackBelow {
			continue
		}
		g.Go(func() error {
			rc, _, err := st.Get(gctx, prefix+e.Object)
			if err != nil {
				return fmt.Errorf("get %s for %s: %w", e.Object, e.Path, err)
			}
			defer rc.Close()
			dst, _ := localPath(dataDir, e.Path)
			return place(dst, rc, e.Mode, e.MTime)
		})
	}
	return g.Wait()
}

func extractPack(ctx context.Context, st Store, key, dataDir string, byPath map[string]FileEntry) error {
	rc, _, err := st.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		e, ok := byPath[h.Name]
		if !ok {
			continue // a pack may hold files an older manifest named
		}
		dst, err := localPath(dataDir, h.Name)
		if err != nil {
			return err
		}
		if err := place(dst, tr, e.Mode, e.MTime); err != nil {
			return err
		}
	}
}
```

Note the pack is uploaded once per generation even when the only change is a large file; that keeps the manifest's small files in one object and is the price of not tracking them one by one.

- [ ] **Step 4: Tidy and run tests**

Run: `nix develop -c go mod tidy && nix develop -c go test ./internal/worldsync/ -count=1`
Expected: PASS. If `go.sum` changed, redo the vendorHash step from Task 3.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum flake.nix internal/worldsync/
git commit -m "feat(worldsync): upload snapshots and download worlds"
```

---

### Task 6: The lease

**Files:**
- Create: `internal/worldsync/lease.go`
- Test: `internal/worldsync/lease_test.go`

**Interfaces:**
- Consumes: `Store`, `LeaseName` (Task 2).
- Produces:
  ```go
  type Lease struct { Node string; Pod string; RenewedAt time.Time }  // json: node, pod, renewedAt
  type HeldError struct { Node string }   // Error(): "held by node <Node>"
  var ErrLeaseLost error
  const StaleAfter = 10 * time.Minute
  func TakeLease(ctx context.Context, st Store, prefix string, me Lease, staleAfter time.Duration) (string /*etag*/, error)
  func RenewLease(ctx context.Context, st Store, prefix string, me Lease, etag string) (string, error) // ErrLeaseLost on 412
  func ReleaseLease(ctx context.Context, st Store, prefix, etag string) error
  func ReadLease(ctx context.Context, st Store, prefix string) (Lease, ObjectInfo, error)
  ```

- [ ] **Step 1: Write the failing tests**

```go
package worldsync

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTheFirstTakerWinsAndAnotherNodeIsTurnedAway(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	_, err := TakeLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: now}, StaleAfter)
	var held *HeldError
	if !errors.As(err, &held) || held.Node != "a" {
		t.Fatalf("err = %v, want HeldError{a}", err)
	}
}

func TestTheSameNodeTakesItsOwnLeaseAgain(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	TakeLease(ctx, st, prefix, Lease{Node: "a", Pod: "p1", RenewedAt: now}, StaleAfter)
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", Pod: "p2", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatalf("the holder could not take its own lease again: %v", err)
	}
}

func TestAStaleLeaseIsTakenOverOnTheStoresClock(t *testing.T) {
	storeNow := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return storeNow })
	ctx := context.Background()
	TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: storeNow}, StaleAfter)
	storeNow = storeNow.Add(StaleAfter + time.Second)
	etag, err := TakeLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: storeNow}, StaleAfter)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := RenewLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: storeNow}, "old-etag-of-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the old holder's renewal: err = %v, want ErrLeaseLost", err)
	}
	if _, err := RenewLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: storeNow}, etag); err != nil {
		t.Fatalf("the new holder's renewal: %v", err)
	}
}

func TestReleaseDeletesOnlyTheHoldersLease(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	etag, _ := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: now}, StaleAfter)
	if err := ReleaseLease(ctx, st, prefix, "not-mine"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("release with a foreign ETag: err = %v", err)
	}
	if err := ReleaseLease(ctx, st, prefix, etag); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadLease(ctx, st, prefix); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lease after release: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `nix develop -c go test ./internal/worldsync/ -run Lease -count=1` → FAIL (undefined).

- [ ] **Step 3: Implement** `internal/worldsync/lease.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const StaleAfter = 10 * time.Minute

var ErrLeaseLost = errors.New("worldsync: the lease is no longer this node's")

type Lease struct {
	Node      string    `json:"node"`
	Pod       string    `json:"pod"`
	RenewedAt time.Time `json:"renewedAt"`
}

type HeldError struct{ Node string }

func (e *HeldError) Error() string { return "worldsync: the world is held by node " + e.Node }

func ReadLease(ctx context.Context, st Store, prefix string) (Lease, ObjectInfo, error) {
	rc, info, err := st.Get(ctx, prefix+LeaseName)
	if err != nil {
		return Lease{}, ObjectInfo{}, err
	}
	defer rc.Close()
	var l Lease
	if err := json.NewDecoder(rc).Decode(&l); err != nil {
		return Lease{}, ObjectInfo{}, fmt.Errorf("decode lease: %w", err)
	}
	return l, info, nil
}

func putLease(ctx context.Context, st Store, prefix string, l Lease, cond PutCondition) (string, error) {
	b, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	info, err := st.Put(ctx, prefix+LeaseName, bytes.NewReader(b), cond)
	if err != nil {
		return "", err
	}
	return info.ETag, nil
}

// TakeLease creates the lease, or rewrites it when this node holds it or
// when it is older than staleAfter by the store's own clock.
func TakeLease(ctx context.Context, st Store, prefix string, me Lease, staleAfter time.Duration) (string, error) {
	etag, err := putLease(ctx, st, prefix, me, PutCondition{IfNoneMatch: true})
	if !errors.Is(err, ErrPrecondition) {
		return etag, err
	}
	cur, info, err := ReadLease(ctx, st, prefix)
	if errors.Is(err, ErrNotFound) {
		return TakeLease(ctx, st, prefix, me, staleAfter) // released in between
	}
	if err != nil {
		return "", err
	}
	if cur.Node != me.Node && info.Date.Sub(cur.RenewedAt) <= staleAfter {
		return "", &HeldError{Node: cur.Node}
	}
	etag, err = putLease(ctx, st, prefix, me, PutCondition{IfMatch: info.ETag})
	if errors.Is(err, ErrPrecondition) {
		return "", &HeldError{Node: cur.Node}
	}
	return etag, err
}

func RenewLease(ctx context.Context, st Store, prefix string, me Lease, etag string) (string, error) {
	next, err := putLease(ctx, st, prefix, me, PutCondition{IfMatch: etag})
	if errors.Is(err, ErrPrecondition) {
		return "", ErrLeaseLost
	}
	return next, err
}

// ReleaseLease deletes the lease if it is still the one with etag. The check
// and the delete are two requests; a takeover needs ten minutes without a
// renewal, which a holder that is releasing has just renewed.
func ReleaseLease(ctx context.Context, st Store, prefix, etag string) error {
	info, err := st.Head(ctx, prefix+LeaseName)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.ETag != etag {
		return ErrLeaseLost
	}
	return st.Delete(ctx, prefix+LeaseName)
}
```

- [ ] **Step 4: Run tests** — `nix develop -c go test ./internal/worldsync/ -count=1` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worldsync/
git commit -m "feat(worldsync): a lease in the bucket keeps a world on one node"
```

---

### Task 7: Deletion marker and sweeper

**Files:**
- Create: `internal/worldsync/deletion.go`
- Test: `internal/worldsync/deletion_test.go`

**Interfaces:**
- Consumes: Tasks 2 and 6.
- Produces:
  ```go
  func MarkDeleted(ctx context.Context, st Store, base, world string) error
  func DeletionPending(ctx context.Context, st Store, base, world string) (bool, error)
  func WorldExists(ctx context.Context, st Store, base, world string) (bool, error) // a manifest exists
  type Sweeper struct { Store Store; Base string; Interval time.Duration; StaleAfter time.Duration; Log logr.Logger }
  func (s *Sweeper) SweepOnce(ctx context.Context) error
  func (s *Sweeper) Start(ctx context.Context) error   // manager.Runnable
  func (s *Sweeper) NeedLeaderElection() bool          // true
  ```

- [ ] **Step 1: Write the failing tests**

```go
package worldsync

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestTheSweeperDeletesAMarkedWorldWithoutALease(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	p := WorldPrefix("", "ns/g/k")
	for _, k := range []string{ManifestName, "objects/a", "packs/1.tar.gz"} {
		st.Put(ctx, p+k, bytes.NewReader(nil), PutCondition{})
	}
	st.Put(ctx, "ns/g/other/manifest.json", bytes.NewReader(nil), PutCondition{})
	if err := MarkDeleted(ctx, st, "", "ns/g/k"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := DeletionPending(ctx, st, "", "ns/g/k"); !pending {
		t.Fatal("no deletion pending after MarkDeleted")
	}

	s := &Sweeper{Store: st, StaleAfter: StaleAfter, Log: logr.Discard()}
	if err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	keys := st.Keys()
	if len(keys) != 1 || keys[0] != "ns/g/other/manifest.json" {
		t.Fatalf("after the sweep: %v", keys)
	}
}

func TestTheSweeperWaitsForAFreshLease(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	p := WorldPrefix("", "ns/g/k")
	st.Put(ctx, p+ManifestName, bytes.NewReader(nil), PutCondition{})
	TakeLease(ctx, st, p, Lease{Node: "a", RenewedAt: now}, StaleAfter)
	MarkDeleted(ctx, st, "", "ns/g/k")

	s := &Sweeper{Store: st, StaleAfter: StaleAfter, Log: logr.Discard()}
	s.SweepOnce(ctx)
	if exists, _ := WorldExists(ctx, st, "", "ns/g/k"); !exists {
		t.Fatal("the sweep deleted a world whose lease is fresh: a node is still uploading it")
	}
	now = now.Add(StaleAfter + time.Second)
	s.SweepOnce(ctx)
	if exists, _ := WorldExists(ctx, st, "", "ns/g/k"); exists {
		t.Fatal("the sweep left a world whose lease went stale")
	}
	if pending, _ := DeletionPending(ctx, st, "", "ns/g/k"); pending {
		t.Fatal("the marker outlived the world")
	}
}
```

- [ ] **Step 2: Run to verify failure** — FAIL (undefined).

- [ ] **Step 3: Implement** `internal/worldsync/deletion.go`:

```go
package worldsync

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

func MarkDeleted(ctx context.Context, st Store, base, world string) error {
	_, err := st.Put(ctx, DeletionKey(base, world), bytes.NewReader(nil), PutCondition{})
	return err
}

func exists(ctx context.Context, st Store, key string) (bool, error) {
	_, err := st.Head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func DeletionPending(ctx context.Context, st Store, base, world string) (bool, error) {
	return exists(ctx, st, DeletionKey(base, world))
}

func WorldExists(ctx context.Context, st Store, base, world string) (bool, error) {
	return exists(ctx, st, WorldPrefix(base, world)+ManifestName)
}

// Sweeper deletes worlds marked by MarkDeleted once no node holds them.
type Sweeper struct {
	Store      Store
	Base       string
	Interval   time.Duration
	StaleAfter time.Duration
	Log        logr.Logger
}

func (s *Sweeper) NeedLeaderElection() bool { return true }

func (s *Sweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if err := s.SweepOnce(ctx); err != nil {
			s.Log.Error(err, "world deletion sweep failed; retrying next interval")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (s *Sweeper) SweepOnce(ctx context.Context) error {
	markers, err := s.Store.List(ctx, DeletionPrefix(s.Base))
	if err != nil {
		return err
	}
	var errs []error
	for _, marker := range markers {
		world := strings.TrimPrefix(marker, DeletionPrefix(s.Base))
		if err := s.sweep(ctx, world, marker); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Sweeper) sweep(ctx context.Context, world, marker string) error {
	prefix := WorldPrefix(s.Base, world)
	l, info, err := ReadLease(ctx, s.Store, prefix)
	switch {
	case err == nil && info.Date.Sub(l.RenewedAt) <= s.StaleAfter:
		return nil
	case err != nil && !errors.Is(err, ErrNotFound):
		return err
	}
	keys, err := s.Store.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k == prefix+ManifestName {
			continue // last but one: a world without manifest is gone for the node agent
		}
		if err := s.Store.Delete(ctx, k); err != nil {
			return err
		}
	}
	if err := s.Store.Delete(ctx, prefix+ManifestName); err != nil {
		return err
	}
	s.Log.Info("deleted world", "world", world, "objects", len(keys))
	return s.Store.Delete(ctx, marker)
}
```

- [ ] **Step 4: Run tests** — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worldsync/
git commit -m "feat(worldsync): deletion marker and a sweeper that waits for the lease"
```

---

### Task 8: The node agent core

**Files:**
- Create: `internal/worldsync/state.go`, `internal/worldsync/node.go`, `internal/worldsync/metrics.go`
- Test: `internal/worldsync/node_test.go`

**Interfaces:**
- Consumes: Tasks 1 to 7.
- Produces:
  ```go
  type Mounter interface { Bind(source, target string) error; Unmount(target string) error }
  type Config struct {
      Root, NodeID, Base string; Store Store; Mounter Mounter
      StaleAfter, RenewEvery, PollEvery, EvictAfter time.Duration
      Parallel int; MinFree float64; Clock func() time.Time; Log logr.Logger
  }
  type PublishRequest struct { World string; Keep []string; Target string; Pod string }
  var ErrUnavailable error   // the CSI layer maps it to codes.Unavailable
  func NewNode(cfg Config) (*Node, error)
  func (n *Node) Resume(ctx context.Context) error
  func (n *Node) Run(ctx context.Context)            // blocks; background loops
  func (n *Node) Publish(ctx context.Context, req PublishRequest) error
  func (n *Node) Unpublish(ctx context.Context, target string) error
  func (n *Node) Settle(ctx context.Context)          // tests: run one pass of every loop
  // control files, relative to the world's data dir:
  const ReadyFile = ".spawnery-worldsync/ready", FailedFile = ".spawnery-worldsync/failed",
        RequestFile = ".spawnery-worldsync/snapshot.request", DoneFile = ".spawnery-worldsync/snapshot.done"
  ```

Local layout under `Root`: `worlds/<ns>/<group>/<key>/data/` (the bind-mount source), `.../state.json`, `.../snapshots/<seq>/`; `orphans/<ns>_<group>_<key>_<unix>/`.

- [ ] **Step 1: Write the failing tests**

`internal/worldsync/node_test.go` covers each publish branch, snapshot requests, unpublish, upload retry, lease loss, and Review Focus 2 and 4. The fake mounter records binds and symlinks target → source so the test can write through the "mount":

```go
package worldsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

type fakeMounter struct{ binds map[string]string }

func (f *fakeMounter) Bind(source, target string) error {
	f.binds[target] = source
	os.Remove(target)
	return os.Symlink(source, target)
}

func (f *fakeMounter) Unmount(target string) error {
	delete(f.binds, target)
	return os.Remove(target)
}

type harness struct {
	t     *testing.T
	st    *MemStore
	now   time.Time
	nodes map[string]*Node
	mnt   map[string]*fakeMounter
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, now: time.Unix(1_700_000_000, 0), nodes: map[string]*Node{}, mnt: map[string]*fakeMounter{}}
	h.st = NewMemStore(func() time.Time { return h.now })
	return h
}

func (h *harness) node(id string) *Node {
	if n, ok := h.nodes[id]; ok {
		return n
	}
	m := &fakeMounter{binds: map[string]string{}}
	n, err := NewNode(Config{
		Root: filepath.Join(h.t.TempDir(), id), NodeID: id, Store: h.st, Mounter: m,
		StaleAfter: StaleAfter, RenewEvery: 30 * time.Second, PollEvery: time.Millisecond,
		EvictAfter: 24 * time.Hour, Parallel: 4, MinFree: 0,
		Clock: func() time.Time { return h.now }, Log: logr.Discard(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.nodes[id], h.mnt[id] = n, m
	return n
}

func (h *harness) publish(id, world, pod string) (string, error) {
	target := filepath.Join(h.t.TempDir(), "mount")
	return target, h.node(id).Publish(context.Background(), PublishRequest{World: world, Keep: []string{"worlds/world"}, Target: target, Pod: pod})
}

func (h *harness) waitFile(target, rel string) string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range h.nodes {
			n.Settle(context.Background())
		}
		if b, err := os.ReadFile(filepath.Join(target, rel)); err == nil {
			return string(b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("%s never appeared under %s", rel, target)
	return ""
}

const w = "ns/g/k"

func TestANewWorldIsReadyAtOnce(t *testing.T) {
	h := newHarness(t)
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ReadyFile)); err != nil {
		t.Fatalf("no ready file for a world without manifest: %v", err)
	}
}

func TestAStoppedWorldResumesOnAnotherNodeWithItsContent(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	if err := h.node("a").Unpublish(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	h.node("a").Settle(context.Background()) // final upload and release

	target2, err := h.publish("b", w, "p2")
	if err != nil {
		t.Fatalf("publish on b after a released: %v", err)
	}
	h.waitFile(target2, ReadyFile)
	if _, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat")); err != nil {
		t.Fatalf("the world did not travel: %v", err)
	}
}

func TestAWorldHeldByAnotherNodeIsUnavailable(t *testing.T) {
	h := newHarness(t)
	h.publish("a", w, "p1")
	if _, err := h.publish("b", w, "p2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestACurrentCacheNeedsNoDownload(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.node("a").Unpublish(context.Background(), target)
	h.node("a").Settle(context.Background())

	gets := downloadsCounted(h.node("a"))
	target2, _ := h.publish("a", w, "p2")
	if _, err := os.Stat(filepath.Join(target2, ReadyFile)); err != nil {
		t.Fatalf("not ready at once on the node that holds the current copy: %v", err)
	}
	if downloadsCounted(h.node("a")) != gets {
		t.Fatal("a download ran for a current cache")
	}
}

func TestASnapshotRequestIsAnsweredAndUploaded(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	os.WriteFile(filepath.Join(target, RequestFile), []byte("3"), 0o644)
	if got := h.waitFile(target, DoneFile); strings.TrimSpace(got) != "3" {
		t.Fatalf("done = %q, want 3", got)
	}
	h.node("a").Settle(context.Background())
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || len(m.Files) != 1 {
		t.Fatalf("manifest after the snapshot: %+v, %v", m, err)
	}
}

func TestAStopDuringAnOutageUploadsLater(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.st.SetOutage(errors.New("down"))
	if err := h.node("a").Unpublish(context.Background(), target); err != nil {
		t.Fatalf("unpublish must not wait for the store: %v", err)
	}
	h.node("a").Settle(context.Background())
	h.st.SetOutage(nil)
	h.now = h.now.Add(2 * time.Minute) // past the upload back-off
	h.node("a").Settle(context.Background())
	if exists, _ := WorldExists(context.Background(), h.st, "", w); !exists {
		t.Fatal("the world was not uploaded after the outage ended")
	}
}

func TestALostLeaseOrphansTheLocalCopy(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	h.now = h.now.Add(StaleAfter + time.Minute) // a stopped renewing
	if _, err := h.publish("b", w, "p2"); err != nil {
		t.Fatalf("takeover by b: %v", err)
	}
	h.node("a").Settle(context.Background()) // a's renewal hits 412
	entries, _ := os.ReadDir(filepath.Join(h.node("a").cfg.Root, "orphans"))
	if len(entries) != 1 {
		t.Fatalf("orphans = %d, want 1", len(entries))
	}
	_ = target
}

// Review Focus 2.
func TestAPublishUnderANewTargetUnpublishesTheOldOneFirst(t *testing.T) {
	h := newHarness(t)
	old, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(old, "worlds/world/level.dat"), 5, h.now)
	fresh, err := h.publish("a", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	if _, bound := h.mnt["a"].binds[old]; bound {
		t.Fatal("the old target is still bound")
	}
	if _, err := os.Stat(filepath.Join(fresh, "worlds/world/level.dat")); err != nil {
		t.Fatalf("the world is not under the new target: %v", err)
	}
}

// Review Focus 4.
func TestAReusedKeyDoesNotInheritADeletedWorld(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.node("a").Unpublish(context.Background(), target)
	h.node("a").Settle(context.Background())
	MarkDeleted(context.Background(), h.st, "", w)
	(&Sweeper{Store: h.st, StaleAfter: StaleAfter, Log: logr.Discard()}).SweepOnce(context.Background())

	again, _ := h.publish("a", w, "p2")
	if _, err := os.Stat(filepath.Join(again, "worlds/world/level.dat")); !os.IsNotExist(err) {
		t.Fatal("a deleted world's cache came back under its old key")
	}
}

func TestAPendingDeletionIsUnavailable(t *testing.T) {
	h := newHarness(t)
	MarkDeleted(context.Background(), h.st, "", w)
	if _, err := h.publish("a", w, "p1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestResumeContinuesAPendingUpload(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.st.SetOutage(errors.New("down"))
	h.node("a").Unpublish(context.Background(), target)
	h.st.SetOutage(nil)

	restarted, err := NewNode(h.node("a").cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.Settle(context.Background())
	if exists, _ := WorldExists(context.Background(), h.st, "", w); !exists {
		t.Fatal("the restarted node agent did not finish the upload")
	}
}
```

`downloadsCounted(n *Node) int` is a test helper reading `n.downloads` (an `atomic.Int64` the node increments per download); add it to the test file:

```go
func downloadsCounted(n *Node) int { return int(n.downloads.Load()) }
```

- [ ] **Step 2: Run to verify failure** — `nix develop -c go test ./internal/worldsync/ -run 'World|Snapshot|Outage|Lease|Publish|Deletion|Resume|Cache' -count=1` → FAIL (undefined `NewNode`).

- [ ] **Step 3: Implement state**

`internal/worldsync/state.go`:

```go
package worldsync

import (
	"os"
	"path/filepath"
	"time"
)

// worldState is one world on this node, persisted as state.json beside its
// data directory.
type worldState struct {
	World        string    `json:"world"`
	Keep         []string  `json:"keep"`
	WorldID      string    `json:"worldId"`
	Generation   int64     `json:"generation"`
	ManifestETag string    `json:"manifestETag"`
	Files        []FileEntry `json:"files"` // the uploaded generation's entries
	LeaseETag    string    `json:"leaseETag"`
	Target       string    `json:"target"`
	Pod          string    `json:"pod"`
	Pending      []int64   `json:"pending"`
	NextSeq      int64     `json:"nextSeq"`
	LastRequest  int64     `json:"lastRequest"`
	LastUsed     time.Time `json:"lastUsed"`
}

func (n *Node) worldDir(world string) string { return filepath.Join(n.cfg.Root, "worlds", filepath.FromSlash(world)) }
func (n *Node) dataDir(world string) string  { return filepath.Join(n.worldDir(world), "data") }
func (n *Node) snapDir(world string, seq int64) string {
	return filepath.Join(n.worldDir(world), "snapshots", strconvI(seq))
}

func (n *Node) saveState(s *worldState) error {
	return writeJSON(filepath.Join(n.worldDir(s.World), "state.json"), s)
}

func (n *Node) loadStates() ([]*worldState, error) {
	var out []*worldState
	root := filepath.Join(n.cfg.Root, "worlds")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.Name() != "state.json" {
			return nil
		}
		var s worldState
		if err := readJSON(p, &s); err != nil {
			return err
		}
		out = append(out, &s)
		return nil
	})
	return out, err
}
```

(`strconvI` is `strconv.FormatInt(v, 10)`; define it in `node.go`.)

- [ ] **Step 4: Implement the node**

`internal/worldsync/node.go`. The structure below is the contract; keep each method short and put a world's work under its own mutex (`n.lock(world)`), never the whole node's, so one slow upload does not stall another world's publish.

```go
package worldsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

	"github.com/spawnery/spawnery/internal/prune"
)

const (
	ReadyFile   = prune.ControlDir + "/ready"
	FailedFile  = prune.ControlDir + "/failed"
	RequestFile = prune.ControlDir + "/snapshot.request"
	DoneFile    = prune.ControlDir + "/snapshot.done"
)

var ErrUnavailable = errors.New("worldsync: the world cannot be published on this node now")

type Mounter interface {
	Bind(source, target string) error
	Unmount(target string) error
}

type Config struct {
	Root, NodeID, Base string
	Store              Store
	Mounter            Mounter
	StaleAfter         time.Duration
	RenewEvery         time.Duration
	PollEvery          time.Duration
	EvictAfter         time.Duration
	Parallel           int
	MinFree            float64
	Clock              func() time.Time
	Log                logr.Logger
}

type PublishRequest struct {
	World  string
	Keep   []string
	Target string
	Pod    string
}

type Node struct {
	cfg       Config
	mu        sync.Mutex // guards worlds and locks
	worlds    map[string]*worldState
	locks     map[string]*sync.Mutex
	downloads atomic.Int64
	backoff   map[string]time.Time // world → earliest next upload attempt
}

func strconvI(v int64) string { return strconv.FormatInt(v, 10) }

func NewNode(cfg Config) (*Node, error) {
	if cfg.Root == "" || cfg.NodeID == "" || cfg.Store == nil || cfg.Mounter == nil {
		return nil, errors.New("worldsync: Root, NodeID, Store and Mounter are required")
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root, "worlds"), 0o755); err != nil {
		return nil, err
	}
	return &Node{cfg: cfg, worlds: map[string]*worldState{}, locks: map[string]*sync.Mutex{}, backoff: map[string]time.Time{}}, nil
}

func (n *Node) lock(world string) func() {
	n.mu.Lock()
	l, ok := n.locks[world]
	if !ok {
		l = &sync.Mutex{}
		n.locks[world] = l
	}
	n.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (n *Node) state(world string) *worldState {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, ok := n.worlds[world]
	if !ok {
		s = &worldState{World: world}
		n.worlds[world] = s
	}
	return s
}

func (n *Node) Resume(ctx context.Context) error {
	states, err := n.loadStates()
	if err != nil {
		return err
	}
	n.mu.Lock()
	for _, s := range states {
		n.worlds[s.World] = s
	}
	n.mu.Unlock()
	return nil
}

func (n *Node) lease(s *worldState) Lease {
	return Lease{Node: n.cfg.NodeID, Pod: s.Pod, RenewedAt: n.cfg.Clock()}
}

func (n *Node) control(s *worldState, rel, content string) error {
	return writeFileAtomic(filepath.Join(n.dataDir(s.World), rel), content)
}

func writeFileAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// wipeKept deletes everything under the data directory that keep holds.
func (n *Node) wipeKept(s *worldState) error {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return err
	}
	files, err := Scan(n.dataDir(s.World), keep)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, f := range files {
		if err := os.Remove(filepath.Join(n.dataDir(s.World), f.Path)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (n *Node) Publish(ctx context.Context, req PublishRequest) error {
	unlock := n.lock(req.World)
	defer unlock()
	s := n.state(req.World)

	if s.Target == req.Target {
		return nil
	}
	if s.Target != "" {
		// Review Focus 2: the old pod's teardown has not reached us yet.
		if err := n.unpublishLocked(ctx, s); err != nil {
			return err
		}
	}

	prefix := WorldPrefix(n.cfg.Base, req.World)
	if pending, err := DeletionPending(ctx, n.cfg.Store, n.cfg.Base, req.World); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	} else if pending {
		return fmt.Errorf("%w: the world is being deleted", ErrUnavailable)
	}

	s.Keep, s.Pod = req.Keep, req.Pod
	etag, err := TakeLease(ctx, n.cfg.Store, prefix, n.lease(s), n.cfg.StaleAfter)
	var held *HeldError
	if errors.As(err, &held) {
		leaseConflicts.Inc()
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err != nil {
		return fmt.Errorf("%w: take the lease: %v", ErrUnavailable, err)
	}
	s.LeaseETag = etag

	m, metag, err := ReadManifest(ctx, n.cfg.Store, prefix)
	switch {
	case errors.Is(err, ErrNotFound):
		if s.WorldID != "" {
			// Review Focus 4: a cache of a world that no longer exists.
			if err := n.wipeKept(s); err != nil {
				return err
			}
			s.WorldID, s.Generation, s.ManifestETag, s.Files, s.Pending = "", 0, "", nil, nil
		}
		if err := n.resetControl(s); err != nil {
			return err
		}
		if err := n.control(s, ReadyFile, ""); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("%w: read the manifest: %v", ErrUnavailable, err)
	case m.WorldID == s.WorldID && m.Generation == s.Generation:
		if err := n.resetControl(s); err != nil {
			return err
		}
		if err := n.control(s, ReadyFile, ""); err != nil {
			return err
		}
	default:
		if len(s.Pending) > 0 {
			n.orphan(s, "the manifest moved past snapshots this node still had to upload")
		}
		if err := n.wipeKept(s); err != nil {
			return err
		}
		if err := n.resetControl(s); err != nil {
			return err
		}
		n.startDownload(s, m, metag)
	}

	if err := os.MkdirAll(n.dataDir(req.World), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(req.Target, 0o755); err != nil {
		return err
	}
	if err := n.cfg.Mounter.Bind(n.dataDir(req.World), req.Target); err != nil {
		return err
	}
	s.Target, s.LastUsed = req.Target, n.cfg.Clock()
	return n.saveState(s)
}

func (n *Node) resetControl(s *worldState) error {
	dir := filepath.Join(n.dataDir(s.World), prune.ControlDir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	s.LastRequest = 0
	return os.MkdirAll(dir, 0o755)
}

// startDownload runs in the background; the container starts meanwhile and
// the agent waits for ready or failed.
func (n *Node) startDownload(s *worldState, m Manifest, etag string) {
	world := s.World
	n.downloads.Add(1)
	go func() {
		started := n.cfg.Clock()
		err := Download(context.Background(), n.cfg.Store, WorldPrefix(n.cfg.Base, world), n.dataDir(world), m, n.cfg.Parallel)
		unlock := n.lock(world)
		defer unlock()
		if err != nil {
			n.cfg.Log.Error(err, "download failed", "world", world)
			_ = n.control(s, FailedFile, err.Error())
			return
		}
		downloadSeconds.Observe(n.cfg.Clock().Sub(started).Seconds())
		s.WorldID, s.Generation, s.ManifestETag, s.Files = m.WorldID, m.Generation, etag, m.Files
		_ = n.saveState(s)
		_ = n.control(s, ReadyFile, "")
	}()
}

func (n *Node) Unpublish(ctx context.Context, target string) error {
	n.mu.Lock()
	var world string
	for w, s := range n.worlds {
		if s.Target == target {
			world = w
		}
	}
	n.mu.Unlock()
	if world == "" {
		_ = n.cfg.Mounter.Unmount(target)
		return nil
	}
	unlock := n.lock(world)
	defer unlock()
	return n.unpublishLocked(ctx, n.state(world))
}

// unpublishLocked unbinds, takes the final snapshot and queues its upload.
func (n *Node) unpublishLocked(ctx context.Context, s *worldState) error {
	if err := n.cfg.Mounter.Unmount(s.Target); err != nil && !errors.Is(err, unix.EINVAL) && !os.IsNotExist(err) {
		return err
	}
	s.Target = ""
	if s.LeaseETag != "" {
		if err := n.snapshot(s); err != nil {
			n.cfg.Log.Error(err, "final snapshot failed", "world", s.World)
		}
	}
	s.LastUsed = n.cfg.Clock()
	return n.saveState(s)
}

// snapshot copies the world into snapshots/<seq> and queues it.
func (n *Node) snapshot(s *worldState) error {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return err
	}
	s.NextSeq++
	seq := s.NextSeq
	base := s.Files
	if len(s.Pending) > 0 {
		// Compare against the newest queued snapshot, not the uploaded one,
		// so a file changed twice is copied for each change.
		if last, err := ReadSnap(n.snapDir(s.World, s.Pending[len(s.Pending)-1])); err == nil {
			base = base[:0:0]
			for _, f := range last.Files {
				base = append(base, f.FileEntry)
			}
		}
	}
	dir := n.snapDir(s.World, seq)
	_ = os.RemoveAll(dir)
	if _, err := TakeSnapshot(n.dataDir(s.World), dir, keep, base, seq); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	s.Pending = append(s.Pending, seq)
	return nil
}
```

Note on `base` for queued snapshots: entries copied in an earlier, not yet uploaded snapshot have an empty `Object`. `TakeSnapshot` then treats an unchanged large file as unchanged and carries an entry without `Object`. Handle this in `UploadSnapshot`'s caller: before uploading snapshot N, rewrite its not-copied entries whose `Object` is empty from the manifest that the upload of N−1 produced (same path, same size and mtime). Implement this as `fillReferences(snap *Snap, uploaded []FileEntry)` in `node.go`, called by `uploadOne`, and write `ReadSnap`→fill→`writeJSON` before `UploadSnapshot`. Add this test to `node_test.go`:

```go
func TestTwoQueuedSnapshotsUploadInOrderWithReferencesFilled(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/region/a.mca"), PackBelow+1, h.now)
	h.st.SetOutage(errors.New("down"))
	os.WriteFile(filepath.Join(target, RequestFile), []byte("1"), 0o644)
	h.waitFile(target, DoneFile)
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	os.WriteFile(filepath.Join(target, RequestFile), []byte("2"), 0o644)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.node("a").Settle(context.Background())
		if b, _ := os.ReadFile(filepath.Join(target, DoneFile)); strings.TrimSpace(string(b)) == "2" {
			break
		}
	}
	h.st.SetOutage(nil)
	h.now = h.now.Add(2 * time.Minute) // past the upload back-off
	h.node("a").Settle(context.Background())
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || m.Generation != 2 || len(m.Files) != 2 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	for _, f := range m.Files {
		if f.Object == "" {
			t.Fatalf("%s has no object", f.Path)
		}
	}
}
```

The remaining loops, still in `node.go`:

```go
func (n *Node) Run(ctx context.Context) {
	poll := time.NewTicker(n.cfg.PollEvery)
	renew := time.NewTicker(n.cfg.RenewEvery)
	evict := time.NewTicker(time.Minute)
	defer poll.Stop()
	defer renew.Stop()
	defer evict.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			n.pollRequests()
			n.uploadAll(ctx)
		case <-renew.C:
			n.renewAll(ctx)
		case <-evict.C:
			n.evict()
		}
	}
}

// Settle runs one pass of every loop, for tests.
func (n *Node) Settle(ctx context.Context) {
	n.pollRequests()
	n.renewAll(ctx)
	n.uploadAll(ctx)
	n.evict()
}

func (n *Node) each(fn func(*worldState)) {
	n.mu.Lock()
	var worlds []string
	for w := range n.worlds {
		worlds = append(worlds, w)
	}
	n.mu.Unlock()
	for _, w := range worlds {
		unlock := n.lock(w)
		if s := n.state(w); s != nil {
			fn(s)
		}
		unlock()
	}
}

func (n *Node) pollRequests() {
	n.each(func(s *worldState) {
		if s.Target == "" {
			return
		}
		b, err := os.ReadFile(filepath.Join(n.dataDir(s.World), RequestFile))
		if err != nil {
			return
		}
		seq, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || seq <= s.LastRequest {
			return
		}
		s.LastRequest = seq
		answer := strconvI(seq)
		if err := n.snapshot(s); err != nil {
			answer = fmt.Sprintf("failed %d %v", seq, err)
		}
		_ = n.saveState(s)
		_ = n.control(s, DoneFile, answer)
	})
}

func (n *Node) uploadAll(ctx context.Context) {
	n.each(func(s *worldState) {
		if until, ok := n.backoff[s.World]; ok && n.cfg.Clock().Before(until) {
			return
		}
		for len(s.Pending) > 0 {
			if err := n.uploadOne(ctx, s); err != nil {
				if errors.Is(err, ErrConflict) {
					n.orphan(s, err.Error())
					return
				}
				n.cfg.Log.Error(err, "upload failed; retrying", "world", s.World)
				n.backoff[s.World] = n.cfg.Clock().Add(n.nextBackoff(s.World))
				return
			}
			delete(n.backoff, s.World)
		}
		pendingUploads.Set(float64(n.countPending()))
		if s.Target == "" && s.LeaseETag != "" {
			n.release(ctx, s)
		}
	})
}

func (n *Node) uploadOne(ctx context.Context, s *worldState) error {
	seq := s.Pending[0]
	dir := n.snapDir(s.World, seq)
	snap, err := ReadSnap(dir)
	if err != nil {
		return err
	}
	fillReferences(&snap, s.Files)
	if err := writeJSON(filepath.Join(dir, snapFile), snap); err != nil {
		return err
	}
	var prev *Manifest
	if s.WorldID != "" {
		prev = &Manifest{WorldID: s.WorldID, Generation: s.Generation, Files: s.Files}
	}
	m, etag, err := UploadSnapshot(ctx, n.cfg.Store, WorldPrefix(n.cfg.Base, s.World), dir, prev, s.ManifestETag, NewWorldID)
	if err != nil {
		return err
	}
	s.WorldID, s.Generation, s.ManifestETag, s.Files = m.WorldID, m.Generation, etag, m.Files
	s.Pending = s.Pending[1:]
	_ = os.RemoveAll(dir)
	return n.saveState(s)
}

func fillReferences(snap *Snap, uploaded []FileEntry) {
	byPath := map[string]FileEntry{}
	for _, f := range uploaded {
		byPath[f.Path] = f
	}
	for i, f := range snap.Files {
		if f.Copied || f.Object != "" {
			continue
		}
		if u, ok := byPath[f.Path]; ok && u.Size == f.Size && u.MTime == f.MTime {
			snap.Files[i].Object = u.Object
		}
	}
}

// release drops scratch, then the lease, once nothing is left to upload.
func (n *Node) release(ctx context.Context, s *worldState) {
	n.dropScratch(s)
	err := ReleaseLease(ctx, n.cfg.Store, WorldPrefix(n.cfg.Base, s.World), s.LeaseETag)
	if err != nil && !errors.Is(err, ErrLeaseLost) {
		n.cfg.Log.Error(err, "release failed; retrying", "world", s.World)
		return
	}
	s.LeaseETag = ""
	_ = n.saveState(s)
}

// dropScratch deletes what keep does not hold; the next start's prune would.
func (n *Node) dropScratch(s *worldState) {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(n.dataDir(s.World))
	for _, e := range entries {
		if e.Name() == prune.ControlDir || keep.Holds(e.Name()) || keep.Toward(e.Name()) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(n.dataDir(s.World), e.Name()))
	}
}

func (n *Node) renewAll(ctx context.Context) {
	n.each(func(s *worldState) {
		if s.LeaseETag == "" {
			return
		}
		etag, err := RenewLease(ctx, n.cfg.Store, WorldPrefix(n.cfg.Base, s.World), n.lease(s), s.LeaseETag)
		if errors.Is(err, ErrLeaseLost) {
			n.orphan(s, "another node took the lease")
			return
		}
		if err != nil {
			n.cfg.Log.Error(err, "lease renewal failed; retrying", "world", s.World)
			return
		}
		s.LeaseETag = etag
		_ = n.saveState(s)
	})
}

// orphan moves the world's directory aside for manual recovery and forgets it.
func (n *Node) orphan(s *worldState, why string) {
	orphans.Inc()
	dst := filepath.Join(n.cfg.Root, "orphans",
		strings.ReplaceAll(s.World, "/", "_")+"_"+strconvI(n.cfg.Clock().Unix()))
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.Rename(n.worldDir(s.World), dst); err != nil {
		n.cfg.Log.Error(err, "could not move an orphaned world aside", "world", s.World)
	}
	n.cfg.Log.Info("orphaned the local copy", "world", s.World, "reason", why, "path", dst)
	target := s.Target
	*s = worldState{World: s.World, Target: target}
}

func (n *Node) evict() {
	n.each(func(s *worldState) {
		if s.Target != "" || len(s.Pending) > 0 || s.LeaseETag != "" {
			return
		}
		if n.cfg.Clock().Sub(s.LastUsed) < n.cfg.EvictAfter && n.freeFraction() >= n.cfg.MinFree {
			return
		}
		_ = os.RemoveAll(n.worldDir(s.World))
		n.mu.Lock()
		delete(n.worlds, s.World)
		n.mu.Unlock()
	})
}

func (n *Node) freeFraction() float64 {
	var st unix.Statfs_t
	if err := unix.Statfs(n.cfg.Root, &st); err != nil || st.Blocks == 0 {
		return 1
	}
	return float64(st.Bavail) / float64(st.Blocks)
}

func (n *Node) countPending() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, s := range n.worlds {
		c += len(s.Pending)
	}
	return c
}

func (n *Node) nextBackoff(world string) time.Duration {
	// 1 s doubling to 1 min, keyed off how far the last deadline was.
	last, ok := n.backoff[world]
	if !ok {
		return time.Second
	}
	d := 2 * last.Sub(n.cfg.Clock())
	if d < time.Second {
		d = time.Second
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d
}
```

Two corrections the tests will force and that are part of this step:
- `orphan` keeps the world in `n.worlds` with only `Target`; when `Target` is empty, delete it from `n.worlds` instead (an orphaned, unpublished world must not be retried).
- `each` must skip worlds removed by `evict`/`orphan` in the same pass (`n.state` would recreate them); look the world up without creating it inside `each`.

`internal/worldsync/metrics.go`:

```go
package worldsync

import "github.com/prometheus/client_golang/prometheus"

var (
	downloadSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "spawnery_worldsync_download_seconds", Help: "Time to download a world at publish.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 40, 80},
	})
	pendingUploads = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_worldsync_pending_snapshots", Help: "Snapshots on this node not yet in the bucket.",
	})
	leaseConflicts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_worldsync_lease_conflicts_total", Help: "Publishes refused because another node held the world.",
	})
	orphans = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_worldsync_orphans_total", Help: "Local copies moved aside after the lease was lost.",
	})
)

// Metrics are registered by the binary, not here, so tests need no registry.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{downloadSeconds, pendingUploads, leaseConflicts, orphans}
}
```

- [ ] **Step 5: Run tests with the race detector**

Run: `nix develop -c go test -race ./internal/worldsync/ -count=1`
Expected: PASS. Fix every race report; the download goroutine and the loops must only touch a world's state under its lock.

- [ ] **Step 6: Commit**

```bash
git add internal/worldsync/
git commit -m "feat(worldsync): node agent with publish, snapshots, uploads and leases"
```

---

### Task 9: CSI services, the bind mounter and the binary

**Files:**
- Create: `internal/worldsync/csi.go`, `internal/worldsync/mount_linux.go`, `cmd/spawnery-worldsync/main.go`
- Test: `internal/worldsync/csi_test.go`, `cmd/spawnery-worldsync/main_test.go`
- Modify: `go.mod`, `go.sum`, `flake.nix` (vendorHash)

**Interfaces:**
- Consumes: `Node`, `PublishRequest`, `ErrUnavailable`, `S3ConfigFromEnv`, `NewS3Store`, `TakeSnapshot`, `UploadSnapshot` (earlier tasks).
- Produces:
  ```go
  const DriverName = "worldsync.spawnery.cloud"
  func NewCSIServer(node *Node, nodeID, version string) *CSIServer   // implements csi.IdentityServer, csi.NodeServer
  type BindMounter struct{}                                         // Mounter over unix.Mount
  func Import(ctx context.Context, st Store, base, world string, keep []string, dir string) (Manifest, error)
  ```
  Volume attributes: `world` (`<namespace>/<group>/<key>`), `keep` (entries joined by `\n`).

- [ ] **Step 1: Add the CSI bindings**

```bash
nix develop -c go get github.com/container-storage-interface/spec@v1.12.0
```

- [ ] **Step 2: Write the failing tests**

`internal/worldsync/csi_test.go`:

```go
package worldsync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func csiFor(t *testing.T) (*CSIServer, *harness) {
	h := newHarness(t)
	return NewCSIServer(h.node("a"), "a", "test"), h
}

func publishReq(t *testing.T, ns, world string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:   "csi-123",
		TargetPath: filepath.Join(t.TempDir(), "mount"),
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/ephemeral":     "true",
			"csi.storage.k8s.io/pod.namespace": ns,
			"csi.storage.k8s.io/pod.name":      "g-k",
			"world":                            world,
			"keep":                             "worlds/world",
		},
	}
}

// Review Focus 1.
func TestAPodCannotMountAWorldOfAnotherNamespace(t *testing.T) {
	s, _ := csiFor(t)
	_, err := s.NodePublishVolume(context.Background(), publishReq(t, "attacker", "victim/g/k"))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestOnlyEphemeralVolumesArePublished(t *testing.T) {
	s, _ := csiFor(t)
	req := publishReq(t, "ns", "ns/g/k")
	delete(req.VolumeContext, "csi.storage.k8s.io/ephemeral")
	if _, err := s.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAHeldWorldIsUnavailableOverCSI(t *testing.T) {
	s, h := csiFor(t)
	if _, err := h.publish("b", "ns/g/k", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	_, err := s.NodePublishVolume(context.Background(), publishReq(t, "ns", "ns/g/k"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable so the kubelet retries", status.Code(err))
	}
	_ = time.Second
}

func TestUnpublishOfAnUnknownTargetSucceeds(t *testing.T) {
	s, _ := csiFor(t)
	if _, err := s.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: "x", TargetPath: filepath.Join(t.TempDir(), "gone")}); err != nil {
		t.Fatalf("idempotent unpublish: %v", err)
	}
}

func TestPluginInfoNamesTheDriver(t *testing.T) {
	s, _ := csiFor(t)
	info, err := s.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil || info.GetName() != DriverName {
		t.Fatalf("info = %v, %v", info, err)
	}
}

func TestImportRefusesAnExistingWorld(t *testing.T) {
	st := NewMemStore(time.Now)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	if _, err := Import(context.Background(), st, "", "ns/g/k", []string{"worlds/world"}, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), st, "", "ns/g/k", []string{"worlds/world"}, dir); err == nil {
		t.Fatal("a second import over an existing world succeeded")
	}
}
```

`cmd/spawnery-worldsync/main_test.go` checks the usage errors:

```go
package main

import (
	"bytes"
	"testing"
)

func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"nonsense"}, func(string) string { return "" }, &stderr); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}

func TestImportNeedsTheStore(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"import", "--world", "ns/g/k", "--dir", t.TempDir(), "--keep", "worlds/world"}, func(string) string { return "" }, &stderr)
	if code != 1 || !bytes.Contains(stderr.Bytes(), []byte("WORLDSYNC_ENDPOINT")) {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}
```

- [ ] **Step 3: Run to verify failure** — FAIL (undefined `NewCSIServer`, `Import`, `run`).

- [ ] **Step 4: Implement**

`internal/worldsync/csi.go`:

```go
package worldsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	DriverName       = "worldsync.spawnery.cloud"
	ctxEphemeral     = "csi.storage.k8s.io/ephemeral"
	ctxPodNamespace  = "csi.storage.k8s.io/pod.namespace"
	ctxPodName       = "csi.storage.k8s.io/pod.name"
	AttrWorld        = "world"
	AttrKeep         = "keep"
)

type CSIServer struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer
	node    *Node
	nodeID  string
	version string
}

func NewCSIServer(node *Node, nodeID, version string) *CSIServer {
	return &CSIServer{node: node, nodeID: nodeID, version: version}
}

func (s *CSIServer) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: s.version}, nil
}

func (s *CSIServer) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (s *CSIServer) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func (s *CSIServer) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: s.nodeID}, nil
}

func (s *CSIServer) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (s *CSIServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	vc := req.GetVolumeContext()
	if vc[ctxEphemeral] != "true" {
		return nil, status.Error(codes.InvalidArgument, "only inline ephemeral volumes are served")
	}
	world, ns := vc[AttrWorld], vc[ctxPodNamespace]
	parts := strings.Split(world, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, status.Errorf(codes.InvalidArgument, "world %q is not <namespace>/<group>/<key>", world)
	}
	if parts[0] != ns {
		return nil, status.Errorf(codes.PermissionDenied, "a pod in namespace %q asked for world %q", ns, world)
	}
	keep := strings.Split(vc[AttrKeep], "\n")
	if vc[AttrKeep] == "" {
		return nil, status.Error(codes.InvalidArgument, "the keep attribute is empty")
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "no target path")
	}
	err := s.node.Publish(ctx, PublishRequest{World: world, Keep: keep, Target: req.GetTargetPath(), Pod: ns + "/" + vc[ctxPodName]})
	switch {
	case errors.Is(err, ErrUnavailable):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *CSIServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if err := s.node.Unpublish(ctx, req.GetTargetPath()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = os.Remove(filepath.Clean(req.GetTargetPath()))
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// Import uploads dir's kept paths as generation 1 of a world that has none.
func Import(ctx context.Context, st Store, base, world string, keep []string, dir string) (Manifest, error) {
	k, err := prune.ParseKeep(keep)
	if err != nil {
		return Manifest{}, err
	}
	snap, err := os.MkdirTemp("", "worldsync-import-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(snap)
	if _, err := TakeSnapshot(dir, snap, k, nil, 1); err != nil {
		return Manifest{}, err
	}
	m, _, err := UploadSnapshot(ctx, st, WorldPrefix(base, world), snap, nil, "", NewWorldID)
	return m, err
}
```

`csi.go` imports `github.com/spawnery/spawnery/internal/prune` for `ParseKeep`.

`internal/worldsync/mount_linux.go`:

```go
package worldsync

import (
	"errors"

	"golang.org/x/sys/unix"
)

type BindMounter struct{}

func (BindMounter) Bind(source, target string) error {
	return unix.Mount(source, target, "", unix.MS_BIND, "")
}

func (BindMounter) Unmount(target string) error {
	err := unix.Unmount(target, 0)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil // not mounted
	}
	return err
}
```

`cmd/spawnery-worldsync/main.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/spawnery/spawnery/internal/version"
	"github.com/spawnery/spawnery/internal/worldsync"
)

func main() { os.Exit(run(os.Args[1:], os.Getenv, os.Stderr)) }

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: spawnery-worldsync node|import [flags]")
		return 2
	}
	switch args[0] {
	case "node":
		return runNode(args[1:], getenv, stderr)
	case "import":
		return runImport(args[1:], getenv, stderr)
	default:
		fmt.Fprintf(stderr, "spawnery-worldsync: unknown subcommand %q\n", args[0])
		return 2
	}
}

func store(getenv func(string) string, stderr io.Writer) (worldsync.Store, string, bool) {
	cfg, base, err := worldsync.S3ConfigFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-worldsync: %v\n", err)
		return nil, "", false
	}
	st, err := worldsync.NewS3Store(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-worldsync: %v\n", err)
		return nil, "", false
	}
	return st, base, true
}

func runImport(args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	world := fs.String("world", "", "<namespace>/<group>/<key>")
	dir := fs.String("dir", "", "the directory holding /data's content")
	keep := fs.String("keep", "", "spec.storage.keep, one entry per line")
	if err := fs.Parse(args); err != nil || *world == "" || *dir == "" || *keep == "" {
		fmt.Fprintln(stderr, "spawnery-worldsync import: --world, --dir and --keep are required")
		return 2
	}
	st, base, ok := store(getenv, stderr)
	if !ok {
		return 1
	}
	m, err := worldsync.Import(context.Background(), st, base, *world, strings.Split(*keep, "\n"), *dir)
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-worldsync import: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "imported %s: %d files, world %s\n", *world, len(m.Files), m.WorldID)
	return 0
}

func runNode(args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	nodeID := fs.String("node-id", getenv("NODE_NAME"), "this node's name")
	root := fs.String("root", "/var/lib/spawnery/worldsync", "the node directory for worlds")
	metrics := fs.String("metrics-bind-address", ":8090", "metrics endpoint")
	opts := zap.Options{}
	opts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(log)
	st, base, ok := store(getenv, stderr)
	if !ok {
		return 1
	}
	node, err := worldsync.NewNode(worldsync.Config{
		Root: *root, NodeID: *nodeID, Base: base, Store: st, Mounter: worldsync.BindMounter{},
		StaleAfter: worldsync.StaleAfter, RenewEvery: 30 * time.Second, PollEvery: 500 * time.Millisecond,
		EvictAfter: 24 * time.Hour, Parallel: 32, MinFree: 0.15, Clock: time.Now, Log: log,
	})
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-worldsync: %v\n", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := node.Resume(ctx); err != nil {
		fmt.Fprintf(stderr, "spawnery-worldsync: resume: %v\n", err)
		return 1
	}
	go node.Run(ctx)

	reg := prometheus.NewRegistry()
	reg.MustRegister(worldsync.Collectors()...)
	go func() { _ = http.ListenAndServe(*metrics, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})) }()

	path := strings.TrimPrefix(*endpoint, "unix://")
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintf(stderr, "spawnery-worldsync: listen %s: %v\n", path, err)
		return 1
	}
	srv := grpc.NewServer()
	cs := worldsync.NewCSIServer(node, *nodeID, version.Version)
	csi.RegisterIdentityServer(srv, cs)
	csi.RegisterNodeServer(srv, cs)
	go func() { <-ctx.Done(); srv.GracefulStop() }()
	if err := srv.Serve(lis); err != nil {
		fmt.Fprintf(stderr, "spawnery-worldsync: serve: %v\n", err)
		return 1
	}
	return 0
}
```

Check `internal/version` for the exported name of the version string and use it; if it has none suitable, pass `"dev"` and set it via `-ldflags` in Task 15.

- [ ] **Step 5: Run tests, fix vendorHash**

Run: `nix develop -c go test -race ./internal/worldsync/ ./cmd/spawnery-worldsync/ -count=1` → PASS. Then the vendorHash step from Task 3.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum flake.nix internal/worldsync/ cmd/spawnery-worldsync/
git commit -m "feat(worldsync): CSI node plugin and import command"
```

---

### Task 10: API field `storage.backend`

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (`StorageSpec`, the `ServerGroupSpec` CEL markers near line 223, helpers near `IsOnDemand` line ~574)
- Generated: `make manifests generate` outputs
- Test: the envtest validation tests in `api/v1alpha1/` (find the file testing `storage.replace` validation, e.g. `grep -ln "replace" api/v1alpha1/*_test.go`, and add beside it)

**Interfaces:**
- Produces:
  ```go
  type StorageBackend string
  const StorageBackendClaim StorageBackend = "Claim"; const StorageBackendObjectStore StorageBackend = "ObjectStore"
  StorageSpec.Backend StorageBackend `json:"backend,omitempty"`
  func (g *ServerGroup) UsesObjectStore() bool
  func (g *ServerGroup) UsesClaim() bool   // !IsEphemeral() && !UsesObjectStore()
  ```

- [ ] **Step 1: Write the failing tests** (in the existing validation test file, following its helper for creating a group and expecting an error message):

```go
func TestObjectStoreBackendNeedsOnDemandAndKeep(t *testing.T) {
	// A Persistent group may not use it.
	g := persistentGroupFixture() // use the file's existing Persistent fixture
	g.Spec.Storage.Backend = StorageBackendObjectStore
	g.Spec.Storage.Keep = []string{"world"}
	expectInvalid(t, g, "storage.backend ObjectStore needs type OnDemand and storage.keep")

	// OnDemand without keep may not either.
	o := onDemandGroupFixture()
	o.Spec.Storage.Backend = StorageBackendObjectStore
	o.Spec.Storage.Keep = nil
	expectInvalid(t, o, "storage.backend ObjectStore needs type OnDemand and storage.keep")

	// OnDemand with keep may.
	o.Spec.Storage.Keep = []string{"world"}
	expectValid(t, o)
}

func TestTheBackendMayChangeBothWays(t *testing.T) {
	o := onDemandGroupFixture()
	o.Spec.Storage.Keep = []string{"world"}
	createValid(t, o)
	o.Spec.Storage.Backend = StorageBackendObjectStore
	updateValid(t, o)
	o.Spec.Storage.Backend = StorageBackendClaim
	updateValid(t, o)
}
```

Map `persistentGroupFixture`, `onDemandGroupFixture`, `expectInvalid`, `expectValid`, `createValid`, `updateValid` onto the helpers the file already has; add thin wrappers if names differ. Also a plain unit test in `api/v1alpha1` (no envtest):

```go
func TestUsesClaim(t *testing.T) {
	g := &ServerGroup{Spec: ServerGroupSpec{Type: ServerGroupOnDemand, Storage: &StorageSpec{}}}
	if !g.UsesClaim() || g.UsesObjectStore() {
		t.Fatal("an OnDemand group without backend uses a claim")
	}
	g.Spec.Storage.Backend = StorageBackendObjectStore
	if g.UsesClaim() || !g.UsesObjectStore() {
		t.Fatal("ObjectStore uses no claim")
	}
	e := &ServerGroup{Spec: ServerGroupSpec{Type: ServerGroupEphemeral}}
	if e.UsesClaim() {
		t.Fatal("an Ephemeral group uses no claim")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `nix develop -c go test ./api/v1alpha1/ -run 'Backend|UsesClaim' -count=1 -p 1` → FAIL.

- [ ] **Step 3: Implement**

In `StorageSpec`, as the first field after `Size`:

```go
	// Backend is where a member's world lives between runs. Claim, the
	// default, keeps it on a PersistentVolumeClaim per member. ObjectStore
	// keeps it in the object store configured for the operator (chart value
	// worldSync) and gives /data from a directory on the node that runs the
	// member; it needs type OnDemand and keep, and synchronises exactly what
	// keep matches. Size, storageClassName, accessModes and annotations are
	// ignored under ObjectStore and stay valid for switching back. The
	// backend may change either way; nothing is moved across.
	// +kubebuilder:validation:Enum=Claim;ObjectStore
	// +optional
	Backend StorageBackend `json:"backend,omitempty"`
```

Above `StorageSpec`:

```go
type StorageBackend string

const (
	StorageBackendClaim       StorageBackend = "Claim"
	StorageBackendObjectStore StorageBackend = "ObjectStore"
)
```

Add to the `ServerGroupSpec` markers:

```go
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(self.storage.backend) || self.storage.backend != 'ObjectStore' || (self.type == 'OnDemand' && has(self.storage.keep))",message="storage.backend ObjectStore needs type OnDemand and storage.keep"
```

Next to `IsOnDemand`:

```go
func (g *ServerGroup) UsesObjectStore() bool {
	return g.Spec.Storage != nil && g.Spec.Storage.Backend == StorageBackendObjectStore
}

// UsesClaim reports whether members get a data claim of their own.
func (g *ServerGroup) UsesClaim() bool {
	return !g.IsEphemeral() && !g.UsesObjectStore()
}
```

Run `nix develop -c make manifests generate`.

- [ ] **Step 4: Run tests** — `nix develop -c go test ./api/v1alpha1/ -count=1 -p 1` → PASS.

- [ ] **Step 5: Commit** (generated files included)

```bash
git add api/ config/ charts/spawnery/templates/crds.yaml docs/reference/
git commit -m "feat(api): storage.backend ObjectStore for OnDemand groups"
```

---

### Task 11: Pod spec for ObjectStore groups

**Files:**
- Create: `internal/podspec/worldsync.go`
- Modify: `internal/podspec/server.go` (`dataVolume` ~line 388; the `Env` list ~line 283)
- Test: `internal/podspec/worldsync_test.go`

**Interfaces:**
- Consumes: `UsesObjectStore` (Task 10), `worldsync.DriverName`, `AttrWorld`, `AttrKeep` (Task 9).
- Produces: `const EnvWorldSync = "SPAWNERY_WORLD_SYNC"`, `const EnvWorldSyncInterval = "SPAWNERY_WORLD_SYNC_INTERVAL"`, `func WithWorldSyncInterval(pod *corev1.Pod, d time.Duration)`.

`internal/podspec` must not import `internal/worldsync` if that drags aws-sdk into every binary that uses podspec (the game image's `spawnery-config` does not use podspec; the operator does and needs the SDK anyway). Duplicate the three string constants in `podspec/worldsync.go` and add a test in `internal/worldsync` that asserts they agree (`podspec.WorldSyncDriver == worldsync.DriverName` etc.), following the source-reading agreement tests the repo already has.

- [ ] **Step 1: Write the failing tests**

```go
package podspec

import (
	"testing"
	"time"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func objectStoreGroup(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
	g.Spec.Type = spawneryv1alpha1.ServerGroupOnDemand
	g.Spec.Scaling = nil
	g.Spec.Storage = &spawneryv1alpha1.StorageSpec{
		Size: resource.MustParse("6Gi"), Backend: spawneryv1alpha1.StorageBackendObjectStore,
		Keep: []string{"worlds/world", "plugins/Example/data"},
	}
}

func TestAnObjectStoreMemberGetsTheCSIVolume(t *testing.T) {
	net, group := testNetwork(), testGroup()
	objectStoreGroup(net, group)
	srv := testServer()
	srv.Spec.Key = "c0ffee"
	pod, err := BuildServerPod(net, group, srv, testEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	v := volumeNamed(pod, DataVolumeName)
	if v == nil || v.CSI == nil {
		t.Fatalf("data volume = %+v, want a CSI inline volume", v)
	}
	if v.CSI.Driver != WorldSyncDriver {
		t.Errorf("driver = %q", v.CSI.Driver)
	}
	if got := v.CSI.VolumeAttributes["world"]; got != "minecraft/lobby/c0ffee" {
		t.Errorf("world = %q", got)
	}
	if got := v.CSI.VolumeAttributes["keep"]; got != "worlds/world\nplugins/Example/data" {
		t.Errorf("keep = %q", got)
	}
	if envValue(pod, EnvWorldSync) != "1" {
		t.Error("SPAWNERY_WORLD_SYNC is not set")
	}
}

func TestAClaimMemberIsUnchanged(t *testing.T) {
	pod := build(t, func(n *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		objectStoreGroup(n, g)
		g.Spec.Storage.Backend = ""
	})
	if v := volumeNamed(pod, DataVolumeName); v == nil || v.PersistentVolumeClaim == nil {
		t.Fatalf("data volume = %+v, want the claim", v)
	}
	if envValue(pod, EnvWorldSync) != "" {
		t.Error("a claim member carries SPAWNERY_WORLD_SYNC")
	}
}

func TestTheIntervalIsAddedAfterTheHash(t *testing.T) {
	pod := build(t, objectStoreGroup)
	WithWorldSyncInterval(pod, 5*time.Minute)
	if got := envValue(pod, EnvWorldSyncInterval); got != "5m0s" {
		t.Fatalf("interval = %q", got)
	}
}
```

Add `envValue(pod, name) string` to the test file if no such helper exists (scan `pod.Spec.Containers[0].Env`). Also: `internal/podspec/env_test.go` checks the reserved `SPAWNERY_` names against CRD markers; extend whatever list it keeps with the two new names if it fails.

- [ ] **Step 2: Run to verify failure** — `nix develop -c go test ./internal/podspec/ -run 'ObjectStore|ClaimMember|Interval' -count=1` → FAIL.

- [ ] **Step 3: Implement**

`internal/podspec/worldsync.go`:

```go
package podspec

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// Copies of internal/worldsync's names; worldsync's agreement test reads
// them from here.
const (
	WorldSyncDriver      = "worldsync.spawnery.cloud"
	EnvWorldSync         = "SPAWNERY_WORLD_SYNC"
	EnvWorldSyncInterval = "SPAWNERY_WORLD_SYNC_INTERVAL"
)

func worldSyncVolume(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) corev1.Volume {
	return corev1.Volume{
		Name: DataVolumeName,
		VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver: WorldSyncDriver,
			VolumeAttributes: map[string]string{
				"world": srv.Namespace + "/" + group.Name + "/" + srv.Spec.Key,
				"keep":  strings.Join(group.Spec.Storage.Keep, "\n"),
			},
		}},
	}
}

func worldSyncEnv(group *spawneryv1alpha1.ServerGroup) []corev1.EnvVar {
	if !group.UsesObjectStore() {
		return nil
	}
	return []corev1.EnvVar{{Name: EnvWorldSync, Value: "1"}}
}

// WithWorldSyncInterval runs after BuildServerPod, like WithAOTCache, so
// changing the operator's interval does not restart every world.
func WithWorldSyncInterval(pod *corev1.Pod, d time.Duration) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		for _, e := range c.Env {
			if e.Name == EnvWorldSync {
				c.Env = append(c.Env, corev1.EnvVar{Name: EnvWorldSyncInterval, Value: d.String()})
				break
			}
		}
	}
}
```

In `server.go`, `dataVolume`:

```go
func dataVolume(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) corev1.Volume {
	if group.UsesObjectStore() {
		return worldSyncVolume(group, srv)
	}
	if keepsWorld(group) {
```

and in the `Env` chain add `worldSyncEnv(group)...` after `replaceEnv(...)`:

```go
		}, substitutionEnv(group.Spec.Substitution)...), keepEnv(group.Spec.Storage)...), replaceEnv(group.Spec.Storage)...), worldSyncEnv(group)...), group.Spec.Env...),
```

(one more `append(` at the front of the chain).

In `internal/worldsync`, add `agreement_test.go`:

```go
package worldsync

import (
	"testing"

	"github.com/spawnery/spawnery/internal/podspec"
)

func TestThePodSpecNamesAgree(t *testing.T) {
	if podspec.WorldSyncDriver != DriverName {
		t.Fatalf("podspec renders driver %q, the node agent registers %q", podspec.WorldSyncDriver, DriverName)
	}
}
```

- [ ] **Step 4: Run tests, including the hash goldens**

Run: `nix develop -c go test ./internal/podspec/ ./internal/worldsync/ -count=1`
Expected: PASS, and `hash_golden_test.go` unchanged (a claim group renders the same pod). If a golden moves, the change touched claim groups: fix the code, not the golden.

- [ ] **Step 5: Commit**

```bash
git add internal/podspec/ internal/worldsync/agreement_test.go
git commit -m "feat(podspec): CSI data volume and world sync env for ObjectStore groups"
```

---

### Task 12: Operator wiring: no claims, refusal when off, deletion, sweeper

**Files:**
- Modify: `internal/controller/server_controller.go` (~lines 225-290), `internal/controller/setup.go` (`Options`, `ServerReconciler` fields), `internal/agentserver/writer.go` (`KubeWriter`, `DeleteServer`), `cmd/spawnery-operator/main.go` (flags ~line 278, `KubeWriter{...}` ~line 397, `SetupAll` ~line 416)
- Test: `internal/agentserver/deleteserver_test.go`, `internal/controller/` (the server reconciler's unit or envtest file that covers claim creation; `grep -ln "BuildDataClaim\|ServerClaimRejected" internal/controller/*_test.go`), `cmd/spawnery-operator/main_test.go` / `flags_docs_test.go`

**Interfaces:**
- Consumes: Tasks 7, 10, 11.
- Produces:
  ```go
  // agentserver
  type WorldDeleter interface {
      Exists(ctx context.Context, world string) (bool, error)
      MarkDeleted(ctx context.Context, world string) error
  }
  KubeWriter.Worlds WorldDeleter  // nil: world sync is off
  var ErrWorldSyncOff error
  // controller
  Options.WorldSync bool; Options.WorldSyncInterval time.Duration
  ServerReconciler.WorldSync bool; ServerReconciler.WorldSyncInterval time.Duration
  const ReasonWorldSyncOff = "WorldSyncOff"
  // worldsync
  type BucketWorlds struct { Store Store; Base string }   // implements agentserver.WorldDeleter
  ```

- [ ] **Step 1: Write the failing tests**

`internal/agentserver/deleteserver_test.go`:

```go
type fakeWorlds struct {
	exists  bool
	deleted []string
}

func (f *fakeWorlds) Exists(context.Context, string) (bool, error) { return f.exists, nil }
func (f *fakeWorlds) MarkDeleted(_ context.Context, w string) error {
	f.deleted = append(f.deleted, w)
	return nil
}

func objectStoreGroup() *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:    spawneryv1alpha1.ServerGroupOnDemand,
			Storage: &spawneryv1alpha1.StorageSpec{Backend: spawneryv1alpha1.StorageBackendObjectStore, Keep: []string{"world"}},
		},
	}
}

func TestDeleteOfAnObjectStoreWorldMarksItInTheBucket(t *testing.T) {
	c := fakeClient(t, objectStoreGroup())  // build like TestDeleteSeesAClaimTheCacheCannot does
	worlds := &fakeWorlds{exists: true}
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now, Worlds: worlds}
	got, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee")
	if err != nil {
		t.Fatal(err)
	}
	if !got.World || len(worlds.deleted) != 1 || worlds.deleted[0] != "minecraft/private-servers/c0ffee" {
		t.Fatalf("got %+v, marked %v", got, worlds.deleted)
	}
}

func TestDeleteOfAnObjectStoreWorldThatIsNowhereIsNotFound(t *testing.T) {
	c := fakeClient(t, objectStoreGroup())
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now, Worlds: &fakeWorlds{}}
	if _, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee"); !errors.Is(err, ErrNoSuchServer) {
		t.Fatalf("err = %v, want ErrNoSuchServer", err)
	}
}

func TestDeleteOfAnObjectStoreWorldWithSyncOffIsRefused(t *testing.T) {
	c := fakeClient(t, objectStoreGroup())
	w := KubeWriter{Client: c, Reader: c, Clock: time.Now}
	if _, err := w.DeleteServer(context.Background(), "minecraft", "private-servers", "c0ffee"); !errors.Is(err, ErrWorldSyncOff) {
		t.Fatalf("err = %v, want ErrWorldSyncOff", err)
	}
}
```

Factor the scheme/fake-client setup of `TestDeleteSeesAClaimTheCacheCannot` into `fakeClient(t, objs...)`. Find where agentserver maps writer errors to answers (`grep -n "ErrForeignClaim" internal/agentserver/*.go`) and map `ErrWorldSyncOff` the way `ErrGroupNotOnDemand` is mapped (a refusal, not an internal error); add a test beside the existing mapping test.

Server controller test (in the file that tests claim creation; envtest or fake, follow it):

```go
func TestAnObjectStoreMemberGetsNoClaim(t *testing.T) {
	// group: OnDemand, backend ObjectStore, keep [world]; reconciler WorldSync: true
	// after one reconcile: a pod exists, no PersistentVolumeClaim named DataClaimName(server)
}

func TestAnObjectStoreMemberWaitsWhenWorldSyncIsOff(t *testing.T) {
	// same group; reconciler WorldSync: false
	// after one reconcile: no pod, Accepted=False with reason WorldSyncOff
}
```

Write both against the file's existing fixtures; the assertions above are the contract.

- [ ] **Step 2: Run to verify failure** — `nix develop -c go test ./internal/agentserver/ ./internal/controller/ -run 'ObjectStore' -count=1 -p 1` → FAIL.

- [ ] **Step 3: Implement**

`internal/agentserver/writer.go`:

```go
// WorldDeleter is the operator's hold on a bucket of worlds. Worlds are
// named "<namespace>/<group>/<key>".
type WorldDeleter interface {
	Exists(ctx context.Context, world string) (bool, error)
	MarkDeleted(ctx context.Context, world string) error
}

var ErrWorldSyncOff = errors.New("the group keeps its worlds in an object store, and world sync is off in this operator")
```

`KubeWriter` gets `Worlds WorldDeleter` (comment: "Nil when the operator runs without --world-sync."). In `DeleteServer`, right after the `IsOnDemand` check:

```go
	if g.UsesObjectStore() {
		return w.deleteObjectStoreWorld(ctx, namespace, group, key, name)
	}
```

```go
func (w KubeWriter) deleteObjectStoreWorld(ctx context.Context, namespace, group, key, name string) (DeletedServer, error) {
	if w.Worlds == nil {
		return DeletedServer{}, ErrWorldSyncOff
	}
	world := namespace + "/" + group + "/" + key
	var srv spawneryv1alpha1.Server
	haveServer := true
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
		haveServer = false
	}
	if haveServer && (srv.Spec.GroupRef.Name != group || srv.Spec.Key != key) {
		haveServer = false
	}
	haveWorld, err := w.Worlds.Exists(ctx, world)
	if err != nil {
		return DeletedServer{}, err
	}
	if !haveServer && !haveWorld {
		return DeletedServer{}, ErrNoSuchServer
	}
	if haveServer {
		if err := w.Client.Delete(ctx, &srv); err != nil && !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
	}
	if err := w.Worlds.MarkDeleted(ctx, world); err != nil {
		return DeletedServer{}, err
	}
	return DeletedServer{Name: name, World: haveWorld}, nil
}
```

`internal/worldsync/deletion.go` gains the adapter:

```go
// BucketWorlds is agentserver.WorldDeleter over a Store.
type BucketWorlds struct {
	Store Store
	Base  string
}

func (b BucketWorlds) Exists(ctx context.Context, world string) (bool, error) {
	return WorldExists(ctx, b.Store, b.Base, world)
}

func (b BucketWorlds) MarkDeleted(ctx context.Context, world string) error {
	return MarkDeleted(ctx, b.Store, b.Base, world)
}
```

`internal/controller/server_controller.go`: replace `if !group.IsEphemeral() {` (the growClaim block) and `if createPod && !group.IsEphemeral() && !claimExists {` with `group.UsesClaim()`. Before the claim creation, add:

```go
	if createPod && group.UsesObjectStore() && !r.WorldSync {
		setAccepted(srv, false, ReasonWorldSyncOff,
			"the group keeps its worlds in an object store (spec.storage.backend ObjectStore), "+
				"and this operator runs without --world-sync; no pod is created until it does")
		createPod = false
	}
```

After `podspec.WithAOTCache(built)`:

```go
		if r.WorldSync && group.UsesObjectStore() {
			podspec.WithWorldSyncInterval(built, r.WorldSyncInterval)
		}
```

`setup.go`: `Options` gets

```go
	// WorldSync: spawnery-worldsync runs on the nodes and the operator holds
	// the bucket; groups with storage.backend ObjectStore get pods only then.
	WorldSync         bool
	WorldSyncInterval time.Duration
```

and the `ServerReconciler` literal passes both.

`cmd/spawnery-operator/main.go`:

```go
	flag.BoolVar(&worldSync, "world-sync", false,
		"serve groups with spec.storage.backend ObjectStore; needs spawnery-worldsync on the nodes "+
			"and WORLDSYNC_ENDPOINT, WORLDSYNC_REGION, WORLDSYNC_BUCKET, AWS_ACCESS_KEY_ID, "+
			"AWS_SECRET_ACCESS_KEY in the environment")
	flag.DurationVar(&worldSyncInterval, "world-sync-snapshot-interval", 5*time.Minute,
		"how often a member of an ObjectStore group asks for a snapshot; the most play a node loss costs")
```

After the manager exists:

```go
	var worlds agentserver.WorldDeleter
	if worldSync {
		cfg, base, err := worldsync.S3ConfigFromEnv(os.Getenv)
		if err != nil {
			setupLog.Error(err, "--world-sync needs the bucket")
			os.Exit(1)
		}
		st, err := worldsync.NewS3Store(cfg)
		if err != nil {
			setupLog.Error(err, "world sync store")
			os.Exit(1)
		}
		worlds = worldsync.BucketWorlds{Store: st, Base: base}
		if err := mgr.Add(&worldsync.Sweeper{Store: st, Base: base, Interval: time.Minute,
			StaleAfter: worldsync.StaleAfter, Log: ctrl.Log.WithName("worldsync")}); err != nil {
			setupLog.Error(err, "add world deletion sweeper")
			os.Exit(1)
		}
	}
```

`KubeWriter{..., Worlds: worlds}`; `controller.Options{..., WorldSync: worldSync, WorldSyncInterval: worldSyncInterval}`. Use the log variable name `main.go` already uses. `flags_docs_test.go` likely checks that flags are documented: run `make manifests` and commit the regenerated docs.

- [ ] **Step 4: Run tests**

Run: `nix develop -c go test ./internal/agentserver/ ./internal/controller/ ./cmd/spawnery-operator/ -count=1 -p 1`
Expected: PASS (controller ~85 s).

- [ ] **Step 5: Commit**

```bash
git add internal/ cmd/spawnery-operator/ docs/reference/
git commit -m "feat(controller): ObjectStore members get no claim, deletion marks the bucket"
```

---

### Task 13: Paper agent waits for the world and asks for snapshots

**Files:**
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/WorldSync.kt`
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`
- Test: `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/WorldSyncTest.kt`

**Interfaces:**
- Consumes: the control files of Task 8 (`ready`, `failed`, `snapshot.request`, `snapshot.done`) under `<cwd>/.spawnery-worldsync/`; env `SPAWNERY_WORLD_SYNC`, `SPAWNERY_WORLD_SYNC_INTERVAL`.
- Produces:
  ```kotlin
  class WorldSync(controlDir: Path, clock: () -> Long = System::currentTimeMillis, sleep: (Long) -> Unit = Thread::sleep) {
      sealed interface Outcome { object Ready; data class Failed(val reason: String); object TimedOut }
      fun awaitReady(timeoutMillis: Long): Outcome
      fun requestSnapshot(seq: Long)
      fun awaitSnapshot(seq: Long, timeoutMillis: Long): Outcome
      companion object { fun parseInterval(value: String?): Long? }  // millis, null if absent/invalid
  }
  ```

- [ ] **Step 1: Write the failing tests**

```kotlin
package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path

class WorldSyncTest {
    @TempDir
    lateinit var dir: Path

    private fun sync(onSleep: (Long) -> Unit = {}): WorldSync {
        var now = 0L
        return WorldSync(dir, clock = { now }, sleep = { now += it; onSleep(now) })
    }

    @Test
    fun `ready ends the wait`() {
        Files.writeString(dir.resolve("ready"), "")
        assertEquals(WorldSync.Outcome.Ready, sync().awaitReady(10_000))
    }

    @Test
    fun `failed carries the reason`() {
        Files.writeString(dir.resolve("failed"), "get objects/x: 503")
        assertEquals(WorldSync.Outcome.Failed("get objects/x: 503"), sync().awaitReady(10_000))
    }

    @Test
    fun `no answer times out`() {
        assertEquals(WorldSync.Outcome.TimedOut, sync().awaitReady(2_000))
    }

    @Test
    fun `ready written while waiting is seen`() {
        val s = sync { now -> if (now >= 1_000) Files.writeString(dir.resolve("ready"), "") }
        assertEquals(WorldSync.Outcome.Ready, s.awaitReady(10_000))
    }

    @Test
    fun `a snapshot request is written and its answer matched by sequence`() {
        val s = sync()
        s.requestSnapshot(4)
        assertEquals("4", Files.readString(dir.resolve("snapshot.request")).trim())
        Files.writeString(dir.resolve("snapshot.done"), "3")
        assertEquals(WorldSync.Outcome.TimedOut, s.awaitSnapshot(4, 1_000))
        Files.writeString(dir.resolve("snapshot.done"), "4")
        assertEquals(WorldSync.Outcome.Ready, s.awaitSnapshot(4, 1_000))
    }

    @Test
    fun `a failed snapshot answer is a failure`() {
        Files.writeString(dir.resolve("snapshot.done"), "failed 5 a file kept changing")
        assertEquals(WorldSync.Outcome.Failed("a file kept changing"), sync().awaitSnapshot(5, 1_000))
    }

    @Test
    fun `the interval parses Go durations`() {
        assertEquals(300_000L, WorldSync.parseInterval("5m0s"))
        assertEquals(90_000L, WorldSync.parseInterval("1m30s"))
        assertNull(WorldSync.parseInterval("soon"))
        assertNull(WorldSync.parseInterval(null))
    }
}
```

- [ ] **Step 2: Run to verify failure** — `nix develop -c make agent` → the paper test suite fails to compile (`WorldSync` unresolved).

- [ ] **Step 3: Implement**

`WorldSync.kt`:

```kotlin
package cloud.spawnery.agent.paper

import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.StandardCopyOption

/** The game server's side of the files spawnery-worldsync leaves in /data/.spawnery-worldsync. */
class WorldSync(
    private val controlDir: Path,
    private val clock: () -> Long = System::currentTimeMillis,
    private val sleep: (Long) -> Unit = Thread::sleep,
) {
    sealed interface Outcome {
        data object Ready : Outcome
        data class Failed(val reason: String) : Outcome
        data object TimedOut : Outcome
    }

    fun awaitReady(timeoutMillis: Long): Outcome = poll(timeoutMillis) {
        when {
            Files.exists(controlDir.resolve("failed")) ->
                Outcome.Failed(Files.readString(controlDir.resolve("failed")).trim())
            Files.exists(controlDir.resolve("ready")) -> Outcome.Ready
            else -> null
        }
    }

    fun requestSnapshot(seq: Long) {
        Files.createDirectories(controlDir)
        val tmp = controlDir.resolve("snapshot.request.tmp")
        Files.writeString(tmp, seq.toString())
        Files.move(tmp, controlDir.resolve("snapshot.request"), StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING)
    }

    fun awaitSnapshot(seq: Long, timeoutMillis: Long): Outcome = poll(timeoutMillis) {
        val done = controlDir.resolve("snapshot.done")
        if (!Files.exists(done)) return@poll null
        val answer = Files.readString(done).trim()
        when {
            answer == seq.toString() -> Outcome.Ready
            answer.startsWith("failed $seq ") -> Outcome.Failed(answer.removePrefix("failed $seq "))
            else -> null
        }
    }

    private fun poll(timeoutMillis: Long, check: () -> Outcome?): Outcome {
        val deadline = clock() + timeoutMillis
        while (true) {
            check()?.let { return it }
            if (clock() >= deadline) return Outcome.TimedOut
            sleep(POLL_MILLIS)
        }
    }

    companion object {
        const val POLL_MILLIS = 100L
        const val CONTROL_DIR = ".spawnery-worldsync"

        private val part = Regex("""(\d+)(h|m|s)""")

        /** Go's time.Duration.String() for whole seconds: "5m0s", "1h0m0s". */
        fun parseInterval(value: String?): Long? {
            if (value.isNullOrBlank()) return null
            var rest = value
            var millis = 0L
            while (rest!!.isNotEmpty()) {
                val m = part.matchAt(rest, 0) ?: return null
                val n = m.groupValues[1].toLong()
                millis += when (m.groupValues[2]) {
                    "h" -> n * 3_600_000
                    "m" -> n * 60_000
                    else -> n * 1_000
                }
                rest = rest.substring(m.value.length)
            }
            return if (millis > 0) millis else null
        }
    }
}
```

`AgentPlugin.kt` additions:

```kotlin
    private val worldSync: WorldSync? =
        if (System.getenv("SPAWNERY_WORLD_SYNC") == "1")
            WorldSync(Path.of(System.getProperty("user.dir"), WorldSync.CONTROL_DIR))
        else null
    private var snapshotSeq = 0L
    private var snapshotInFlight = false

    // Paper loads the worlds after every plugin's onLoad, so waiting here keeps
    // the server off a world that is still arriving. halt, not exit: no
    // shutdown hook may save a half-downloaded world.
    override fun onLoad() {
        val sync = worldSync ?: return
        val started = System.currentTimeMillis()
        when (val outcome = sync.awaitReady(WORLD_WAIT_MILLIS)) {
            WorldSync.Outcome.Ready ->
                logger.info("spawnery world sync: world ready after ${System.currentTimeMillis() - started} ms")
            is WorldSync.Outcome.Failed -> {
                logger.severe("spawnery world sync: the world could not be downloaded: ${outcome.reason}")
                Runtime.getRuntime().halt(1)
            }
            WorldSync.Outcome.TimedOut -> {
                logger.severe("spawnery world sync: no world after ${WORLD_WAIT_MILLIS / 1000} s")
                Runtime.getRuntime().halt(1)
            }
        }
    }

    private fun startSnapshots() {
        val sync = worldSync ?: return
        val millis = WorldSync.parseInterval(System.getenv("SPAWNERY_WORLD_SYNC_INTERVAL")) ?: DEFAULT_SNAPSHOT_MILLIS
        val ticks = millis / 50
        server.scheduler.runTaskTimer(this, Runnable { snapshot(sync) }, ticks, ticks)
    }

    /** Main thread: freeze saving, flush, ask; the wait runs off the main thread. */
    private fun snapshot(sync: WorldSync) {
        if (snapshotInFlight) return
        snapshotInFlight = true
        val autosave = server.worlds.associateWith { it.isAutoSave }
        autosave.keys.forEach { it.isAutoSave = false }
        server.dispatchCommand(server.consoleSender, "save-all flush")
        val seq = ++snapshotSeq
        sync.requestSnapshot(seq)
        server.scheduler.runTaskAsynchronously(this, Runnable {
            val outcome = sync.awaitSnapshot(seq, SNAPSHOT_WAIT_MILLIS)
            server.scheduler.runTask(this, Runnable {
                autosave.forEach { (world, on) -> world.isAutoSave = on }
                snapshotInFlight = false
                if (outcome != WorldSync.Outcome.Ready) {
                    logger.warning("spawnery world sync: snapshot $seq: $outcome")
                }
            })
        })
    }
```

Call `startSnapshots()` at the end of the `Environment.Configured` branch of `onEnable` (and also in the `Dormant` branch: world sync does not need the operator session). Constants in the companion (or file-level, matching the file's style):

```kotlin
private const val WORLD_WAIT_MILLIS = 10 * 60 * 1000L
private const val SNAPSHOT_WAIT_MILLIS = 60 * 1000L
private const val DEFAULT_SNAPSHOT_MILLIS = 5 * 60 * 1000L
```

- [ ] **Step 4: Run tests** — `nix develop -c make agent` → PASS (both plugins build, JUnit green).

- [ ] **Step 5: Commit**

```bash
git add agent/
git commit -m "feat(agent): wait for a synced world and ask for snapshots"
```

---

### Task 14: Chart

**Files:**
- Create: `charts/spawnery/templates/worldsync.yaml`
- Modify: `charts/spawnery/values.yaml`, `charts/spawnery/templates/deployment.yaml`, `charts/spawnery/Chart.yaml` (version later in Task 17)
- Generated: `docs/reference/chart-values.md` via `make manifests`
- Test: a helm render check in `hack/` style — add `hack/chart-worldsync-test.sh` (run by `make test` the way the other `*-test.sh` are; check the Makefile's `test` target for how they are listed)

- [ ] **Step 1: Write the failing render test**

`hack/chart-worldsync-test.sh`:

```bash
#!/usr/bin/env bash
# Renders the chart with world sync off and on and checks what each must hold.
set -euo pipefail
cd "$(dirname "$0")/.."

off="$(helm template t charts/spawnery)"
if grep -q 'worldsync.spawnery.cloud' <<<"$off"; then
	echo "world sync is off by default, yet the chart renders the driver" >&2
	exit 1
fi

on="$(helm template t charts/spawnery --namespace ops \
	--set worldSync.enabled=true \
	--set worldSync.namespace=ws \
	--set worldSync.objectStore.endpoint=https://s3.example \
	--set worldSync.objectStore.region=r \
	--set worldSync.objectStore.bucket=b \
	--set worldSync.objectStore.credentialsSecret=creds)"
for want in 'kind: CSIDriver' 'name: worldsync.spawnery.cloud' 'kind: DaemonSet' 'namespace: ws' \
	'--world-sync=true' 'WORLDSYNC_BUCKET' 'mountPropagation: Bidirectional' 'privileged: true'; do
	if ! grep -qF -- "$want" <<<"$on"; then
		echo "world sync on: the render lacks '$want'" >&2
		exit 1
	fi
done

if helm template t charts/spawnery --set worldSync.enabled=true >/dev/null 2>&1; then
	echo "world sync on without a bucket rendered; it must fail" >&2
	exit 1
fi
echo ok
```

Wire it into the Makefile's `test` target next to the other `hack/*-test.sh` calls.

- [ ] **Step 2: Run to verify failure** — `nix develop -c bash hack/chart-worldsync-test.sh` → FAIL (`lacks 'kind: CSIDriver'`).

- [ ] **Step 3: Implement**

`values.yaml`, at the end:

```yaml
# Worlds of groups with spec.storage.backend ObjectStore
# (docs/guides/object-store-worlds.md). Off by default.
worldSync:
  enabled: false
  # Where the node agent DaemonSet runs. It needs privileged pods, so it
  # cannot share a namespace held to the restricted Pod Security level. The
  # chart does not create it. Empty: the release namespace.
  namespace: ""
  image:
    repository: ghcr.io/spawnery/spawnery-worldsync
    # Empty: the chart's appVersion.
    tag: ""
    digest: ""
  registrarImage: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0
  objectStore:
    endpoint: ""
    region: ""
    bucket: ""
    # A prefix inside the bucket; empty for none.
    prefix: ""
    # A Secret with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, in both the
    # release namespace (for deletions) and worldSync.namespace.
    credentialsSecret: ""
  # The node directory for worlds and their snapshots.
  hostPath: /var/lib/spawnery/worldsync
  # How often a running member asks for a snapshot: the most play a node
  # loss costs.
  snapshotInterval: 5m
  nodeSelector: {}
  tolerations: []
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      memory: 512Mi
```

Check the registrar tag against `registry.k8s.io` when implementing and pin the newest v2 release.

`deployment.yaml`, in `args` after `--aot-cache`:

```yaml
            - --world-sync={{ .Values.worldSync.enabled }}
            {{- if .Values.worldSync.enabled }}
            - --world-sync-snapshot-interval={{ .Values.worldSync.snapshotInterval }}
            {{- end }}
```

and in `env`:

```yaml
            {{- if .Values.worldSync.enabled }}
            {{- include "spawnery.worldSyncEnv" . | nindent 12 }}
            {{- end }}
```

`_helpers.tpl`:

```yaml
{{- define "spawnery.worldSyncEnv" -}}
{{- $s := .Values.worldSync.objectStore -}}
- name: WORLDSYNC_ENDPOINT
  value: {{ required "worldSync.objectStore.endpoint is required" $s.endpoint | quote }}
- name: WORLDSYNC_REGION
  value: {{ required "worldSync.objectStore.region is required" $s.region | quote }}
- name: WORLDSYNC_BUCKET
  value: {{ required "worldSync.objectStore.bucket is required" $s.bucket | quote }}
- name: WORLDSYNC_PREFIX
  value: {{ $s.prefix | quote }}
- name: AWS_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ required "worldSync.objectStore.credentialsSecret is required" $s.credentialsSecret }}
      key: AWS_ACCESS_KEY_ID
- name: AWS_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $s.credentialsSecret }}
      key: AWS_SECRET_ACCESS_KEY
{{- end -}}

{{- define "spawnery.worldSyncImage" -}}
{{- $i := .Values.worldSync.image -}}
{{- if $i.digest -}}{{ $i.repository }}@{{ $i.digest }}{{- else -}}{{ $i.repository }}:{{ $i.tag | default .Chart.AppVersion }}{{- end -}}
{{- end -}}
```

`templates/worldsync.yaml`:

```yaml
{{- if .Values.worldSync.enabled }}
{{- $ns := .Values.worldSync.namespace | default .Release.Namespace }}
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: worldsync.spawnery.cloud
  labels:
    {{- include "spawnery.labels" . | nindent 4 }}
spec:
  attachRequired: false
  podInfoOnMount: true
  volumeLifecycleModes:
    - Ephemeral
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: spawnery-worldsync
  namespace: {{ $ns }}
  labels:
    {{- include "spawnery.labels" . | nindent 4 }}
automountServiceAccountToken: false
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: spawnery-worldsync
  namespace: {{ $ns }}
  labels:
    {{- include "spawnery.labels" . | nindent 4 }}
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: spawnery-worldsync
  template:
    metadata:
      labels:
        app.kubernetes.io/name: spawnery-worldsync
    spec:
      serviceAccountName: spawnery-worldsync
      # Uploads after a pod's stop continue across a restart from state.json;
      # the grace only lets an upload in flight finish its object.
      terminationGracePeriodSeconds: 30
      containers:
        - name: registrar
          image: {{ .Values.worldSync.registrarImage }}
          args:
            - --csi-address=/csi/csi.sock
            - --kubelet-registration-path=/var/lib/kubelet/plugins/worldsync.spawnery.cloud/csi.sock
          volumeMounts:
            - name: plugin-dir
              mountPath: /csi
            - name: registration-dir
              mountPath: /registration
        - name: worldsync
          image: {{ include "spawnery.worldSyncImage" . }}
          args:
            - node
            - --endpoint=unix:///csi/csi.sock
            - --root=/worldsync
          env:
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            {{- include "spawnery.worldSyncEnv" . | nindent 12 }}
          ports:
            - name: metrics
              containerPort: 8090
          securityContext:
            # Bind mounts into the kubelet's pod directories.
            privileged: true
          resources:
            {{- toYaml .Values.worldSync.resources | nindent 12 }}
          volumeMounts:
            - name: plugin-dir
              mountPath: /csi
            - name: pods-dir
              mountPath: /var/lib/kubelet/pods
              mountPropagation: Bidirectional
            - name: root
              mountPath: /worldsync
              mountPropagation: Bidirectional
      volumes:
        - name: plugin-dir
          hostPath:
            path: /var/lib/kubelet/plugins/worldsync.spawnery.cloud
            type: DirectoryOrCreate
        - name: registration-dir
          hostPath:
            path: /var/lib/kubelet/plugins_registry
            type: Directory
        - name: pods-dir
          hostPath:
            path: /var/lib/kubelet/pods
            type: Directory
        - name: root
          hostPath:
            path: {{ .Values.worldSync.hostPath }}
            type: DirectoryOrCreate
      {{- with .Values.worldSync.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.worldSync.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end }}
```

Run `nix develop -c make manifests` (regenerates `docs/reference/chart-values.md`).

- [ ] **Step 4: Run tests** — `nix develop -c bash hack/chart-worldsync-test.sh` → `ok`; `nix develop -c make test` (with `-p 1` semantics on the dev VM: run `go test -p 1` if `make test` exhausts memory) → PASS, including `internal/rbacaudit` (no RBAC changed).

- [ ] **Step 5: Commit**

```bash
git add charts/ hack/chart-worldsync-test.sh Makefile docs/reference/
git commit -m "feat(chart): world sync node agent, CSI driver and operator flags"
```

---

### Task 15: Build and publish the node agent image

**Files:**
- Create: `nix/worldsync-image.nix`
- Modify: `flake.nix` (package + image), `hack/publish.sh` (the list near line 44), `.github/workflows/release.yml` (the "what this tag publishes" step)

- [ ] **Step 1: Package**

In `flake.nix`, beside `spawnery-operator`:

```nix
          spawnery-worldsync = pkgs.buildGoModule {
            pname = "spawnery-worldsync";
            # Ships with the operator: the two agree on the bucket layout.
            version = operatorVersion;
            src = ./.;
            vendorHash = "<the same hash as the others>";
            subPackages = [ "cmd/spawnery-worldsync" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
          };
```

Add it to the `inherit` list of packages, and in the x86_64-linux block:

```nix
          worldsync-image = pkgs.callPackage ./nix/worldsync-image.nix {
            inherit spawnery-worldsync operatorVersion oci-common;
          };
```

`nix/worldsync-image.nix`:

```nix
# The node agent for ObjectStore worlds: a static binary, run as root in a
# privileged container because it bind-mounts into the kubelet's pod
# directories.
{ dockerTools
, spawnery-worldsync
, operatorVersion
, oci-common
}:

dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/spawnery-worldsync";
  tag = operatorVersion;
  architecture = "amd64";
  contents = [
    oci-common.passwd
    oci-common.group
    (oci-common.binIn { package = spawnery-worldsync; name = "spawnery-worldsync"; })
  ];
  config = {
    User = "0:0";
    WorkingDir = "/";
    Entrypoint = [ "/usr/local/bin/spawnery-worldsync" ];
    Labels = {
      "org.opencontainers.image.title" = "Spawnery world sync";
      "org.opencontainers.image.version" = operatorVersion;
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
    };
  };
}
```

The binary writes temporary files with `os.MkdirTemp` only in `import`; give the image a `/tmp` if `oci-common` offers one (look at how `purpur-image.nix` gets its `/tmp`), otherwise `import` passes `--tmp` pointing at the mounted directory. Prefer adding `/tmp`.

- [ ] **Step 2: Build it**

Run: `nix build .#worldsync-image --no-link` → succeeds. Then `nix build .#spawnery-worldsync && ./result/bin/spawnery-worldsync` → exit 2 with the usage line.

- [ ] **Step 3: Publish wiring**

In `hack/publish.sh`, add `"worldsync-image:result-worldsync"` beside `"operator-image:result-operator"`, and wherever the script special-cases `operator-image` (line ~161, digest handling), check whether the worldsync image needs the same; it is versioned like the operator. In `release.yml`, print `worldsync  $(nix eval --raw .#worldsync-image.imageTag)` in the "what this tag publishes" list and make sure the publish step runs `hack/publish.sh` with the new attribute (follow how `operator-image` is passed).

- [ ] **Step 4: Commit**

```bash
git add flake.nix nix/worldsync-image.nix hack/publish.sh .github/workflows/release.yml
git commit -m "build(worldsync): node agent image, published with the operator"
```

---

### Task 16: e2e on kind with MinIO

**Files:**
- Create: `hack/e2e-worldsync.sh`, `test/e2e/worldsync_test.go`, `test/e2e/manifests/worldsync.yaml`, `test/e2e/manifests/minio.yaml`, `test/e2e/worldsync-kind.yaml`
- Modify: `Makefile` (target `e2e-worldsync`), `.github/workflows/nightly.yml` (add the run beside `e2e-ondemand`, if that is where e2e-ondemand runs: `grep -n ondemand .github/workflows/*.yml`)

- [ ] **Step 1: Cluster and store manifests**

`test/e2e/worldsync-kind.yaml`:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
# Two workers: the test resumes a world on the node it did not run on.
nodes:
  - role: control-plane
  - role: worker
  - role: worker
```

`test/e2e/manifests/minio.yaml`: namespace `worldsync-e2e`, a MinIO Deployment (pin a current `quay.io/minio/minio:RELEASE.…` tag; it must support `If-Match` on PUT, which MinIO has since 2024), env `MINIO_ROOT_USER=e2e-access`, `MINIO_ROOT_PASSWORD=e2e-secret-key`, a Service `minio:9000`, and a Job with `quay.io/minio/mc` that creates bucket `worlds`. Plus the credentials Secret `worldsync-s3` (keys `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`) in both `platform-system` and `worldsync-e2e`.

`test/e2e/manifests/worldsync.yaml`: copy `ondemand.yaml`, rename the namespace to `minecraft-ws` (so both runs never share objects), and give the on-demand group

```yaml
  storage:
    size: 1Gi
    backend: ObjectStore
    keep:
      - world
```

- [ ] **Step 2: The script**

`hack/e2e-worldsync.sh` follows `hack/e2e-ondemand.sh` step for step, with these differences:
- builds and loads `worldsync-image` too;
- creates the cluster from `test/e2e/worldsync-kind.yaml`;
- applies `minio.yaml` and waits for the bucket Job;
- installs the chart with

  ```bash
  --set worldSync.enabled=true \
  --set worldSync.namespace=worldsync-e2e \
  --set worldSync.image.repository="$worldsync_repo" \
  --set worldSync.image.tag="$worldsync_tag" \
  --set worldSync.objectStore.endpoint=http://minio.worldsync-e2e.svc:9000 \
  --set worldSync.objectStore.region=us-east-1 \
  --set worldSync.objectStore.bucket=worlds \
  --set worldSync.objectStore.credentialsSecret=worldsync-s3
  ```

  after labelling `worldsync-e2e` with `pod-security.kubernetes.io/enforce=privileged`;
- waits for the DaemonSet rollout;
- runs `SPAWNERY_E2E_WORLDSYNC=1 go test -tags e2e -count=1 -v -timeout 40m -run TestAnObjectStoreWorld ./test/e2e/...`.

Add `e2e-worldsync:` to the Makefile like `e2e-ondemand:`.

- [ ] **Step 3: The test**

`test/e2e/worldsync_test.go` reuses `applyManifest`, `aProxyPodOf`, `startServer`, `stopServer`, `waitReady`, `writeMarker`, `readMarker`, `eventually` from the package:

```go
//go:build e2e

package e2e

// TestAnObjectStoreWorldTravelsBetweenNodes starts a member, writes a marker
// into its world, stops it, cordons the node it ran on, starts it again and
// reads the marker on the other node. Then it deletes the member and waits
// for the world's prefix to empty.
func TestAnObjectStoreWorldTravelsBetweenNodes(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_WORLDSYNC") != "1" {
		t.Skip("set SPAWNERY_E2E_WORLDSYNC=1; hack/e2e-worldsync.sh does")
	}
	// apply test/e2e/manifests/worldsync.yaml
	// first := startServer(...); waitReady(...)
	// node := the pod's spec.nodeName
	// marker := fmt.Sprintf("worldsync-e2e %d", time.Now().UnixNano()); writeMarker(...)
	// stopServer(...); wait for the pod to be gone
	// kubectl cordon <node>; t.Cleanup(uncordon)
	// second := startServer(...); waitReady(...)
	// assert the new pod's spec.nodeName != node
	// assert readMarker(...) == marker
	// deleteServer over the agent channel (same helper style as startServer)
	// eventually: `kubectl -n worldsync-e2e exec deploy/minio -- mc ls --recursive local/worlds/minecraft-ws/`
	//             prints nothing under the member's prefix
}
```

The helpers in `ondemand_test.go` are written against the constants `onDemandNamespace`, `onDemandGroup`, `onDemandKey`. Before writing the test, change them to take the namespace, group and key as parameters (keeping the on-demand test's behaviour), so both tests call the same code; commit that refactor separately first (`refactor(e2e): on-demand helpers take namespace, group and key`). Write the comments above as code in the same style as `TestAPrivateServersWorldOutlivesItsServer`; every assertion's failure message says what a user would see.

- [ ] **Step 4: Run it**

On a machine with a container runtime (paul-desktop, podman):

```bash
systemd-run --scope --user --property=Delegate=yes -- nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-worldsync
```

Expected: PASS. The dev VM cannot run this (memory); say so in the report rather than skipping silently.

- [ ] **Step 5: Commit**

```bash
git add hack/e2e-worldsync.sh test/e2e/ Makefile .github/workflows/
git commit -m "test(e2e): an ObjectStore world travels between nodes and is deleted"
```

---

### Task 17: Guide, spec status, versions

**Files:**
- Create: `docs/guides/object-store-worlds.md`
- Modify: `mkdocs.yml` (nav, beside `persistent-worlds.md`), `docs/superpowers/specs/2026-10-06-object-store-worlds-design.md` (status), `flake.nix` (`imageVersion`, `operatorVersion` → `0.24.0`), `charts/spawnery/Chart.yaml` (`version`, `appVersion` → `0.24.0`), `charts/spawnery/values.yaml` (`image.tag` → `0.24.0`), the on-demand manifests' image tags (`hack/e2e-ondemand.sh` checks them), `docs/reference/known-issues.md` (only if an open problem remains)

- [ ] **Step 1: The guide**

`docs/guides/object-store-worlds.md` explains, for someone running spawnery: what the backend does; the chart values (with an example for a generic S3 endpoint, invented bucket name); that the node agent's namespace must admit privileged pods; the `keep` requirement; that a plugin reading the world in its own `onLoad` must declare `SpawneryAgent` with `load: BEFORE`; the snapshot interval and what a node loss costs; the import command with a Job example that mounts a stopped member's claim read-only; the limits from the spec §8. Keep it within `hack/docs-length.sh`'s limits (`nix develop -c bash hack/docs-length.sh`).

- [ ] **Step 2: Versions and generated files**

Bump the four numbers to `0.24.0`, run `nix develop -c make manifests`, update image tags named in `test/e2e/manifests/ondemand.yaml`, `test/e2e/manifests/worldsync.yaml`, `config/samples/ondemand.yaml` and wherever `hack/image-tag-pins-agree.sh` looks. Set the spec's status to `implemented (branch feat/object-store-worlds)`, and correct it wherever the implementation ended up deciding differently from it.

- [ ] **Step 3: Full test run**

```bash
nix develop -c make test
nix develop -c make lint
nix develop -c make agent
nix build .#operator-image .#worldsync-image .#purpur-image --no-link
```

Expected: all green. Paste the tail of each into the report.

- [ ] **Step 4: Commit and open the PR**

```bash
git add -A docs/ mkdocs.yml flake.nix charts/ config/ test/e2e/manifests/
git commit -m "chore: 0.24.0, object store worlds guide"
git push -u origin feat/object-store-worlds
gh pr create --title "feat: on-demand worlds in an object store" --body-file <(…)  # body via the humanizer skill, ending with the Claude Code line
```

Merging and tagging `v0.24.0` wait for Paul's word.

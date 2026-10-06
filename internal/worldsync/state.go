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
	"path/filepath"
	"time"
)

// worldState is one world on this node, persisted as state.json beside its
// data directory. Every field is guarded by the world's lock.
type worldState struct {
	World        string      `json:"world"`
	Keep         []string    `json:"keep"`
	Replace      []string    `json:"replace,omitempty"`
	WorldID      string      `json:"worldId"`
	Generation   int64       `json:"generation"`
	ManifestETag string      `json:"manifestETag"`
	Files        []FileEntry `json:"files"`
	LeaseETag    string      `json:"leaseETag"`
	Target       string      `json:"target"`
	Pod          string      `json:"pod"`
	Group        *int        `json:"group,omitempty"`
	Pending      []int64     `json:"pending"`
	NextSeq      int64       `json:"nextSeq"`
	LastRequest  int64       `json:"lastRequest"`
	LastUsed     time.Time   `json:"lastUsed"`
	// Incomplete: the keep paths hold a download that did not finish.
	Incomplete bool `json:"incomplete,omitempty"`
	// FinalPending: the snapshot at unpublish failed and is still owed.
	FinalPending bool `json:"finalPending,omitempty"`

	// lost: the lease is gone and a pod still runs on the copy, which
	// now lies in lostDir.
	lost       bool
	lostDir    string
	download   *download
	uploading  chan struct{}
	working    bool
	retryAt    time.Time
	retryDelay time.Duration
}

func (s *worldState) gid() int {
	if s.Group == nil {
		return -1
	}
	return *s.Group
}

func (s *worldState) forgetContent() {
	s.WorldID, s.Generation, s.ManifestETag, s.Files = "", 0, "", nil
	s.Pending, s.FinalPending, s.Incomplete = nil, false, false
}

func (n *Node) worldDir(world string) string {
	return filepath.Join(n.cfg.Root, "worlds", filepath.FromSlash(world))
}

func (n *Node) dataDir(world string) string { return filepath.Join(n.worldDir(world), "data") }

func (n *Node) snapDir(world string, seq int64) string {
	return filepath.Join(n.worldDir(world), "snapshots", strconvI(seq))
}

func (n *Node) dataOf(s *worldState) string {
	if s.lostDir != "" {
		return filepath.Join(s.lostDir, "data")
	}
	return n.dataDir(s.World)
}

func (n *Node) saveState(s *worldState) error {
	if s.lost {
		return nil
	}
	return writeJSON(filepath.Join(n.worldDir(s.World), "state.json"), s)
}

// loadStates reads worlds/<ns>/<group>/<key>/state.json only: a file of that
// name deeper down belongs to a world's data.
func (n *Node) loadStates() ([]*worldState, error) {
	paths, err := filepath.Glob(filepath.Join(n.cfg.Root, "worlds", "*", "*", "*", "state.json"))
	if err != nil {
		return nil, err
	}
	var out []*worldState
	for _, p := range paths {
		var s worldState
		if err := readJSON(p, &s); err != nil {
			n.cfg.Log.Error(err, "skipping an unreadable world state", "path", p)
			continue
		}
		if filepath.Dir(p) != n.worldDir(s.World) {
			n.cfg.Log.Error(nil, "skipping a world state that names another world", "path", p, "world", s.World)
			continue
		}
		out = append(out, &s)
	}
	return out, nil
}

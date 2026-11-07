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
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spawnery/spawnery/internal/prune"
)

// StandbySlot is the local name of the slot "<namespace>/<group>/<server>".
func StandbySlot(standby string) (string, error) {
	if err := checkWorld(standby); err != nil {
		return "", fmt.Errorf("worldsync: standby %q is not <namespace>/<group>/<server>", standby)
	}
	ns, group := groupOf(standby)
	return ns + "/" + group + "/" + standbyPrefix + standby[strings.LastIndex(standby, "/")+1:], nil
}

// publishStandby gives a member of an Ephemeral group an empty directory. It
// makes no store call and takes no lease: until the member keys its world,
// nothing of it leaves the node.
func (n *Node) publishStandby(req PublishRequest) error {
	slot, err := StandbySlot(req.Standby)
	if err != nil {
		return err
	}
	if _, err := prune.ParseKeep(req.Keep); err != nil {
		return err
	}
	if _, err := prune.ParseReplace(req.Replace); err != nil {
		return err
	}
	// A slot that took a key since is found by its target, under its world.
	n.mu.Lock()
	name, ok := n.targets[req.Target]
	n.mu.Unlock()
	if !ok {
		name = slot
	}
	unlock := n.lock(name)
	defer unlock()
	if !ok {
		if err := n.dropSlotCache(slot); err != nil {
			return err
		}
	}
	s := n.state(name)
	if s.Target == req.Target {
		s.Group = req.Group
		if !s.lost {
			if err := n.shareWorld(s); err != nil {
				return err
			}
			n.save(s)
		}
		return n.rebind(s)
	}
	if s.Target != "" {
		if err := n.unpublishLocked(s); err != nil {
			return err
		}
		s = n.state(name)
	}
	// What a slot of this name left behind is no world of anybody's.
	if err := os.RemoveAll(n.worldDir(slot)); err != nil {
		return err
	}
	*s = worldState{
		World: slot, Standby: true, Server: req.Standby[strings.LastIndex(req.Standby, "/")+1:],
		Keep: req.Keep, Replace: req.Replace, Pod: req.Pod, Group: req.Group,
	}
	if err := n.markReady(s); err != nil {
		return err
	}
	if err := n.shareWorld(s); err != nil {
		return err
	}
	if err := n.bind(s, req.Target); err != nil {
		return err
	}
	s.LastUsed = n.cfg.Clock()
	return n.saveState(s)
}

// dropSlotCache frees a slot's directory that a world keyed in it still
// uses as its cache, which happens when a new member has the name of one
// that kept its world on this node. The cache goes only once it owes the
// store nothing; until then the slot is unavailable. It runs under the
// slot's lock.
func (n *Node) dropSlotCache(slot string) error {
	var world string
	n.mu.Lock()
	for w, s := range n.worlds {
		if s.Slot == slot {
			world = w
			break
		}
	}
	n.mu.Unlock()
	if world == "" {
		return nil
	}
	unlock, ok := n.tryLock(world)
	if !ok {
		return fmt.Errorf("%w: world %s in this slot is busy", ErrUnavailable, world)
	}
	defer unlock()
	s := n.lookup(world)
	if s == nil || s.Slot != slot {
		return nil
	}
	if !idle(s) {
		return fmt.Errorf("%w: world %s in this slot is in use or not fully uploaded", ErrUnavailable, world)
	}
	n.drop(s)
	if n.lookup(world) != nil {
		return fmt.Errorf("%w: could not drop the cache of world %s in this slot", ErrUnavailable, world)
	}
	return nil
}

// pollKeyRequests answers the key requests of mounted members: a standby
// slot takes its key, a slot that already took one says so again.
func (n *Node) pollKeyRequests(ctx context.Context) {
	n.eachFree(func(s *worldState) {
		if s.Target == "" || s.lost || (!s.Standby && s.Slot == "") {
			return
		}
		data := n.dataOf(s)
		if lstatAt(data, KeyDoneFile) == nil {
			return
		}
		b, err := readSmall(data, KeyRequestFile, 256)
		if err != nil {
			return
		}
		key := strings.TrimSpace(string(b))
		answer := key
		if err := n.takeKey(ctx, s, key); err != nil {
			n.cfg.Log.Error(err, "could not key the world", "slot", s.local(), "key", key)
			answer = fmt.Sprintf("failed %s %v", key, err)
		}
		if err := n.control(s, KeyDoneFile, answer); err != nil {
			n.cfg.Log.Error(err, "could not answer a key request", "world", s.World)
		}
	})
}

// takeKey moves a standby slot onto the world "<namespace>/<group>/<key>",
// under that world's lease and in the slot's own directory. From then on the
// world is synced as any other, snapshots and final upload included. It runs
// under the slot's lock.
func (n *Node) takeKey(ctx context.Context, s *worldState, key string) error {
	ns, group := groupOf(s.local())
	world := ns + "/" + group + "/" + key
	if err := checkWorld(world); err != nil || strings.ContainsAny(key, "/.") {
		return fmt.Errorf("%q is not a key", key)
	}
	if !s.Standby {
		if s.World != world {
			return fmt.Errorf("the world already took the key of %s", s.World)
		}
		return nil
	}
	unlock, ok := n.tryLock(world)
	if !ok {
		return errors.New("the world is busy on this node; ask again")
	}
	defer unlock()
	if other := n.lookup(world); other != nil {
		if !idle(other) {
			return errors.New("this node still holds that world for another member")
		}
		n.drop(other)
		if n.lookup(world) != nil {
			return errors.New("this node could not drop its old copy of that world")
		}
	}

	c, cancel := n.call(ctx)
	binding, err := ReadBinding(c, n.cfg.Store, n.prefix(world))
	cancel()
	switch {
	case errors.Is(err, ErrNotFound):
		return errors.New("the operator has not bound that world to this server")
	case err != nil:
		return fmt.Errorf("read the binding: %w", err)
	case binding.Server != s.Server || binding.Pod != s.Pod:
		return fmt.Errorf("the operator bound that world to server %s (pod %s)", binding.Server, binding.Pod)
	}
	if pending, err := n.deletionPending(ctx, world); err != nil {
		return fmt.Errorf("check for a pending deletion: %w", err)
	} else if pending {
		return errors.New("that world is being deleted")
	}
	etag, err := n.takeLeaseOn(ctx, world, s)
	if errors.Is(err, ErrLeaseLost) {
		leaseConflicts.Inc()
	}
	if err != nil {
		return fmt.Errorf("take the lease: %w", err)
	}
	if _, _, err := n.readManifest(ctx, world); !errors.Is(err, ErrNotFound) {
		if rerr := n.releaseLeaseOn(ctx, world, etag); rerr != nil {
			n.cfg.Log.Error(rerr, "could not release a lease taken for a key that was refused", "world", world)
		}
		if err == nil {
			return errors.New("that key already holds a world")
		}
		return fmt.Errorf("read the manifest: %w", err)
	}

	slot := s.World
	n.mu.Lock()
	delete(n.worlds, slot)
	n.worlds[world] = s
	n.targets[s.Target] = world
	n.mu.Unlock()
	s.World, s.Slot, s.Standby, s.LeaseETag, s.kept = world, slot, false, etag, nil
	s.LastUsed = n.cfg.Clock()
	n.refreshPolicy(ctx, s)
	if err := n.saveState(s); err != nil {
		return fmt.Errorf("save the state: %w", err)
	}
	n.cfg.Log.Info("keyed a world", "world", world, "slot", slot, "server", s.Server)
	return nil
}

func (n *Node) releaseLeaseOn(ctx context.Context, world, etag string) error {
	c, cancel := n.call(ctx)
	defer cancel()
	err := ReleaseLease(c, n.cfg.Store, n.prefix(world), etag)
	if errors.Is(err, ErrLeaseLost) {
		return nil
	}
	return err
}

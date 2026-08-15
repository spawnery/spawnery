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

package controller

import (
	"sort"
	"strconv"
	"strings"
)

// PersistentServerName is the name of the server holding one ordinal of a
// persistent group.
//
// The ordinal is the identity of a persistent server, and this name is how it
// is carried: podspec.DataClaimName derives the claim from the server's name,
// so this string is also what makes a world addressable across every deletion
// and recreation of the Server object. An ephemeral server is named by
// NewServerName instead, with a random suffix, because it has no identity to
// preserve.
func PersistentServerName(group string, ordinal int32) string {
	return group + "-" + strconv.Itoa(int(ordinal))
}

// OrdinalOf reads the ordinal back out of a server name, and reports whether
// the name is one PersistentServerName could have produced for this group.
//
// The prefix must equal the group exactly, and everything after it is the
// ordinal, which is what keeps a group whose own name ends in a number from
// reading its own name as an ordinal. The digits are parsed strictly:
// "survival-01" is refused rather than read as 1, because no name this package
// writes looks like that and accepting it would let two strings claim one
// identity.
func OrdinalOf(group, server string) (int32, bool) {
	prefix := group + "-"
	if !strings.HasPrefix(server, prefix) {
		return 0, false
	}
	digits := server[len(prefix):]
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 31)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

// PersistentInputs is everything the persistent sizing rule may look at. It
// carries none of what ScalingInputs holds about slots, capacity, player
// counts or generations, because a persistent group is sized by a number a
// user wrote down and by nothing else.
type PersistentInputs struct {
	// Group is the group's name, which is the prefix every one of its ordinal
	// names is built from.
	Group string
	// Replicas is spec.replicas: how many ordinals this group should have.
	Replicas int32
	// Views are the group's servers.
	Views []ServerView
	// PendingCreates are the servers this reconciler has asked to create and
	// the cache has not shown yet, by name. Without them a create in flight is
	// issued a second time under the same name.
	PendingCreates map[string]bool
	// PendingDeletes are the removals it has asked for and not yet seen.
	PendingDeletes map[string]bool
}

// DecidePersistentSize decides which ordinals a persistent group is missing
// and which it has too many of.
//
// It stands beside DecideSize rather than inside it: the two share a decision
// type and nothing else. The slot rule asks what capacity the players need;
// this one asks what number the user wrote in spec.replicas. The CRD forbids
// spec.scaling on a persistent group for exactly that reason.
//
// An ordinal counts as taken while any server carrying it exists, whatever
// phase that server is in. A draining server has not released its claim, and
// building its replacement now would mean two pods mounting one ReadWriteOnce
// volume -- which does not fail cleanly, it hangs on the volume. So the
// replacement waits for the drain, bounded by spec.drain.timeoutSeconds.
//
// A view's Ordinal, read from spec.ordinal by way of ServerView.Ordinal --
// never re-derived by parsing the name -- decides which ordinal a server
// holds. A view whose Ordinal is nil is ignored in both directions: it fills
// no ordinal, and it is not deleted as surplus. This rule removes what it can
// name, and something it cannot name is not its to remove.
func DecidePersistentSize(in PersistentInputs) SizeDecision {
	held := make(map[int32]string, len(in.Views))
	for _, v := range in.Views {
		if v.Ordinal == nil {
			continue
		}
		held[*v.Ordinal] = v.Name
	}

	var decision SizeDecision
	for ordinal := int32(0); ordinal < in.Replicas; ordinal++ {
		if _, taken := held[ordinal]; taken {
			continue
		}
		if in.PendingCreates[PersistentServerName(in.Group, ordinal)] {
			continue
		}
		decision.CreateOrdinals = append(decision.CreateOrdinals, ordinal)
	}

	surplus := make([]int32, 0, len(held))
	for ordinal := range held {
		if ordinal >= in.Replicas {
			surplus = append(surplus, ordinal)
		}
	}
	sort.Slice(surplus, func(i, j int) bool { return surplus[i] > surplus[j] })
	for _, ordinal := range surplus {
		name := held[ordinal]
		if in.PendingDeletes[name] {
			continue
		}
		if viewByName(in.Views, name).leaving() {
			continue
		}
		decision.Delete = append(decision.Delete, name)
	}
	return decision
}

func viewByName(views []ServerView, name string) ServerView {
	for _, v := range views {
		if v.Name == name {
			return v
		}
	}
	return ServerView{}
}

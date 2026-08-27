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

	"github.com/spawnery/spawnery/internal/phase"
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
	// PodHash is podspec.DesiredServerHash for the group as it stands now. A
	// view whose PodHash differs is stale; a view whose PodHash is empty is
	// adopted rather than replaced, so an upgrade that introduces the field
	// does not restart every world in the installation.
	//
	// Empty is also what a caller that has not yet computed the hash passes,
	// and that case is handled the same way as a view's empty hash: adopted,
	// not compared. See the empty check in DecidePersistentSize's stale loop.
	PodHash string
}

// DecidePersistentSize decides which ordinals a persistent group is missing,
// and, when it has too many, which one goes first: a surplus ordinal outranks
// a stale spec, which outranks -- lowest, and gated the same two ways stale
// is -- a claim still waiting on a filesystem resize.
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
//
// # An ordinal two servers carry
//
// held is a map, so a second server claiming an ordinal used to overwrite the
// first and the loser vanished from this rule entirely: never surplus, because
// the surplus loop walks held and the loser is not in it; never recreated,
// because the ordinal is taken; and still running its pod against the claim
// named after its own name. Which of the two survived in held was decided by
// the order of in.Views, so the answer could change between passes.
//
// Such an ordinal is now collected into Conflicts and excluded from all three
// nominations below. Excluding it is the point rather than a side effect. The
// deletes all read held[ordinal], so acting on a duplicated ordinal means
// deleting whichever of the two the map happened to keep -- an arbitrary
// choice between two worlds, and an irreversible one. Refusing leaves the
// group oversized or stale until a person resolves it, which is the safe half
// of that trade and the one this repository takes everywhere else.
//
// Creation is deliberately not excluded, because it is not affected: the
// create loop asks only whether an ordinal is taken, and a duplicated one is
// taken twice over.
func DecidePersistentSize(in PersistentInputs) SizeDecision {
	held := make(map[int32]string, len(in.Views))
	// carrying is only built for the ordinals that turn out to be duplicated,
	// which is why it is filled from held rather than in place of it: the
	// ordinary case allocates nothing beyond the map header.
	carrying := map[int32][]string{}
	for _, v := range in.Views {
		if v.Ordinal == nil {
			continue
		}
		if first, taken := held[*v.Ordinal]; taken {
			if len(carrying[*v.Ordinal]) == 0 {
				carrying[*v.Ordinal] = []string{first}
			}
			carrying[*v.Ordinal] = append(carrying[*v.Ordinal], v.Name)
			continue
		}
		held[*v.Ordinal] = v.Name
	}

	var decision SizeDecision
	if len(carrying) > 0 {
		decision.Conflicts = ordinalConflicts(carrying)
	}
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
		if carrying[ordinal] != nil {
			continue
		}
		if ordinal >= in.Replicas {
			surplus = append(surplus, ordinal)
		}
	}
	sort.Slice(surplus, func(i, j int) bool { return surplus[i] > surplus[j] })

	// Gate A subsumes the PendingDeletes and leaving() checks the surplus
	// loop used to make per-candidate: any outstanding delete or any server
	// already on its way out means an ordinal is already down, and this rule
	// nominates at most one at a time. So the check moves in front of both the
	// surplus and the stale nomination below, rather than filtering each one's
	// candidate list.
	if takedownInFlight(in) {
		return decision
	}

	if len(surplus) > 0 {
		// surplus is already sorted highest-first.
		decision.Delete = append(decision.Delete, held[surplus[0]])
		decision.DeleteReason = "SurplusOrdinal"
		return decision
	}

	// Gate B: a stale ordinal is nominated only once every ordinal the group
	// is supposed to have is confirmed Ready.
	//
	// An ordinal that can never become Ready therefore holds the whole group
	// at its current spec, forever: nothing times this wait out. That is
	// inherited from StatefulSet's shape knowingly rather than overlooked, and
	// it is tolerable only because the stall is reported -- ConditionDegraded
	// publishes for a persistent group, and CountFailures' requiredOrdinals
	// rule is what makes it actually arrive rather than being reset by a
	// healthy sibling. An operator who sees Degraded on a persistent group and
	// an update that has not moved is looking at this.
	// Surplus removal above never
	// reaches here, and that is deliberate -- see groupRecovered's comment.
	if !groupRecovered(in) {
		return decision
	}

	stale := make([]int32, 0, len(held))
	for ordinal, name := range held {
		if ordinal >= in.Replicas || carrying[ordinal] != nil {
			continue
		}
		v := viewByName(in.Views, name)
		// An empty view hash is adopted rather than compared -- see
		// ServerView.PodHash. An empty in.PodHash is the same adoption from
		// the other side: see PersistentInputs.PodHash. Both live in
		// staleSpec, which the ephemeral rule calls too, so the two group
		// kinds cannot drift apart on what stale means.
		if !staleSpec(v, in.PodHash) {
			continue
		}
		stale = append(stale, ordinal)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i] > stale[j] })
	if len(stale) > 0 {
		decision.Delete = append(decision.Delete, held[stale[0]])
		decision.DeleteReason = "StaleSpec"
		return decision
	}

	// The lowest-priority class: a claim the CSI driver has grown but whose
	// filesystem needs the pod restarted to follow. Reached only when nothing
	// missing, surplus or stale outranked it -- most drivers expand online and
	// never ask for this at all.
	resizing := make([]int32, 0, len(held))
	for ordinal, name := range held {
		if ordinal >= in.Replicas || carrying[ordinal] != nil {
			continue
		}
		if viewByName(in.Views, name).ResizePending {
			resizing = append(resizing, ordinal)
		}
	}
	sort.Slice(resizing, func(i, j int) bool { return resizing[i] > resizing[j] })
	if len(resizing) > 0 {
		decision.Delete = append(decision.Delete, held[resizing[0]])
		decision.DeleteReason = "ResizePending"
	}
	return decision
}

// ordinalConflicts turns the duplicate map into the sorted list a caller can
// put in a message. Sorted by ordinal, and each ordinal's names sorted too, so
// the condition a group publishes does not change its wording between passes
// over a state that has not changed -- a condition whose message churns is one
// whose LastTransitionTime stops meaning anything.
func ordinalConflicts(carrying map[int32][]string) []OrdinalConflict {
	out := make([]OrdinalConflict, 0, len(carrying))
	for ordinal, names := range carrying {
		sorted := append([]string(nil), names...)
		sort.Strings(sorted)
		out = append(out, OrdinalConflict{Ordinal: ordinal, Servers: sorted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

// takedownInFlight is Gate A: does any ordinal-bearing view of the group
// already count as leaving(), or have an outstanding PendingDeletes entry.
// DecidePersistentSize nominates at most one name and returns, so a single
// call was never the risk; this gate is what makes the *next* call wait while
// an ordinal is on its way out.
//
// On its way out for any reason, not only this rule's own prior nomination:
// leaving() is equally true of a server condemned off a draining node, one
// escalated out of Retiring, and one an operator deleted by hand. This gate
// holds for all of them, so the rule adds nothing to a takedown it did not
// order. What it cannot do is bound one: condemn() names every server on a
// departing node and removes them all in a single pass, ungated by anything
// here, so a node holding two ordinals takes two down -- and an ordinal this
// rule nominated on an earlier pass can still be draining while that happens.
// Deliberate and unthrottled since 4c-3, see docs/known-issues.md's "A node
// holding a whole group empties it at once". So the one-at-a-time budget is a
// statement about what this rule nominates, not about every way an ordinal
// can go down.
//
// A view with a nil Ordinal is skipped, the same as every other pass over
// in.Views in this file: DecidePersistentSize's own doc comment says a
// nil-ordinal view "fills no ordinal, and it is not deleted as surplus" --
// this rule removes what it can name, and something it cannot name is not
// its to remove. See docs/known-issues.md's "A squatter can stall an
// ordinal silently": without this, the same kind of object would stall every
// ordinal's takedown instead of just its own.
func takedownInFlight(in PersistentInputs) bool {
	for _, v := range in.Views {
		if v.Ordinal == nil {
			continue
		}
		if v.leaving() || in.PendingDeletes[v.Name] {
			return true
		}
	}
	return false
}

// groupRecovered is Gate B: does every ordinal the group is supposed to have
// currently have a Ready server.
//
// It deliberately does not gate surplus removal. A surplus ordinal sits above
// Replicas and is invisible to this test, so relying on it there would
// release the next nomination while the previous one was still draining --
// Gate A is what holds the invariant for surplus. Beyond the mechanics:
// scaling down is an instruction an operator gave explicitly, often because
// something is wrong, and withholding it until an unrelated ordinal recovers
// withholds the remedy.
func groupRecovered(in PersistentInputs) bool {
	readyOrdinals := make(map[int32]bool, len(in.Views))
	for _, v := range in.Views {
		if v.Ordinal != nil && v.Phase == phase.Ready {
			readyOrdinals[*v.Ordinal] = true
		}
	}
	for ordinal := int32(0); ordinal < in.Replicas; ordinal++ {
		if !readyOrdinals[ordinal] {
			return false
		}
	}
	return true
}

func viewByName(views []ServerView, name string) ServerView {
	for _, v := range views {
		if v.Name == name {
			return v
		}
	}
	return ServerView{}
}

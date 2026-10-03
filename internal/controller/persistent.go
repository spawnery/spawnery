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

package controller

import (
	"sort"
	"strconv"
	"strings"

	"github.com/spawnery/spawnery/internal/phase"
)

// PersistentServerName is also the claim's identity: podspec.DataClaimName
// derives the claim from the server's name.
func PersistentServerName(group string, ordinal int32) string {
	return group + "-" + strconv.Itoa(int(ordinal))
}

// OrdinalOf inverts PersistentServerName. "survival-01" is refused rather than
// read as 1, so two strings cannot claim one identity.
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

type PersistentInputs struct {
	Group    string
	Replicas int32
	Views    []ServerView
	// Created but not yet in the cache, by name.
	PendingCreates map[string]bool
	PendingDeletes map[string]bool
	// PodHash is podspec.DesiredServerHash. Empty here, or on a view, means
	// adopt rather than compare (see staleSpec).
	PodHash string
	// ChangeoverRefused withholds the stale nomination while an earlier stage
	// of the network is still changing over.
	ChangeoverRefused bool
}

// DecidePersistentSize nominates at most one delete: surplus outranks stale,
// which outranks a claim waiting on a filesystem resize.
//
// An ordinal stays taken while its server drains: a replacement now would put
// two pods on one ReadWriteOnce volume, which hangs rather than fails. A view
// with a nil Ordinal fills no ordinal and is never deleted.
//
// An ordinal two servers carry goes into Conflicts and is excluded from every
// delete, since held keeps one of them arbitrarily and deleting it would be an
// irreversible coin toss between two worlds.
func DecidePersistentSize(in PersistentInputs) SizeDecision {
	held := make(map[int32]string, len(in.Views))
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

	if takedownInFlight(in) {
		return decision
	}

	if len(surplus) > 0 {
		decision.Delete = append(decision.Delete, held[surplus[0]])
		decision.DeleteReason = "SurplusOrdinal"
		return decision
	}

	// An ordinal that never becomes Ready holds the update forever, as in a
	// StatefulSet; the stall surfaces as ConditionDegraded.
	if !groupRecovered(in) {
		return decision
	}

	stale := make([]int32, 0, len(held))
	for ordinal, name := range held {
		if ordinal >= in.Replicas || carrying[ordinal] != nil {
			continue
		}
		v := viewByName(in.Views, name)
		if !staleSpec(v, in.PodHash) {
			continue
		}
		stale = append(stale, ordinal)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i] > stale[j] })
	if len(stale) > 0 && !in.ChangeoverRefused {
		decision.Delete = append(decision.Delete, held[stale[0]])
		decision.DeleteReason = "StaleSpec"
		return decision
	}

	// A claim the CSI driver has grown whose filesystem needs a pod restart.
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

// ordinalConflicts sorts so the condition message does not churn between
// passes over an unchanged state.
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

// takedownInFlight holds the next nomination while any ordinal is leaving, for
// whatever reason. It does not bound condemn(), which takes down every server
// on a departing node in one pass, deliberately: those players are evicted
// regardless.
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

// groupRecovered deliberately does not gate surplus removal: scaling down is
// often the remedy for the ordinal that is not recovering.
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

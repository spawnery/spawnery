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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// expectationTTL bounds how long an unobserved create, delete or retire is
// believed, so a lost watch event cannot block sizing forever. An unobserved
// retire holds a slot of spec.update.maxUnavailable until it expires.
const expectationTTL = 30 * time.Second

type expectationKind int

const (
	expectationCreate expectationKind = iota
	expectationDelete
	expectationRetire
)

type expectation struct {
	kind    expectationKind
	expires time.Time
	// number is 0 where no number was searched for: a proxy pod, or a
	// persistent server whose number is its ordinal.
	number int32
}

// expectations reserves the creates, deletes and retirements a reconcile has
// issued and the cache has not caught up with yet, so a reconcile reading a
// stale cache neither creates a second server nor exceeds maxUnavailable.
//
// Kept apart from the ServerView list on purpose: SelectDeletionCandidates
// sorts never-played servers first and would nominate a placeholder at once,
// and AggregateGroup and the PodDisruptionBudget read that same slice.
type expectations struct {
	mu      sync.Mutex
	now     func() time.Time
	byGroup map[string]map[string]expectation
}

func newExpectations(now func() time.Time) *expectations {
	return &expectations{now: now, byGroup: make(map[string]map[string]expectation)}
}

// Pass 0 for number where none was assigned.
func (e *expectations) expectCreated(group, name string, number int32) {
	e.record(group, name, expectationCreate, number)
}

func (e *expectations) expectDeleted(group, name string) {
	e.record(group, name, expectationDelete, 0)
}

func (e *expectations) expectRetired(group, name string) {
	e.record(group, name, expectationRetire, 0)
}

func (e *expectations) record(group, name string, kind expectationKind, number int32) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m, ok := e.byGroup[group]
	if !ok {
		m = make(map[string]expectation)
		e.byGroup[group] = m
	}
	m[name] = expectation{kind: kind, expires: e.now().Add(expectationTTL), number: number}
}

func (e *expectations) observe(group string, views []ServerView) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m := e.byGroup[group]
	if len(m) == 0 {
		return
	}
	seen := make(map[string]ServerView, len(views))
	for _, v := range views {
		seen[v.Name] = v
	}

	now := e.now()
	for name, exp := range m {
		if !now.Before(exp.expires) {
			delete(m, name)
			continue
		}
		v, present := seen[name]
		switch exp.kind {
		case expectationCreate:
			if present {
				delete(m, name)
			}
		case expectationDelete:
			// leavingByPhase(), not leaving(): Condemned is a node-level signal that
			// can turn true before the cache shows this delete, and clearing on it
			// would let condemned() re-list the same server next pass.
			if !present || v.leavingByPhase() {
				delete(m, name)
			}
		case expectationRetire:
			if !present || v.Retire {
				delete(m, name)
			}
		}
	}
	if len(m) == 0 {
		delete(e.byGroup, group)
	}
}

func (e *expectations) observePods(group string, pods []corev1.Pod) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m := e.byGroup[group]
	if len(m) == 0 {
		return
	}
	seen := make(map[string]bool, len(pods))
	for i := range pods {
		seen[pods[i].Name] = true
	}

	now := e.now()
	for name, exp := range m {
		if !now.Before(exp.expires) {
			delete(m, name)
			continue
		}
		switch exp.kind {
		case expectationCreate:
			if seen[name] {
				delete(m, name)
			}
		case expectationDelete:
			if !seen[name] {
				delete(m, name)
			}
		}
	}
	if len(m) == 0 {
		delete(e.byGroup, group)
	}
}

// pending returns the reserved creates, deletes and retires, keyed by name;
// creates are named because the persistent rule needs their ordinals.
func (e *expectations) pending(group string) (map[string]bool, map[string]bool, map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	creates := make(map[string]bool)
	deletes := make(map[string]bool)
	retires := make(map[string]bool)
	for name, exp := range e.byGroup[group] {
		switch exp.kind {
		case expectationCreate:
			creates[name] = true
		case expectationDelete:
			deletes[name] = true
		case expectationRetire:
			retires[name] = true
		}
	}
	return creates, deletes, retires
}

func (e *expectations) pendingNumbers(group string) map[int32]bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	numbers := make(map[int32]bool)
	for _, exp := range e.byGroup[group] {
		if exp.kind == expectationCreate && exp.number > 0 {
			numbers[exp.number] = true
		}
	}
	return numbers
}

func (e *expectations) forget(group string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.byGroup, group)
}

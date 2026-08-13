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
	"sync"
	"time"
)

// expectationTTL bounds how long an unobserved create or delete is believed.
// Without it, a lost watch event would leave a reservation standing forever and
// the group could never size itself again; with it, the group decides on what
// the cache shows, which by then is correct.
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
}

// expectations reserves the creates and deletes a reconcile has issued and the
// cache has not caught up with yet.
//
// collectViews lists Servers through the manager's cached client. A reconcile
// triggered by its own create event can therefore read a cache that has not
// caught up, see the group still short, create a second server, and have the
// next pass delete the surplus again. Holding a floor hits that rarely; a
// scaler that creates servers in response to player counts hits it as a matter
// of course.
//
// This is the ReplicaSet controller's mechanism, keyed by name rather than by
// count, which makes observing one a set membership test that needs no
// ordering. It is deliberately not folded into the ServerView list:
// SelectDeletionCandidates sorts servers that never took players first and
// would nominate a placeholder immediately, and AggregateGroup and the
// PodDisruptionBudget read that same slice.
//
// Safe for concurrent use: one instance is shared by every reconcile of every
// group.
type expectations struct {
	mu      sync.Mutex
	now     func() time.Time
	byGroup map[string]map[string]expectation
}

func newExpectations(now func() time.Time) *expectations {
	return &expectations{now: now, byGroup: make(map[string]map[string]expectation)}
}

// expectCreated records a Server this reconciler has just created.
func (e *expectations) expectCreated(group, name string) {
	e.record(group, name, expectationCreate)
}

// expectDeleted records a Server whose removal this reconciler has just asked
// for.
func (e *expectations) expectDeleted(group, name string) {
	e.record(group, name, expectationDelete)
}

// expectRetired records a Server this reconciler has just asked to retire.
func (e *expectations) expectRetired(group, name string) {
	e.record(group, name, expectationRetire)
}

func (e *expectations) record(group, name string, kind expectationKind) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m, ok := e.byGroup[group]
	if !ok {
		m = make(map[string]expectation)
		e.byGroup[group] = m
	}
	m[name] = expectation{kind: kind, expires: e.now().Add(expectationTTL)}
}

// observe drops every reservation the cache has caught up with, and every one
// that has waited longer than expectationTTL.
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
			// Gone, or on its way out: either way the cache has caught up with
			// the removal, and the group may size itself on what it shows.
			if !present || v.leaving() {
				delete(m, name)
			}
		case expectationRetire:
			// Satisfied when the cache shows the patch, and also when the
			// server is gone: a retirement that completed between the patch
			// and this list has nothing left to reserve.
			if !present || v.Retire {
				delete(m, name)
			}
		}
	}
	if len(m) == 0 {
		delete(e.byGroup, group)
	}
}

// pending is what the group has outstanding: how many creates, which names are
// already on their way out, and which have been asked to retire.
func (e *expectations) pending(group string) (int32, map[string]bool, map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var creates int32
	deletes := make(map[string]bool)
	retires := make(map[string]bool)
	for name, exp := range e.byGroup[group] {
		switch exp.kind {
		case expectationCreate:
			creates++
		case expectationDelete:
			deletes[name] = true
		case expectationRetire:
			retires[name] = true
		}
	}
	return creates, deletes, retires
}

// forget drops a group entirely, so the map does not grow with every group
// that ever existed.
func (e *expectations) forget(group string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.byGroup, group)
}

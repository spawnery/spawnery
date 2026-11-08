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

package agentserver

import (
	"math"
	"sync"
	"time"
)

// fillCandidate is one eligible server as Fill sees it. Players includes the
// chooser's own tally; Line is the resolved playable slots; Slots 0 means the
// server has not reported and has room without limit.
type fillCandidate struct {
	Name                         string
	Players, Slots, Line, Number int32
	Stale                        bool
}

// chooseFill picks the fullest server below its fill line. The proxies' Router
// applies the same order, so every chooser picks the same server.
func chooseFill(cands []fillCandidate) (string, bool) {
	if len(cands) == 0 {
		return "", false
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if fillsBefore(c, best) {
			best = c
		}
	}
	return best.Name, true
}

func fillsBefore(a, b fillCandidate) bool {
	ka, kb := fillKeyOf(a), fillKeyOf(b)
	if ka.room != kb.room {
		return ka.room
	}
	if a.Stale != b.Stale {
		return !a.Stale
	}
	if ka.belowLine != kb.belowLine {
		return ka.belowLine
	}
	if ka.belowLine {
		if a.Players != b.Players {
			return a.Players > b.Players
		}
	} else if ka.free != kb.free {
		return ka.free > kb.free
	}
	if ka.number != kb.number {
		return ka.number < kb.number
	}
	return a.Name < b.Name
}

type fillKey struct {
	room, belowLine bool
	free, number    int64
}

func fillKeyOf(c fillCandidate) fillKey {
	slots, line := int64(c.Slots), int64(c.Line)
	if c.Slots == 0 {
		slots, line = math.MaxInt64, math.MaxInt64
	}
	number := int64(c.Number)
	if c.Number == 0 {
		number = math.MaxInt64
	}
	players := int64(c.Players)
	return fillKey{
		room:      players < slots,
		belowLine: players < line,
		free:      slots - players,
		number:    number,
	}
}

// pendingMoves counts the players the operator has sent to a server lately,
// which the server's reported count does not include yet.
type pendingMoves struct {
	clock func() time.Time
	ttl   time.Duration
	// afterChoose runs between a reserve's choice and its record; a test
	// holds it to make a second chooser arrive inside that window.
	afterChoose func()

	mu    sync.Mutex
	moves map[pendingKey][]time.Time
}

type pendingKey struct{ namespace, server string }

func newPendingMoves(clock func() time.Time, ttl time.Duration) *pendingMoves {
	return &pendingMoves{clock: clock, ttl: ttl, moves: map[pendingKey][]time.Time{}}
}

func (p *pendingMoves) add(namespace, server string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addLocked(namespace, server, p.clock())
}

func (p *pendingMoves) count(namespace, server string) int32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.countLocked(namespace, server, p.clock())
}

// reserve runs choose against the tally and records the server it returns
// when move is true, under one lock, so two sessions cannot both read a
// server's last seat as free.
func (p *pendingMoves) reserve(
	namespace string,
	choose func(count func(server string) int32) (server string, move bool),
) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.clock()
	server, move := choose(func(server string) int32 {
		return p.countLocked(namespace, server, now)
	})
	if p.afterChoose != nil {
		p.afterChoose()
	}
	if move {
		p.addLocked(namespace, server, now)
	}
}

func (p *pendingMoves) addLocked(namespace, server string, now time.Time) {
	for key := range p.moves {
		p.expire(key, now)
	}
	key := pendingKey{namespace, server}
	p.moves[key] = append(p.moves[key], now)
}

func (p *pendingMoves) countLocked(namespace, server string, now time.Time) int32 {
	key := pendingKey{namespace, server}
	p.expire(key, now)
	return int32(len(p.moves[key]))
}

func (p *pendingMoves) expire(key pendingKey, now time.Time) {
	live := p.moves[key][:0]
	for _, at := range p.moves[key] {
		if now.Sub(at) < p.ttl {
			live = append(live, at)
		}
	}
	if len(live) == 0 {
		delete(p.moves, key)
		return
	}
	p.moves[key] = live
}

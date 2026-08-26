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

package main

import (
	"net"
	"sync"
	"sync/atomic"
)

// deafness is the fourth thing this stub can do to an agent, and the only one
// the agent cannot be told about: at a chosen moment every connection goes
// silent without being closed. Nothing is written, nothing that arrives is
// delivered upward, and no FIN and no RST ever reach the agent.
//
// # What this reproduces, and what it does not
//
// It is the shape of a peer that is gone without the transport having noticed,
// which docs/known-issues.md measured at over 200 seconds and twice not at all
// within 213. It is not the same fault. A real black hole drops packets, so
// the agent's own TCP gets no acknowledgements either and eventually gives up
// on its own; here the stub's kernel goes on acknowledging, so nothing under
// the agent will ever end the wait. That makes this the harsher half of the
// pair and the honest one to test a keepalive against: if the ping is what
// ends the wait, it is the only thing that can.
//
// The two other ways the stub can go quiet are different states and are not
// this one. --mute-after is an operator that accepts a stream and never
// answers it, which SessionLoop.awaitAnswer already has a clock for. Killing
// the stub closes its sockets, which the agent sees at once. This is the state
// with no clock on either side, and OperatorChannel's keepalive is what this
// exists to exercise.
//
// One inbound frame may still be processed after deafness begins: a Read
// already parked in the kernel returns whatever arrives next before the check
// below is reached again. It changes nothing the agent can observe, because
// every write out is discarded from the same instant.
type deafness struct{ on atomic.Bool }

// listener wraps inner so that every connection it hands out honours d.
func (d *deafness) listener(inner net.Listener) net.Listener {
	return &deafListener{Listener: inner, deafness: d}
}

type deafListener struct {
	net.Listener
	deafness *deafness
}

func (l *deafListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &deafConn{Conn: conn, deafness: l.deafness, closed: make(chan struct{})}, nil
}

type deafConn struct {
	net.Conn
	deafness *deafness
	closed   chan struct{}
	once     sync.Once
}

// Read blocks for the life of the connection once deafness has begun, which is
// what makes this a black hole rather than an error: an error would break the
// stream, and a broken stream is the one thing the agent already handles.
func (c *deafConn) Read(p []byte) (int, error) {
	if c.deafness.on.Load() {
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Read(p)
}

// Write reports success and sends nothing. The server's HTTP/2 layer goes on
// believing it answered -- a ping acknowledgement included, which is precisely
// what the agent is waiting for.
func (c *deafConn) Write(p []byte) (int, error) {
	if c.deafness.on.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *deafConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

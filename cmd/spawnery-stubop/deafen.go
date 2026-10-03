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

package main

import (
	"net"
	"sync"
	"sync/atomic"
)

// deafness makes every connection go silent at a chosen moment without being
// closed: nothing is written, nothing that arrives is delivered upward, and no
// FIN or RST reaches the agent.
//
// Unlike a real black hole, the stub's kernel goes on acknowledging, so the
// agent's TCP never gives up either: only OperatorChannel's keepalive can end
// the wait, which is what this exercises. One inbound frame already parked in
// a Read may still be processed after it begins.
type deafness struct{ on atomic.Bool }

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

// Read blocks for the life of the connection once deafness has begun; an error
// would break the stream, which the agent already handles.
func (c *deafConn) Read(p []byte) (int, error) {
	if c.deafness.on.Load() {
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Read(p)
}

// Write reports success and sends nothing, ping acknowledgements included.
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

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
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// silencingProxy forwards TCP to target until silence, after which every
// connection open at that moment swallows what it reads and forwards nothing,
// the way a network that drops packets leaves a connection: established on
// both ends, and dead. Connections accepted later forward normally.
type silencingProxy struct {
	ln     net.Listener
	target string

	mu     sync.Mutex
	open   []*proxied
	closed bool
}

type proxied struct {
	client, server net.Conn
	mu             sync.Mutex
	silent         bool
}

func (c *proxied) isSilent() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.silent
}

func newSilencingProxy(t *testing.T, target string) *silencingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &silencingProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(p.close)
	return p
}

func (p *silencingProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		c := &proxied{client: client, server: server}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			return
		}
		p.open = append(p.open, c)
		p.mu.Unlock()
		go pipe(c, client, server)
		go pipe(c, server, client)
	}
}

func pipe(c *proxied, from, to net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := from.Read(buf)
		if n > 0 && !c.isSilent() {
			if _, werr := to.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *silencingProxy) silence() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.open {
		c.mu.Lock()
		c.silent = true
		c.mu.Unlock()
	}
}

func (p *silencingProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	_ = p.ln.Close()
	for _, c := range p.open {
		_ = c.client.Close()
		_ = c.server.Close()
	}
}

func TestS3RecoversFromAConnectionThatWentSilent(t *testing.T) {
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"0123abcd"`)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	t.Cleanup(backend.Close)

	proxy := newSilencingProxy(t, backend.Listener.Addr().String())
	roots := x509.NewCertPool()
	roots.AddCert(backend.Certificate())
	s, err := newS3Store(S3Config{
		Endpoint: "https://" + proxy.ln.Addr().String(), Region: "fsn1", Bucket: "worlds",
		AccessKey: "a", SecretKey: "b",
	}, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}

	head := func(d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		_, err := s.Head(ctx, "manifest.json")
		return err
	}

	if err := head(5 * time.Second); err != nil {
		t.Fatalf("first request through a healthy network: %v", err)
	}
	proxy.silence()
	if err := head(time.Second); err == nil {
		t.Fatal("a request over the silenced connection succeeded; the proxy forwards nothing there")
	}
	if err := head(5 * time.Second); err != nil {
		t.Fatalf("the network is back, but the store still fails: %v. "+
			"A request that gives up must not leave its connection to the next one.", err)
	}
}

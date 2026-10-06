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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, path, ifMatch, ifNoneMatch string
}

func fakeS3(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*S3Store, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recorded{r.Method, r.URL.Path, r.Header.Get("If-Match"), r.Header.Get("If-None-Match")})
		mu.Unlock()
		w.Header().Set("Date", "Tue, 06 Oct 2026 12:00:00 GMT")
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	s, err := NewS3Store(S3Config{Endpoint: srv.URL, Region: "fsn1", Bucket: "worlds", AccessKey: "a", SecretKey: "b"})
	if err != nil {
		t.Fatal(err)
	}
	return s, &reqs
}

func TestS3PutSendsABareIfMatch(t *testing.T) {
	s, reqs := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"0123abcd"`)
	})
	info, err := s.Put(context.Background(), "k", bytes.NewReader([]byte("x")), PutCondition{IfMatch: "cafe"})
	if err != nil {
		t.Fatal(err)
	}
	if (*reqs)[0].ifMatch != "cafe" {
		t.Fatalf("If-Match = %q, want the bare ETag", (*reqs)[0].ifMatch)
	}
	if info.ETag != "0123abcd" {
		t.Fatalf("ETag = %q, want it without quotes", info.ETag)
	}
	if want := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC); !info.Date.Equal(want) {
		t.Fatalf("Date = %v, want the response's Date header", info.Date)
	}
}

func TestS3PutCreateOnlySendsIfNoneMatchStar(t *testing.T) {
	s, reqs := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"e"`)
	})
	if _, err := s.Put(context.Background(), "k", bytes.NewReader(nil), PutCondition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if (*reqs)[0].ifNoneMatch != "*" {
		t.Fatalf("If-None-Match = %q", (*reqs)[0].ifNoneMatch)
	}
}

func TestS3MapsStatusCodes(t *testing.T) {
	s, _ := fakeS3(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusPreconditionFailed)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
		}
	})
	if _, err := s.Put(context.Background(), "k", bytes.NewReader(nil), PutCondition{IfMatch: "x"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("412: err = %v", err)
	}
	if _, err := s.Head(context.Background(), "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 Head: err = %v", err)
	}
	if _, _, err := s.Get(context.Background(), "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 Get: err = %v", err)
	}
}

func TestS3ConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"WORLDSYNC_ENDPOINT": "https://s3.example", "WORLDSYNC_REGION": "r",
		"WORLDSYNC_BUCKET": "b", "WORLDSYNC_PREFIX": "p",
		"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "s",
	}
	cfg, prefix, err := S3ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || cfg.Bucket != "b" || prefix != "p" {
		t.Fatalf("cfg=%+v prefix=%q err=%v", cfg, prefix, err)
	}
	delete(env, "WORLDSYNC_BUCKET")
	if _, _, err := S3ConfigFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "WORLDSYNC_BUCKET") {
		t.Fatalf("missing bucket: err = %v", err)
	}
}

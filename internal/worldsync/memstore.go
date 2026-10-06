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
	"crypto/md5"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is a Store in memory, for tests. Its ETags are bare MD5 hex, as
// a single-part S3 upload's are.
type MemStore struct {
	mu      sync.Mutex
	clock   func() time.Time
	objects map[string][]byte
	outage  error
}

func NewMemStore(clock func() time.Time) *MemStore {
	return &MemStore{clock: clock, objects: map[string][]byte{}}
}

func (m *MemStore) SetOutage(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outage = err
}

func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func etagOf(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func (m *MemStore) info(b []byte) ObjectInfo {
	return ObjectInfo{ETag: etagOf(b), Size: int64(len(b)), Date: m.clock()}
}

func (m *MemStore) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return nil, ObjectInfo{}, m.outage
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), m.info(b), nil
}

func (m *MemStore) Head(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return ObjectInfo{}, m.outage
	}
	b, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return m.info(b), nil
}

func (m *MemStore) Put(_ context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return ObjectInfo{}, m.outage
	}
	old, exists := m.objects[key]
	if cond.IfNoneMatch && exists {
		return ObjectInfo{}, ErrPrecondition
	}
	if cond.IfMatch != "" && (!exists || etagOf(old) != cond.IfMatch) {
		return ObjectInfo{}, ErrPrecondition
	}
	m.objects[key] = b
	return m.info(b), nil
}

func (m *MemStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return m.outage
	}
	delete(m.objects, key)
	return nil
}

func (m *MemStore) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outage != nil {
		return nil, m.outage
	}
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

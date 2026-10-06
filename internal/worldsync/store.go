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

// Package worldsync keeps the worlds of an OnDemand group in an S3 bucket
// with a copy on the node that runs them; see
// docs/superpowers/specs/2026-10-06-object-store-worlds-design.md.
package worldsync

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrNotFound     = errors.New("worldsync: no such object")
	ErrPrecondition = errors.New("worldsync: precondition failed")
)

// ObjectInfo describes an object as one response saw it. ETag carries no
// quotes. Date is the store's clock at that response and LastModified the
// store's clock when the object was written; both are the store's, the only
// clock all nodes share.
type ObjectInfo struct {
	ETag         string
	Size         int64
	Date         time.Time
	LastModified time.Time
}

// PutCondition: IfNoneMatch creates only; a non-empty IfMatch replaces only
// the object with that ETag.
type PutCondition struct {
	IfNoneMatch bool
	IfMatch     string
}

type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error)
	// Delete of a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List returns full keys under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
}

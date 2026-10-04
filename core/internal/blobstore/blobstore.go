// Package blobstore is where model weights live.
//
// Weights are the one thing Fleet must never put in a database: a 70B model is
// 140 GiB of blobs, and what an operator needs from storage is a prefix, not
// rows. The interface is deliberately small — put, get, list, delete, stats —
// because everything above it (the registry, the puller, the deployment
// planner) should not care whether the bytes live in an object store or a
// directory on disk.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned when a key does not exist. Callers translate it into
// a 404 rather than letting a backend-specific error escape.
var ErrNotFound = errors.New("blobstore: not found")

// Object is one stored blob.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Info describes a bucket, for the console's storage card.
//
// The tags are the API contract, and they are camelCase because every other
// type on the control plane's wire is: registry.Model is sizeBytes, Cluster is
// nodeCount. Go's default field names are neither camelCase nor snake_case, so
// leaving the tags off is how a working backend ends up rendered as
// "unreachable" with an empty bucket in the console.
//
// (The gateway's /fleet/status is snake_case, which is a separate
// inconsistency in the other direction. Worth unifying; not worth churning
// every console type over before the second API exists.)
type Info struct {
	Endpoint    string `json:"endpoint"`
	Bucket      string `json:"bucket"`
	Region      string `json:"region"`
	Reachable   bool   `json:"reachable"`
	UsedBytes   int64  `json:"usedBytes"`
	ObjectCount int64  `json:"objectCount"`
	Message     string `json:"message,omitempty"`
}

// Store is the blob backend.
type Store interface {
	// Put writes a reader to key. The reader is not closed.
	Put(ctx context.Context, key string, r io.Reader, size int64) (Object, error)
	// Get returns a reader. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, Object, error)
	// List returns objects under a prefix, in key order. The store is expected
	// to treat prefix as a plain string prefix, not a directory, because S3
	// has no directories.
	List(ctx context.Context, prefix string, limit int) ([]Object, error)
	// Delete removes a key. Removing a key that is not there is not an error:
	// a retried delete must be safe.
	Delete(ctx context.Context, key string) error
	// Info reports bucket health and usage. A backend that cannot answer must
	// return an Info with Reachable false and a message, not an error, because
	// the console shows this as a card and a missing card is less useful than
	// an unhealthy one.
	Info(ctx context.Context) Info
	// EnsureBucket creates the bucket if it does not exist.
	EnsureBucket(ctx context.Context) error
}

// ModelPrefix is the key prefix for one model's weights.
//
// The layout is models/<owner>/<name>/<revision>/<file>. Pinning the revision
// into the path is what makes a deployment reproducible: a later pull of the
// same repository at a different revision writes elsewhere and leaves the
// running replica alone.
type ModelPrefix string

// Prefix builds the key prefix for one model's weights.
func Prefix(owner, name, revision string) ModelPrefix {
	return ModelPrefix(strings.Join([]string{"models", owner, name, revision}, "/"))
}

// Key is the object key for one file inside the prefix.
func (p ModelPrefix) Key(file string) string { return string(p) + "/" + file }

// String renders the prefix, e.g. models/Qwen/Qwen2.5-7B-Instruct/main.
func (p ModelPrefix) String() string { return string(p) }

// ParsePrefix splits a stored prefix back into its parts, which is how a
// registry entry recovers the owner and name from the path it was given.
func ParsePrefix(prefix string) (owner, name, revision string, ok bool) {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) != 4 || parts[0] != "models" {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}

// TotalSize adds up a set of objects. The puller uses it to report progress
// against a total the Hub declared rather than one it is discovering.
func TotalSize(objs []Object) int64 {
	var n int64
	for _, o := range objs {
		n += o.Size
	}
	return n
}

// SortByKey orders objects by key. S3 returns them in lexicographic order
// already; a filesystem does not, and the console shows progress that assumes
// a stable order.
func SortByKey(objs []Object) {
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
}

// validKey rejects keys that would escape the bucket namespace. S3 keys are
// opaque, so a caller could otherwise create an object that shadows the whole
// models/ tree.
func validKey(key string) error {
	if key == "" {
		return fmt.Errorf("blobstore: empty key")
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return fmt.Errorf("blobstore: invalid key %q", key)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("blobstore: control character in key %q", key)
		}
	}
	return nil
}

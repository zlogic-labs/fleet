// Package hub downloads model repositories and uploads them to object storage.
//
// Fleet does not stream weights through a Kubernetes node. A 70B model is
// 140 GiB; pulling it per replica, per node, per restart is what turns a model
// deployment into a data-movement problem. Instead the control plane pulls once
// into object storage, and a deployment mounts that prefix. This package is
// that once.
package hub

import (
	"context"
	"io"
)

// File is one file in a repository.
type File struct {
	// Path is relative to the repository root, e.g. model-00001-of-00004.safetensors.
	Path string
	Size int64
	// URL is the direct download link, already resolved for the revision.
	URL string
	// LFS is true for files stored in git-lfs, which is every weight file.
	LFS bool
}

// Repo is a repository at a specific revision.
type Repo struct {
	Owner string
	Name  string
	// Revision is a commit hash, a branch, or a tag. Callers that need
	// reproducibility resolve it first.
	Revision string
	Files    []File
	// TotalBytes is the sum of every file's size.
	TotalBytes int64
	// Resolved is the commit hash the revision pointed at.
	Resolved string
}

// Ref renders "owner/name@revision".
func (r Repo) Ref() string { return r.Owner + "/" + r.Name + "@" + r.Resolved }

// Hub is a model repository client.
type Hub interface {
	// Resolve fetches the file list for a repository at a revision. A
	// branch or tag is resolved to a commit hash so that everything
	// downstream can pin to it.
	Resolve(ctx context.Context, owner, name, revision string) (Repo, error)
	// Open streams a file's contents. The caller closes the reader.
	Open(ctx context.Context, file File) (io.ReadCloser, int64, error)
}

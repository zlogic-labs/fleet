package blobstore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FS is a filesystem-backed Store.
//
// It exists for development and for the single-node install, where an operator
// has one machine and no object store. It is not a production backend: it has
// no concurrency control, so two engines mounting the same directory while a
// pull is writing into it is undefined.
type FS struct {
	root string
}

func NewFS(root string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, err
	}
	return &FS{root: abs}, nil
}

var _ Store = (*FS)(nil)

func (f *FS) EnsureBucket(ctx context.Context) error { return nil }

func (f *FS) path(key string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	// Join and then verify containment, rather than trusting validKey alone:
	// this is the boundary that stops a key from writing outside the root.
	full := filepath.Join(f.root, filepath.FromSlash(key))
	rel, err := filepath.Rel(f.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", os.ErrPermission
	}
	return full, nil
}

func (f *FS) Put(_ context.Context, key string, r io.Reader, _ int64) (Object, error) {
	full, err := f.path(key)
	if err != nil {
		return Object{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return Object{}, err
	}
	fh, err := os.Create(full)
	if err != nil {
		return Object{}, err
	}
	defer fh.Close()

	n, err := io.Copy(fh, r)
	if err != nil {
		return Object{}, err
	}
	return Object{Key: key, Size: n, LastModified: time.Now()}, nil
}

func (f *FS) Get(_ context.Context, key string) (io.ReadCloser, Object, error) {
	full, err := f.path(key)
	if err != nil {
		return nil, Object{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Object{}, ErrNotFound
		}
		return nil, Object{}, err
	}
	fh, err := os.Open(full)
	if err != nil {
		return nil, Object{}, err
	}
	return fh, Object{Key: key, Size: info.Size(), LastModified: info.ModTime()}, nil
}

func (f *FS) List(_ context.Context, prefix string, limit int) ([]Object, error) {
	base := f.root
	if prefix != "" {
		p, err := f.path(strings.TrimSuffix(prefix, "/"))
		if err != nil {
			return nil, err
		}
		// A prefix is not a directory: list the parent and filter, so that
		// "models/a/b" also matches "models/a/bb/...".
		base = filepath.Dir(p)
	}

	var out []Object
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(f.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		out = append(out, Object{Key: key, Size: info.Size(), LastModified: info.ModTime()})
		if limit > 0 && len(out) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	SortByKey(out)
	return out, nil
}

func (f *FS) Delete(_ context.Context, key string) error {
	full, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (f *FS) Info(_ context.Context) Info {
	info := Info{Endpoint: "file://" + f.root, Bucket: "(local filesystem)", Reachable: true}
	objs, err := f.List(context.Background(), "", 0)
	if err != nil {
		info.Reachable = false
		info.Message = err.Error()
		return info
	}
	info.UsedBytes = TotalSize(objs)
	info.ObjectCount = int64(len(objs))
	return info
}

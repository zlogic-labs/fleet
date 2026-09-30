package apiserver

import (
	"context"
	"fmt"
	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"sync"
)

// fetchAll downloads every file, a few at a time.
//
// Each file is streamed straight into storage rather than buffered: a 4 GiB
// shard does not fit in the memory a control plane should be using, and
// buffering it here would make the failure mode "the pod gets OOM-killed at
// 80% of a pull" instead of a resumable, reportable error.
func (p *Puller) fetchAll(ctx context.Context, h hub.Hub, repo hub.Repo, prefix blobstore.ModelPrefix, progress func(string, int64)) error {
	limit := p.FileConcurrency
	if limit < 1 {
		limit = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(repo.Files))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for _, file := range repo.Files {
		select {
		case <-ctx.Done():
			// Stop launching new work but let what is running settle, so the
			// error reported is the real one and not a derived cancellation.
			wg.Wait()
			return firstErr(errs)
		default:
		}

		file := file
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := p.fetchOne(ctx, h, prefix, file); err != nil {
				errs <- err
				cancel()
				return
			}
			progress(file.Path, file.Size)
		}()
	}

	wg.Wait()
	return firstErr(errs)
}

func (p *Puller) fetchOne(ctx context.Context, h hub.Hub, prefix blobstore.ModelPrefix, file hub.File) error {
	body, _, err := h.Open(ctx, file)
	if err != nil {
		return fmt.Errorf("%s: %w", file.Path, err)
	}
	defer body.Close()

	key := prefix.Key(file.Path)
	if _, err := p.Blobs.Put(ctx, key, body, file.Size); err != nil {
		return fmt.Errorf("%s: storing: %w", file.Path, err)
	}
	return nil
}

func firstErr(errs chan error) error {
	for {
		select {
		case err := <-errs:
			if err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

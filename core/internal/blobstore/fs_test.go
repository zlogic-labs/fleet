package blobstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFS(t *testing.T) *FS {
	t.Helper()
	f, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return f
}

func put(t *testing.T, f *FS, key, body string) {
	t.Helper()
	if _, err := f.Put(context.Background(), key, strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// A prefix that has never been written under is empty, not an error.
//
// This was found by asking whether a model was stored before downloading it --
// the ordinary question -- and getting "The system cannot find the path
// specified" back. Against S3 the same call returns an empty list, so the answer
// used to depend on which backend was configured.
func TestListingAPrefixWithNothingUnderItIsEmpty(t *testing.T) {
	f := newFS(t)
	put(t, f, "models/other/file.gguf", "x")

	objs, err := f.List(context.Background(), "models/absent", 0)
	if err != nil {
		t.Fatalf("List on an untouched prefix: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("want no objects, got %d: %+v", len(objs), objs)
	}
}

func TestListingADeepPrefixWithNothingUnderItIsEmpty(t *testing.T) {
	// The prefix's parent exists but the prefix itself does not, which is the
	// case that reaches the walk rather than the root.
	f := newFS(t)
	put(t, f, "models/a/file.gguf", "x")

	objs, err := f.List(context.Background(), "models/a/deeper/still", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("want no objects, got %d", len(objs))
	}
}

func TestAPrefixAlsoMatchesItsLongerNames(t *testing.T) {
	// Listing from the parent directory is deliberate: "a/b" must match
	// "a/bb/file" too, which it would not if the prefix were treated as a
	// directory to walk into.
	f := newFS(t)
	put(t, f, "models/a/b/file.gguf", "x")
	put(t, f, "models/a/bb/other.gguf", "y")

	objs, err := f.List(context.Background(), "models/a/b", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 {
		keys := make([]string, 0, len(objs))
		for _, o := range objs {
			keys = append(keys, o.Key)
		}
		t.Fatalf("want 2 objects, got %d: %v", len(objs), keys)
	}
}

func TestKeysAreReportedWithSlashesWhateverTheHostSeparatorIs(t *testing.T) {
	// The store hands out slash-separated keys because that is what S3 does and
	// what the registry stores. On a host whose separator is a backslash, a key
	// read back with backslashes is a key no caller can use to Get it again.
	f := newFS(t)
	put(t, f, "models/acme/qwen/file.gguf", "x")

	objs, err := f.List(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("want 1 object, got %d", len(objs))
	}
	if strings.ContainsRune(objs[0].Key, '\\') {
		t.Fatalf("key %q carries the host separator", objs[0].Key)
	}
	if objs[0].Key != "models/acme/qwen/file.gguf" {
		t.Fatalf("key = %q", objs[0].Key)
	}
}

func TestGetAndDeleteAgreeOnTheKeyListReports(t *testing.T) {
	f := newFS(t)
	put(t, f, "models/acme/qwen/file.gguf", "hello")

	objs, err := f.List(context.Background(), "models/acme", 0)
	if err != nil || len(objs) != 1 {
		t.Fatalf("List: %v, %d objects", err, len(objs))
	}
	rc, obj, err := f.Get(context.Background(), objs[0].Key)
	if err != nil {
		t.Fatalf("Get with the key List reported: %v", err)
	}
	defer rc.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if buf.String() != "hello" {
		t.Fatalf("body = %q", buf.String())
	}
	if obj.Size != 5 {
		t.Fatalf("size = %d, want 5", obj.Size)
	}
}

func TestAMissingKeyIsNotFoundRatherThanAnError(t *testing.T) {
	f := newFS(t)
	_, _, err := f.Get(context.Background(), "models/nothing.gguf")
	if err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestDeletingSomethingAbsentIsNotAnError(t *testing.T) {
	f := newFS(t)
	if err := f.Delete(context.Background(), "models/nothing.gguf"); err != nil {
		t.Fatalf("Delete on a missing key: %v", err)
	}
}

func TestAKeyCannotEscapeTheRoot(t *testing.T) {
	f := newFS(t)
	put(t, f, "models/ok", "x")

	for _, key := range []string{"../outside", "models/../../outside", "models/ok/../../../etc"} {
		if _, err := f.Put(context.Background(), key, strings.NewReader("x"), 1); err == nil {
			t.Fatalf("Put(%q) was allowed", key)
		}
	}
	// And nothing landed outside.
	parent := filepath.Dir(f.root)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(f.root)) && e.Name() != filepath.Base(f.root) {
			t.Fatalf("a sibling directory %q was created", e.Name())
		}
	}
}

func TestLimitStopsTheWalk(t *testing.T) {
	f := newFS(t)
	for _, k := range []string{"models/a/1", "models/a/2", "models/a/3", "models/a/4"} {
		put(t, f, k, "x")
	}
	objs, err := f.List(context.Background(), "models/a", 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("want 2 objects, got %d", len(objs))
	}
}

func TestInfoCountsWhatIsThere(t *testing.T) {
	f := newFS(t)
	put(t, f, "models/a", "12345")
	info := f.Info(context.Background())
	if !info.Reachable {
		t.Fatalf("not reachable: %s", info.Message)
	}
	if info.ObjectCount != 1 {
		t.Fatalf("ObjectCount = %d, want 1", info.ObjectCount)
	}
	if info.UsedBytes != 5 {
		t.Fatalf("UsedBytes = %d, want 5", info.UsedBytes)
	}
}

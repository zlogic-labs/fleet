package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// realTree is the first two entries of a live response from
// https://huggingface.co/api/models/<repo>/tree/main?recursive=true, taken
// verbatim including the fields Fleet does not use. A hand-written fixture here
// would only prove the decoder agrees with the fixture.
const realTree = `[
  {"type":"file","oid":"2b70b6b2d5c38a7b88d29ab4370e0576de868a63","size":3193,"path":".gitattributes"},
  {"type":"directory","oid":"a1b2c3","size":0,"path":"onnx"},
  {"type":"file","oid":"deadbeef","size":130,"path":"README.md",
   "lfs":{"oid":"cafe","size":4096,"pointerSize":135}}
]`

func treeServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tree/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"41ba88dbac95fed2528c92514c131d73eb5a174b"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestTheTreeIsAnArrayNotAnObject(t *testing.T) {
	// This is the regression: the response is a bare array. Decoding it as
	// {"siblings": [...]} fails with "cannot unmarshal array into Go value",
	// which is what every real pull reported before this was fixed.
	h := &HTTP{BaseURL: treeServer(t, realTree).URL}
	repo, err := h.Resolve(context.Background(), "bartowski", "Qwen2.5-0.5B-Instruct-GGUF", "main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(repo.Files) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(repo.Files), repo.Files)
	}
}

func TestDirectoryEntriesAreNotDownloads(t *testing.T) {
	h := &HTTP{BaseURL: treeServer(t, realTree).URL}
	repo, err := h.Resolve(context.Background(), "bartowski", "x", "main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, f := range repo.Files {
		if f.Path == "onnx" {
			t.Fatal("a directory was listed as a downloadable file")
		}
		if f.Size == 0 {
			t.Fatalf("%s has no size, which is what a directory looks like", f.Path)
		}
	}
}

func TestTheLfsSizeWinsOverThePointerSize(t *testing.T) {
	// For an lfs entry `size` is the pointer file and `lfs.size` is the real
	// object. Progress driven by the pointer reports a 4096-byte download as 130.
	h := &HTTP{BaseURL: treeServer(t, realTree).URL}
	repo, err := h.Resolve(context.Background(), "bartowski", "x", "main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, f := range repo.Files {
		if f.Path != "README.md" {
			continue
		}
		if f.Size != 4096 {
			t.Fatalf("README.md size = %d, want the lfs figure 4096", f.Size)
		}
		if !f.LFS {
			t.Fatal("an lfs entry did not report itself as one")
		}
		return
	}
	t.Fatal("README.md missing from the listing")
}

func TestTotalBytesIsTheSumOfWhatIsListed(t *testing.T) {
	h := &HTTP{BaseURL: treeServer(t, realTree).URL}
	repo, err := h.Resolve(context.Background(), "bartowski", "x", "main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// 3193 + 4096, with the directory's zero deliberately not counted.
	if repo.TotalBytes != 7289 {
		t.Fatalf("TotalBytes = %d, want 7289", repo.TotalBytes)
	}
}

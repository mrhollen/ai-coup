package main

import (
	"bytes"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	couphttp "forgejo.lan/mrhollen/ai-coup/infrastructure/http"
)

// TestStaticClientServedIndependentOfCWD proves the go:embed fix (ui-spec U1):
// when the process working directory is NOT the repository root — a temp dir
// with no web/ at all — "/" still serves index.html from the embedded FS,
// byte-identical to the on-disk file, and unknown paths keep the §11.1 JSON
// not_found envelope exactly as before (no net/http text 404 page).
func TestStaticClientServedIndependentOfCWD(t *testing.T) {
	wantIndex, err := os.ReadFile(filepath.Join("web", "index.html")) // default CWD = repo root
	if err != nil {
		t.Fatalf("read web/index.html from the repo root: %v", err)
	}

	root := t.TempDir()
	t.Chdir(root) // CWD now has NO web/ — success below proves no CWD dependency

	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	srv := couphttp.NewServer(nil, nil, time.Second, false, "heuristic", slog.New(slog.DiscardHandler), webRoot)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		t.Fatalf("read GET / body: %v", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200 (process CWD is %s)", resp.StatusCode, root)
	}
	if !bytes.Equal(body, wantIndex) {
		t.Fatalf("GET / body differs from the embedded index.html (%d vs %d bytes)", len(body), len(wantIndex))
	}

	// Miss behavior is unchanged: §11.1 JSON envelope, not a text 404 page.
	resp2, err := http.Get(ts.URL + "/definitely-not-a-file")
	if err != nil {
		t.Fatalf("GET /definitely-not-a-file: %v", err)
	}
	body2, rerr := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if rerr != nil {
		t.Fatalf("read miss body: %v", rerr)
	}
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("miss status = %d, want 404", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("miss content type = %q, want application/json", ct)
	}
	if !strings.Contains(string(body2), `"code":"not_found"`) {
		t.Fatalf("miss body = %s, want the §11.1 not_found envelope", body2)
	}
}

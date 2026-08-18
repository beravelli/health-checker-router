package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// bundleDir writes a minimal built-frontend layout to a temp dir.
func bundleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<!doctype html><title>console</title>")
	write(filepath.Join("assets", "index-abc123.js"), "console.log(1)")
	return dir
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestStaticServesIndexAndAssets(t *testing.T) {
	h := staticHandler(bundleDir(t))

	if rec := get(t, h, "/"); rec.Code != http.StatusOK {
		t.Errorf("GET / = %d, want 200", rec.Code)
	}
	rec := get(t, h, "/assets/index-abc123.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asset = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q, want immutable", cc)
	}
}

// TestStaticSPAFallback covers client-side routes, which are not files on disk.
func TestStaticSPAFallback(t *testing.T) {
	h := staticHandler(bundleDir(t))

	for _, path := range []string{"/rollouts", "/incidents", "/deep/link/here"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (index fallback)", path, rec.Code)
		}
		if body := rec.Body.String(); body != "<!doctype html><title>console</title>" {
			t.Errorf("GET %s served %q, want index.html", path, body)
		}
	}

	// index.html must not be cached, or a deploy never reaches the browser.
	if cc := get(t, h, "/rollouts").Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index Cache-Control = %q, want no-cache", cc)
	}
}

// TestStaticNeverShadowsAPI guards the mux precedence: the static handler is
// registered at "/" and would otherwise swallow unmatched API paths, turning a
// genuine 404 into an HTML page the frontend cannot parse.
func TestStaticNeverShadowsAPI(t *testing.T) {
	h := staticHandler(bundleDir(t))
	if rec := get(t, h, "/api/does-not-exist"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/... = %d, want 404 rather than the SPA fallback", rec.Code)
	}
}

func TestStaticMissingBundleIs404(t *testing.T) {
	h := staticHandler(filepath.Join(t.TempDir(), "nonexistent"))
	if rec := get(t, h, "/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET / with no bundle = %d, want 404", rec.Code)
	}
}

func TestStaticRejectsTraversal(t *testing.T) {
	dir := bundleDir(t)
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not serve me"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := staticHandler(dir)
	for _, path := range []string{"/../secret.txt", "/../../secret.txt", "/assets/../../secret.txt"} {
		rec := get(t, h, path)
		if body := rec.Body.String(); body == "do not serve me" {
			t.Errorf("GET %s escaped the bundle directory", path)
		}
	}
}

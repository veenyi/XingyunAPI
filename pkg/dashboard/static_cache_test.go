package dashboard

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func staticTestHandler() *Handler {
	fsys := fstest.MapFS{
		"index.html":            {Data: []byte("<html><body>spa</body></html>")},
		"assets/app-AbCd1234.js": {Data: []byte("console.log(1)")},
		"assets/st-UiVw9876.css": {Data: []byte("body{}")},
		"logo.png":              {Data: []byte("png")},
		"terminal/index.html":   {Data: []byte("<html>term</html>")},
	}
	return NewHandler(nil, fsys, nil)
}

func serveReq(h *Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeStatic(w, r)
	return w
}

func TestServeStaticCacheDiscipline(t *testing.T) {
	h := staticTestHandler()

	t.Run("hashed_asset_immutable", func(t *testing.T) {
		w := serveReq(h, "GET", "/assets/app-AbCd1234.js", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/javascript") {
			t.Errorf("Content-Type = %q, want text/javascript", ct)
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("Cache-Control = %q, want immutable", cc)
		}
	})

	t.Run("missing_asset_404_no_store_never_html", func(t *testing.T) {
		w := serveReq(h, "GET", "/assets/missing-Qq1W2E3r.js", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, must not be text/html", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
		if strings.Contains(w.Body.String(), "<html") {
			t.Error("body contains the SPA html — this is what poisons browser caches")
		}
	})

	t.Run("missing_non_asset_still_falls_through", func(t *testing.T) {
		w := serveReq(h, "GET", "/logo.png", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for existing root file", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", cc)
		}
	})

	t.Run("index_no_cache", func(t *testing.T) {
		w := serveReq(h, "GET", "/", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", cc)
		}
	})

	t.Run("spa_route_fallback_no_cache", func(t *testing.T) {
		w := serveReq(h, "GET", "/accounts", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", cc)
		}
	})

	t.Run("terminal_page_no_cache", func(t *testing.T) {
		w := serveReq(h, "GET", "/terminal/index.html", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", cc)
		}
	})
}

func TestServeStaticKnownAPIPathsUnchanged(t *testing.T) {
	h := staticTestHandler()

	w := serveReq(h, "GET", "/models", map[string]string{"Accept": "application/json"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("sdk /models status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	w = serveReq(h, "GET", "/models", map[string]string{"Accept": "text/html,application/xhtml+xml"})
	if w.Code != http.StatusOK {
		t.Fatalf("browser /models status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

// realStaticFS opens the actual embedded SPA sources so the guards below run
// against the bytes that ship in the binary, not a synthetic fixture.
func realStaticFS(t *testing.T) fs.FS {
	t.Helper()
	fsys := os.DirFS(filepath.Join("..", "..", "cmd", "JoyCode2Api", "static"))
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		t.Fatalf("static dir not found: %v", err)
	}
	return fsys
}

var assetRefRe = regexp.MustCompile(`[A-Za-z0-9]+[A-Za-z0-9._-]*-[A-Za-z0-9_-]{8}\.(?:js|css)`)

// TestReferencedAssetsExist fails if any chunk references a hashed asset that
// is missing from the embedded files (a renamed or dropped file once shipped
// as a 404 and later, before the 404 fix, as poisoned HTML).
func TestReferencedAssetsExist(t *testing.T) {
	fsys := realStaticFS(t)
	refs := map[string]bool{}
	entries, err := fs.ReadDir(fsys, "assets")
	if err != nil {
		t.Fatalf("read assets dir: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names["assets/" + e.Name()] = true
	}
	idx, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	for _, m := range assetRefRe.FindAllString(string(idx), -1) {
		refs[m] = true
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		body, err := fs.ReadFile(fsys, "assets/"+e.Name())
		if err != nil {
			t.Fatalf("read asset: %v", err)
		}
		for _, m := range assetRefRe.FindAllString(string(body), -1) {
			refs[m] = true
		}
	}
	var missing []string
	for ref := range refs {
		if !names["assets/"+ref] {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("referenced assets missing from static/assets: %v", missing)
	}
}

// TestAssetsAreNotHTML fails if any asset file actually contains a saved HTML
// fallback page instead of its real content.
func TestAssetsAreNotHTML(t *testing.T) {
	fsys := realStaticFS(t)
	entries, err := fs.ReadDir(fsys, "assets")
	if err != nil {
		t.Fatalf("read assets dir: %v", err)
	}
	var bad []string
	for _, e := range entries {
		body, err := fs.ReadFile(fsys, "assets/"+e.Name())
		if err != nil {
			t.Fatalf("read asset: %v", err)
		}
		head := strings.ToLower(string(body))
		if len(head) >= 15 && (strings.HasPrefix(head, "<!doctype") || strings.HasPrefix(head, "<html")) {
			bad = append(bad, e.Name())
		}
	}
	if len(bad) > 0 {
		t.Fatalf("asset files contain saved HTML instead of real content: %v", bad)
	}
}

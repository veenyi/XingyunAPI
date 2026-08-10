package dashboard

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/keepalive"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

func setupTestHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Create minimal static FS for tests
	staticDir := filepath.Join(dir, "static")
	os.MkdirAll(staticDir, 0755)
	os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<html>test</html>"), 0644)
	os.MkdirAll(filepath.Join(staticDir, "assets"), 0755)
	os.WriteFile(filepath.Join(staticDir, "assets", "test.js"), []byte("console.log(1)"), 0644)

	subFS := os.DirFS(staticDir)
	k := keepalive.NewKeeper(s, time.Hour)
	h := NewHandler(s, subFS, k)
	return h, s
}

func makeRequest(t *testing.T, method, path string, body interface{}) *http.Request {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	return req
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode json: %v, body: %s", err, w.Body.String())
	}
	return m
}

// --- Health ---

func TestHandleHealth(t *testing.T) {
	h, _ := setupTestHandler(t)
	h.Version = "9.9.9-test"
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	m := decodeJSON(t, w)
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok", m["status"])
	}
	if _, ok := m["accounts"]; !ok {
		t.Error("missing accounts field")
	}
	if m["version"] != "9.9.9-test" {
		t.Errorf("version = %v, want 9.9.9-test (dynamic version not wired)", m["version"])
	}
}

func TestHandleHealthCORS(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("OPTIONS", "/api/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if origin := w.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("CORS origin = %q, want *", origin)
	}
}

// --- Accounts ---

func TestHandleListAccountsEmpty(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/accounts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	m := decodeJSON(t, w)
	accounts, ok := m["accounts"].([]interface{})
	if !ok {
		t.Fatalf("accounts is not a list: %T", m["accounts"])
	}
	if len(accounts) != 0 {
		t.Errorf("accounts len = %d, want 0", len(accounts))
	}
}

func TestHandleAddAndListAccounts(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Add account
	req := makeRequest(t, "POST", "/api/accounts", map[string]interface{}{
		"user_id": "test-user",
		"pt_key":  "test-pt",
		"nickname": "TestNick",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("add status = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	m := decodeJSON(t, w)
	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}

	// List accounts
	req = httptest.NewRequest("GET", "/api/accounts", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	m = decodeJSON(t, w)
	accounts := m["accounts"].([]interface{})
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(accounts))
	}
	acc := accounts[0].(map[string]interface{})
	if acc["user_id"] != "test-user" {
		t.Errorf("user_id = %v, want test-user", acc["user_id"])
	}
	if acc["nickname"] != "TestNick" {
		t.Errorf("nickname = %v, want TestNick", acc["nickname"])
	}
}

func TestHandleAddAccountMissingFields(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := makeRequest(t, "POST", "/api/accounts", map[string]interface{}{
		"api_key": "test-key",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleRemoveAccount(t *testing.T) {
	h, s := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	s.AddAccount("del-key", "pt", "user", false, "")

	req := httptest.NewRequest("DELETE", "/api/accounts/del-key", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	m := decodeJSON(t, w)
	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
}

func TestHandleSetDefault(t *testing.T) {
	h, s := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	s.AddAccount("key1", "pt1", "user1", true, "")
	s.AddAccount("key2", "pt2", "user2", false, "")

	req := httptest.NewRequest("PUT", "/api/accounts/key2/default", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	a, _ := s.GetAccount("key2")
	if !a.IsDefault {
		t.Error("key2 should be default after PUT")
	}
}

func TestHandleUpdateModel(t *testing.T) {
	h, s := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	s.AddAccount("key1", "pt1", "user1", true, "")

	req := makeRequest(t, "PUT", "/api/accounts/key1/model", map[string]interface{}{
		"default_model": "GLM-5.1",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	a, _ := s.GetAccount("key1")
	if a.DefaultModel != "GLM-5.1" {
		t.Errorf("model = %q, want GLM-5.1", a.DefaultModel)
	}
}

// --- Models ---

func TestHandleModels(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/models", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	m := decodeJSON(t, w)
	models, ok := m["models"].([]interface{})
	if !ok {
		t.Fatalf("models is not a list: %T", m["models"])
	}
	if len(models) == 0 {
		t.Error("expected at least 1 model")
	}
	first := models[0].(map[string]interface{})
	if first["id"] == "" || first["name"] == "" {
		t.Error("model should have id and name")
	}
}

// --- Stats ---

func TestHandleStatsEmpty(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	m := decodeJSON(t, w)
	if m["total_requests"] != float64(0) {
		t.Errorf("total_requests = %v, want 0", m["total_requests"])
	}
}

func TestHandleStatsWithLogs(t *testing.T) {
	h, s := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	s.AddAccount("key1", "pt1", "user1", true, "")
	s.LogRequest("key1", "JoyAI-Code", "/v1/chat", true, 200, 500, "", 0, 0)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	m := decodeJSON(t, w)
	if m["total_requests"] != float64(1) {
		t.Errorf("total_requests = %v, want 1", m["total_requests"])
	}
}

// --- Settings ---

func TestHandleGetSettingsEmpty(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/settings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	m := decodeJSON(t, w)
	settings, ok := m["settings"].(map[string]interface{})
	if !ok {
		t.Fatalf("settings is not a map: %T", m["settings"])
	}
	if len(settings) != 0 {
		t.Errorf("settings len = %d, want 0", len(settings))
	}
}

func TestHandleUpdateAndGetSettings(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Update
	req := makeRequest(t, "PUT", "/api/settings", map[string]interface{}{
		"theme": "dark",
		"lang":  "zh",
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("put status = %d, want 200", w.Code)
	}

	// Get
	req = httptest.NewRequest("GET", "/api/settings", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	m := decodeJSON(t, w)
	settings := m["settings"].(map[string]interface{})
	if settings["theme"] != "dark" {
		t.Errorf("theme = %v, want dark", settings["theme"])
	}
	if settings["lang"] != "zh" {
		t.Errorf("lang = %v, want zh", settings["lang"])
	}
}

// Regression for #11: a client (e.g. an older frontend Switch) sends a raw
// JSON bool/number for a setting. The PUT must accept it and coerce to string
// instead of failing with "cannot unmarshal bool into Go value of type string".
func TestHandleUpdateSettingsCoercesNonStringValues(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Raw JSON body with bool, number, null and string values.
	body := `{"enable_request_logging":false,"max_connections":12,"theme":"dark","cleared":null}`
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("put status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/settings", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	settings := decodeJSON(t, w)["settings"].(map[string]interface{})

	if got := settings["enable_request_logging"]; got != "false" {
		t.Errorf("enable_request_logging = %v, want \"false\"", got)
	}
	if got := settings["max_connections"]; got != "12" {
		t.Errorf("max_connections = %v, want \"12\"", got)
	}
	if got := settings["theme"]; got != "dark" {
		t.Errorf("theme = %v, want \"dark\"", got)
	}
	if got := settings["cleared"]; got != "" {
		t.Errorf("cleared = %v, want \"\"", got)
	}
}

// --- Account Stats ---

func TestHandleAccountStats(t *testing.T) {
	h, s := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	s.AddAccount("key1", "pt1", "user1", true, "")
	s.LogRequest("key1", "JoyAI-Code", "/v1/chat", true, 200, 500, "", 0, 0)
	s.LogRequest("key1", "GLM-5.1", "/v1/msg", false, 200, 300, "", 0, 0)

	req := httptest.NewRequest("GET", "/api/accounts/key1/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	m := decodeJSON(t, w)
	if m["total_requests"] != float64(2) {
		t.Errorf("total_requests = %v, want 2", m["total_requests"])
	}
}

func TestHandleAccountStatsNonexistent(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/accounts/nonexistent/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- Static file serving ---

func TestServeStaticIndex(t *testing.T) {
	h, _ := setupTestHandler(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeStatic(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "test") {
		t.Errorf("expected html content, got: %s", body)
	}
}

func TestServeStaticSPAFallback(t *testing.T) {
	h, _ := setupTestHandler(t)

	// Unknown path should fallback to index.html
	req := httptest.NewRequest("GET", "/accounts", nil)
	w := httptest.NewRecorder()
	h.ServeStatic(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "test") {
		t.Errorf("expected fallback to index.html, got: %s", body)
	}
}

func TestServeStaticAsset(t *testing.T) {
	h, _ := setupTestHandler(t)

	req := httptest.NewRequest("GET", "/assets/test.js", nil)
	w := httptest.NewRecorder()
	h.ServeStatic(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "console.log") {
		t.Errorf("expected js content, got: %s", body)
	}
}

// --- Method not allowed ---

func TestMethodNotAllowed(t *testing.T) {
	h, _ := setupTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest("PATCH", "/api/accounts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 405 {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

// --- Interface check ---

var _ fs.FS = (os.DirFS(""))

// --- SPA catch-all API path interception ---

func TestServeStaticAPIPathReturnsJSON404(t *testing.T) {
	h, _ := setupTestHandler(t)

	paths := []string{
		"/chat/completions",
		"/completions",
		"/messages",
		"/models",
		"/embeddings",
		"/web-search",
		"/rerank",
		"/images/generations",
		"/audio/transcriptions",
		"/audio/translations",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("POST", path, nil)
			w := httptest.NewRecorder()
			h.ServeStatic(w, req)

			if w.Code != 404 {
				t.Errorf("status = %d, want 404 for %s", w.Code, path)
			}

			ct := w.Header().Get("Content-Type")
			if !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json for %s", ct, path)
			}

			var resp map[string]interface{}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode json for %s: %v, body: %s", path, err, w.Body.String())
			}
			errObj, ok := resp["error"].(map[string]interface{})
			if !ok {
				t.Fatalf("no error object for %s: %v", path, resp)
			}
			msg, _ := errObj["message"].(string)
			if !strings.Contains(msg, "/v1/") {
				t.Errorf("error message should mention /v1/ for %s, got: %s", path, msg)
			}
		})
	}
}

func TestServeStaticAPIPathGETAlsoReturns404(t *testing.T) {
	h, _ := setupTestHandler(t)

	req := httptest.NewRequest("GET", "/chat/completions", nil)
	w := httptest.NewRecorder()
	h.ServeStatic(w, req)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	errObj := resp["error"].(map[string]interface{})
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "GET") {
		t.Errorf("error message should include GET method, got: %s", msg)
	}
}

func TestServeStaticNonAPIPathStillFallsThrough(t *testing.T) {
	h, _ := setupTestHandler(t)

	// These paths are NOT in knownAPISet, should get SPA fallback
	paths := []string{"/accounts", "/settings", "/some-random-page", "/dashboard"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()
			h.ServeStatic(w, req)

			if w.Code != 200 {
				t.Errorf("status = %d, want 200 (SPA fallback) for %s", w.Code, path)
			}
			body := w.Body.String()
			if !strings.Contains(body, "test") {
				t.Errorf("expected SPA fallback HTML for %s, got: %s", path, body)
			}
		})
	}
}

func TestServeStaticRootPathStillWorks(t *testing.T) {
	h, _ := setupTestHandler(t)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeStatic(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "test") {
		t.Errorf("root path should serve index.html, got: %s", w.Body.String())
	}
}

func TestServeStaticAssetStillWorksAfterAPICheck(t *testing.T) {
	h, _ := setupTestHandler(t)

	req := httptest.NewRequest("GET", "/assets/test.js", nil)
	w := httptest.NewRecorder()
	h.ServeStatic(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "console.log") {
		t.Errorf("asset should still be served, got: %s", w.Body.String())
	}
}

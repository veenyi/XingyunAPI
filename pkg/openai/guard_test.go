package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuardRejectsMissingAndUnknownKey(t *testing.T) {
	srv := NewServer(nil, nil)
	srv.KeyValidator = func(key string) bool { return key == "good-key" }
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	// 无 key
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code = %d, want 401", rec.Code)
	}

	// 错 key
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: code = %d, want 401", rec.Code)
	}

	// 好 key 进到 handler（nil store 时 handleModels 走 routerFor=nil →
	// getClient→Resolver=nil→Client=nil 会让 ListModels panic；只验证非 401
	// 即可，用 recover 包裹）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("x-api-key", "good-key")
	func() {
		defer func() { _ = recover() }()
		mux.ServeHTTP(rec, req)
	}()
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("good key was rejected")
	}
}

func TestGuardAllowsHealthAndSkipsWhenUnset(t *testing.T) {
	srv := NewServer(nil, nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health without validator: code = %d, want 200", rec.Code)
	}
}

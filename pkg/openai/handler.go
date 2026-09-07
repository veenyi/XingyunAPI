package openai

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/joycode"
	"github.com/veenyi/XingyunAPI/pkg/route"
	"github.com/veenyi/XingyunAPI/pkg/store"
)

// ClientResolver returns the appropriate joycode.Client for a request.
type ClientResolver func(r *http.Request) *joycode.Client

// APIKeyValidator reports whether a /v1 request credential is accepted.
type APIKeyValidator func(key string) bool

// Server implements the OpenAI-compatible HTTP API.
type Server struct {
	Client   *joycode.Client
	Resolver ClientResolver
	// KeyValidator enforces credentials on /v1/* (except /health). When set,
	// missing or unknown keys are rejected with 401 — the reverse tunnel makes
	// loopback indistinguishable from public traffic, so there is no
	// loopback exemption.
	KeyValidator APIKeyValidator
	store        *store.Store
	// keyless/health 由 server 启动时注入（SetRouteDeps）；注入后 /v1/*
	// 的对话请求经 route.Router 做跨渠道故障转移。
	keyless route.KeylessSource
	health  *health.Registry
}

// NewServer creates a new OpenAI-compatible proxy server.
func NewServer(c *joycode.Client, s *store.Store) *Server {
	return &Server{Client: c, store: s}
}

// SetRouteDeps wires keyless channels and the shared health registry.
func (s *Server) SetRouteDeps(kl route.KeylessSource, hg *health.Registry) {
	s.keyless = kl
	s.health = hg
}

// routerFor builds a per-request Router over the shared health registry;
// returns nil when no keyless channels are wired (纯 JoyCode 请求无需路由).
func (s *Server) routerFor(r *http.Request) *route.Router {
	if s.keyless == nil {
		return nil
	}
	client := s.getClient(r)
	if client == nil {
		return nil
	}
	return &route.Router{
		Primary: client.AsProvider(),
		Keyless: s.keyless,
		Health:  s.health,
		Store:   s.store,
	}
}

func (s *Server) getClient(r *http.Request) *joycode.Client {
	if s.Resolver != nil {
		return s.Resolver(r)
	}
	return s.Client
}

// RegisterRoutes registers all OpenAI-compatible endpoints on the mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", s.guard(s.handleChat))
	mux.HandleFunc("/v1/models", s.guard(s.handleModels))
	mux.HandleFunc("/v1/web-search", s.guard(s.handleWebSearch))
	mux.HandleFunc("/v1/rerank", s.guard(s.handleRerank))
	mux.HandleFunc("/health", s.handleHealth)
}

// guard enforces the API-key requirement on /v1 endpoints.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.KeyValidator != nil && r.Method != http.MethodOptions {
			key := APIKeyOf(r)
			if !s.KeyValidator(key) {
				slog.Warn("v1 auth: rejected request", "path", r.URL.Path, "has_key", key != "")
				writeError(w, http.StatusUnauthorized, "invalid or missing API key")
				return
			}
		}
		next(w, r)
	}
}

// APIKeyOf extracts the request credential from x-api-key or Authorization.
func APIKeyOf(r *http.Request) string {
	key := r.Header.Get("x-api-key")
	if key == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			key = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	return strings.TrimSpace(key)
}

func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	w.Write(b)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "api_error"},
	})
}

func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(200)
		return false
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return false
	}
	return true
}

func requireGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(200)
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(200)
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"status": "ok", "service": "joycode-openai-proxy",
		"endpoints": []string{
			"/v1/chat/completions", "/v1/models",
			"/v1/web-search", "/v1/rerank",
		},
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	if rt := s.routerFor(r); rt != nil {
		names := route.CleanNames(rt.ModelNames())
		if len(names) > 0 {
			data := make([]map[string]interface{}, 0, len(names))
			for _, m := range names {
				data = append(data, map[string]interface{}{"id": m, "object": "model", "owned_by": "xingyun"})
			}
			writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
			return
		}
	}
	models, err := s.getClient(r).ListModels()
	if err != nil {
		slog.Error("list models upstream error", "error", err)
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, TranslateModels(models))
}

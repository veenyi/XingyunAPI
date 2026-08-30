package openai

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/route"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

// ClientResolver returns the appropriate joycode.Client for a request.
type ClientResolver func(r *http.Request) *joycode.Client

var _ provider.Chat = (*joycode.Client)(nil)

// Server implements the OpenAI-compatible HTTP API.
type Server struct {
	Client   *joycode.Client
	Keyfree  provider.Keyless
	Keyed    provider.Keyless
	// Extras 动态返回自定义渠道列表（保存后即时生效）；命中名单时直接派给对应上游。
	Extras func() []provider.Keyless
	Route  *route.Router
	Resolver ClientResolver
	store    *store.Store
}

// NewServer creates a new OpenAI-compatible proxy server.
func NewServer(c *joycode.Client, s *store.Store) *Server {
	return &Server{Client: c, store: s}
}

// extras 返回当前已启用的免登录 / 自带 Key 渠道；开关每次请求重读，改设置不必重启。
func (s *Server) extras() []provider.Keyless {
	out := make([]provider.Keyless, 0, 4)
	for _, p := range []provider.Keyless{s.Keyfree, s.Keyed} {
		if p != nil && p.Enabled() {
			out = append(out, p)
		}
	}
	if s.Extras != nil {
		for _, p := range s.Extras() {
			if p != nil && p.Enabled() {
				out = append(out, p)
			}
		}
	}
	return out
}

func (s *Server) getClient(r *http.Request) *joycode.Client {
	if s.Resolver != nil {
		return s.Resolver(r)
	}
	return s.Client
}

// RegisterRoutes registers all OpenAI-compatible endpoints on the mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/web-search", s.handleWebSearch)
	mux.HandleFunc("/v1/rerank", s.handleRerank)
	mux.HandleFunc("/health", s.handleHealth)
}

func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("writeJSON: marshal failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
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
	models, err := s.getClient(r).ListModels()
	if err != nil {
		slog.Error("list models upstream error", "error", err)
		writeError(w, 500, err.Error())
		return
	}
	// 额外渠道只有在开关打开后才出现在目录里，未安装/未开启时目录与原来一致。
	// 额外渠道的模型以 "渠道/模型" 前缀形式输出（B.AI/qwen3.8-max）：
	// 不同渠道的同名模型互不混淆，聚合 Key 直接点名前缀名即可精确路由。
	for _, p := range s.extras() {
		if free, kerr := p.ListModels(); kerr == nil {
			for _, m := range free {
				base := m.ModelID
				if base == "" {
					base = m.ChatAPIModel
				}
				if base == "" {
					base = m.Label
				}
				if base == "" {
					continue // 空模型名不拼前缀，避免产出 "渠道/" 脏条目
				}
				m.ModelID = p.Name() + "/" + m.ModelID
				m.ChatAPIModel = p.Name() + "/" + m.ChatAPIModel
				m.Label = p.Name() + "/" + m.Label
				models = append(models, m)
			}
		}
	}
	writeJSON(w, 200, TranslateModels(models))
}

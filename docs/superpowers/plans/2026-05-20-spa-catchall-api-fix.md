# Bug Fix: SPA Catch-All Intercepts API Requests Without `/v1` Prefix

> **For agentic workers:** REQUIRED SUB-SKILL: `superpowers:subagent-driven-development`
> Steps use checkbox (`- [ ]`) syntax.

**Symptom:** 当 openai SDK 使用 `base_url="http://127.0.0.1:34891"`（不带 `/v1` 后缀）时，`/chat/completions` 请求被 SPA catch-all 路由捕获，返回 HTML 而非 JSON。SDK 将 HTML 当作响应体，下游报 `AttributeError: 'str' object has no attribute 'choices'`。

**Root Cause:** `pkg/dashboard/handler.go:179` 的 `ServeStatic` 函数作为 `mux.HandleFunc("/", ...)` 注册，是 Go `http.ServeMux` 的 catch-all。所有不匹配 `/v1/*` 已注册路由的请求都会落入此 handler，包括 `/chat/completions`、`/messages` 等明显是 API 调用的路径。

**Impact:** 所有不带 `/v1` 前缀的 OpenAI/Anthropic API 路径受影响，包括 `/chat/completions`、`/messages`、`/models`、`/completions`、`/embeddings` 等。用户第一次调用就报错，且错误信息完全不相关，极难排查。

**Architecture:** 请求进入 `ServeStatic` → 检查路径是否为已知 API 端点名 → 是则返回 JSON 404 错误（含修正提示）→ 否则继续 SPA fallback。

**Tech Stack:** Go 1.22, net/http ServeMux

**Scope:** Tiny
**Risk:** Low
**Risks:** 无风险 — 仅在 `ServeStatic` 入口添加路径前检查，不影响正常 API 路由和 SPA 功能。

**Autonomy Level:** Full

---

### Task 1: 在 ServeStatic 中拦截已知 API 路径，返回 JSON 404

**Depends on:** None
**Files:**
- Modify: `pkg/dashboard/handler.go:179-211`（ServeStatic 函数）

- [ ] **Step 1: 添加 API 路径拦截逻辑到 ServeStatic 函数**

在 `ServeStatic` 函数开头（OAuth 处理之前），添加已知 API 路径检查。命中时返回 JSON 404 错误，提示用户使用 `/v1/` 前缀。

文件: `pkg/dashboard/handler.go:179`（替换整个 ServeStatic 函数）

```go
// ServeStatic serves the SPA frontend for non-API routes.
func (h *Handler) ServeStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Intercept known API paths that are missing the /v1/ prefix.
	// Return a structured JSON 404 so SDKs get a clear error instead of HTML.
	for _, p := range knownAPIPaths {
		if path == p || path == "/v1"+p {
			continue // /v1/ variants are handled by registered routes
		}
		if strings.HasPrefix(path, strings.TrimSuffix(p, "/"))+"/") {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"error": map[string]string{
					"type":    "invalid_request_error",
					"message": fmt.Sprintf("%s %s not found. JoyCodeProxy serves the API under /v1/. Set base_url to http://<host>:<port>/v1", r.Method, path),
				},
			})
			return
		}
	}

	// Check exact match against known API paths
	if _, isAPI := knownAPISet[path]; isAPI {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("%s %s not found. JoyCodeProxy serves the API under /v1/. Set base_url to http://<host>:<port>/v1", r.Method, path),
			},
		})
		return
	}

	// Handle JoyCode OAuth callback on root path: /?pt_key=xxx
	if path == "/" && r.URL.Query().Get("pt_key") != "" {
		h.handleOAuthCallback(w, r)
		return
	}

	if path == "/" {
		path = "/index.html"
	}

	// Try exact file
	if f, err := h.staticFS.Open(strings.TrimPrefix(path, "/")); err == nil {
		defer f.Close()
		stat, _ := f.Stat()
		if !stat.IsDir() {
			http.ServeContent(w, r, filepath.Base(path), stat.ModTime(), readFileSeeker{f})
			return
		}
	}

	// SPA fallback
	f, err := h.staticFS.Open("index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, _ := f.Stat()
	http.ServeContent(w, r, "index.html", stat.ModTime(), readFileSeeker{f})
}
```

- [ ] **Step 2: 在 handler.go 中添加已知 API 路径常量**

在 `ServeStatic` 函数上方添加已知 API 路径的查找表。

文件: `pkg/dashboard/handler.go:176`（在 ServeStatic 函数之前插入）

```go
// knownAPIPaths lists OpenAI/Anthropic-style endpoint paths that users
// commonly hit without the /v1/ prefix. When these paths arrive at the
// SPA catch-all we return a JSON 404 with a helpful hint instead of HTML.
var knownAPIPaths = []string{
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

var knownAPISet = func() map[string]bool {
	m := make(map[string]bool, len(knownAPIPaths))
	for _, p := range knownAPIPaths {
		m[p] = true
	}
	return m
}()
```

- [ ] **Step 3: 验证 — 编译检查**

Run: `cd /Users/cc11001100/github/vibe-coding-labs/JoyCodeProxy && go build ./...`
Expected:
  - Exit code: 0
  - Output does NOT contain: "error" or "undefined"

- [ ] **Step 4: 验证 — 手动测试路由行为**

Run: `cd /Users/cc11001100/github/vibe-coding-labs/JoyCodeProxy && go test ./pkg/dashboard/...`
Expected:
  - Exit code: 0 (如果有测试) 或无 test files 报错也可接受

额外手动验证（需要服务运行时）：
- `curl -s http://127.0.0.1:34891/chat/completions` → 应返回 JSON 404 而非 HTML
- `curl -s http://127.0.0.1:34891/v1/chat/completions` → 正常 API 行为不变
- `curl -s http://127.0.0.1:34891/` → SPA 正常返回 HTML
- `curl -s http://127.0.0.1:34891/accounts` → SPA 正常 fallback（React Router 处理）

- [ ] **Step 5: 质量门禁检查**

Run: `cd /Users/cc11001100/github/vibe-coding-labs/JoyCodeProxy && go build ./... && go vet ./...`
Expected:
  - Exit code: 0
  - 无遗留 debug 语句
  - 无未使用的 import

- [ ] **Step 6: 提交**

Run: `git add pkg/dashboard/handler.go && git commit -m "fix(router): return JSON 404 for API paths missing /v1/ prefix

Previously, requests to /chat/completions (without /v1/) were caught
by the SPA catch-all and returned HTML. SDKs silently treated this as
the response body, causing confusing errors like 'str has no attribute
choices'. Now we intercept known API endpoint paths and return a
structured JSON 404 with a hint to use the /v1/ prefix.

Fixes #3"`

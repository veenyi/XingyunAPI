package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/anthropic"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/auth"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/dashboard"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/keepalive"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/logrot"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/openai"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/proxy"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/store"
)

var (
	serveHost       string
	servePort       int
	serveTLS        bool
	requestCounter uint64
)

// per-account client 缓存：同一账号复用同一个 joycode.Client（共享 SessionID 与连接池）。
// 每次请求新建 client 会生成全新 SessionID，上游需为新会话重建上下文 → TTFB 波动（时快时慢）。
// ptKey 变化（keepalive 刷新）时通过对比 PtKey 自动重建缓存。
var (
	clientCacheMu sync.RWMutex
	clientCache   = map[string]*joycode.Client{}
)

// cachedClient 返回账号对应的复用 client；缓存缺失或 ptKey 已变化时重建。
func cachedClient(acc *store.Account, timeout time.Duration, sharedTransport *http.Transport) *joycode.Client {
	if acc == nil || acc.UserID == "" {
		return nil
	}
	clientCacheMu.RLock()
	cl := clientCache[acc.UserID]
	clientCacheMu.RUnlock()
	if cl != nil && cl.PtKey == acc.PtKey {
		return cl
	}
	cl = joycode.NewClient(acc.PtKey, acc.UserID)
	// 账号专属网关上下文：官方客户端每个账号的 tenant/loginType/网关地址不同，
	// 写死默认值会导致 401（账号未登录）或 AI_GRAY_ACCESS_DENIED。
	if acc.Tenant != "" || acc.LoginType != "" || acc.ColorBaseURL != "" || acc.MasterBaseURL != "" || acc.OrgFullName != "" {
		cl.SetColorContext(acc.ColorBaseURL, acc.MasterBaseURL, acc.Tenant, acc.LoginType, acc.OrgFullName)
	}
	cl.SetTimeout(timeout)
	if sharedTransport != nil {
		cl.SetTransport(sharedTransport)
	}
	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	if existing := clientCache[acc.UserID]; existing != nil && existing.PtKey == acc.PtKey {
		return existing
	}
	clientCache[acc.UserID] = cl
	return cl
}

var serveCmd = &cobra.Command{
	Use:     "serve",
	Short:   "启动代理服务器",
	Long:    "启动 OpenAI/Anthropic 兼容的 API 代理服务器，将请求转换为 JoyCode API 格式。",
	GroupID: "core",
	Example: `  # 默认启动（0.0.0.0:34891）
  joycode-proxy serve

  # 指定端口
  joycode-proxy serve -p 8080

  # 启用调试日志
  joycode-proxy -v serve

  # 跳过凭据验证（用于测试）
  joycode-proxy serve --skip-validation`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if os.Getenv("_JOYCODE_DAEMON_CHILD") == "1" {
			runAsDaemonChild()
		}

		setupLogRotation()

		client, err := resolveClient()
		if err != nil {
			return err
		}

		// Open store for dashboard
		s, err := store.Open("")
		if err != nil {
			log.Printf("Warning: dashboard store unavailable: %v", err)
		}

		// Migrate historical request_logs: map old api_keys to first account
		if s != nil {
			accounts, _ := s.ListAccounts()
			if len(accounts) > 0 {
				if n, err := s.ReassignLogs([]string{"", "joycode", "default"}, accounts[0].UserID); err == nil && n > 0 {
					log.Printf("Migrated %d request logs to account %q", n, accounts[0].UserID)
				}
			}
		}
		if s != nil {
			if n, err := s.MigrateTokenLogs(); err == nil && n > 0 {
				log.Printf("Migrated %d token-based request logs to account api_keys", n)
			}
		}

		srv := openai.NewServer(client, s)
		anth := anthropic.NewHandler(client, s)

		// Start credential keepalive: check every 1min, refresh accounts older than 1h
		keeper := keepalive.NewKeeper(s, 1*time.Hour)
		keeper.Start(1 * time.Minute)
		// 账号保活：周期向每个账号发送随机极短聊天消息（模拟 JoyCode 客户端对话），
		// 防止账号因长期无客户端活动被上游冻结。间隔用户可在设置页自定义
		// （keepalive_interval_minutes，1~1440 分钟，默认 360 = 6 小时）。
		keepaliveMin := s.GetIntSetting("keepalive_interval_minutes", 360)
		if keepaliveMin < 1 {
			keepaliveMin = 1
		}
		if keepaliveMin > 1440 {
			keepaliveMin = 1440
		}
		keeper.SetKeepaliveTTL(time.Duration(keepaliveMin) * time.Minute)

		// stopCh is closed on shutdown so background goroutines can exit.
		stopCh := make(chan struct{})

		// 监听保活间隔设置变化，动态更新（无需重启服务）
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			last := keepaliveMin
			for {
				select {
				case <-ticker.C:
					m := s.GetIntSetting("keepalive_interval_minutes", 360)
					if m < 1 {
						m = 1
					}
					if m > 1440 {
						m = 1440
					}
					if m != last {
						keeper.SetKeepaliveTTL(time.Duration(m) * time.Minute)
						last = m
					}
				case <-stopCh:
					return
				}
			}
		}()

		// Per-request client resolution from database accounts
		if s != nil {
			// Shared transport for connection pooling and limits
			// MaxConnsPerHost 需要足够大：每个流式请求会长期占用一条到上游的连接
			// （SSE 可能持续数分钟），20 的上限在多个客户端并发对话时会迅速占满，
			// 后续请求只能排队等连接释放 → 表现为间歇性卡顿。
			sharedTransport := &http.Transport{
				MaxIdleConnsPerHost: 50,
				MaxConnsPerHost:     100,
				IdleConnTimeout:     90 * time.Second,
			}

			// Background goroutine to sync max_connections setting
			go func() {
				ticker := time.NewTicker(10 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						maxConns := s.GetIntSetting("max_connections", 100)
						if maxConns < 1 {
							maxConns = 1
						}
						sharedTransport.MaxConnsPerHost = maxConns
						idle := maxConns / 2
						if idle < 2 {
							idle = 2
						}
						sharedTransport.MaxIdleConnsPerHost = idle
					case <-stopCh:
						return
					}
				}
			}()

			resolver := func(r *http.Request) *joycode.Client {
				systemClient := client
				if systemClient != nil && systemClient.PtKey == "placeholder" {
					if creds, err := auth.LoadFromSystem(); err == nil {
						systemClient = joycode.NewClient(creds.PtKey, creds.UserID)
						systemClient.SetColorContext(creds.ColorBaseURL, creds.MasterBaseURL, creds.Tenant, creds.LoginType, creds.OrgFullName)
					}
				}
				apiKey := extractAPIKey(r)
				timeout := s.GetIntSetting("request_timeout", 1800)
				if timeout < 60 {
					timeout = 60
				}
				timeoutDur := time.Duration(timeout) * time.Second
				// 聚合 Key：自动按剩余积分在多个账号间轮询
				if apiKey != "" && apiKey == getAggregateKey(s) {
					if cl := resolveAggregateClient(s, timeoutDur, sharedTransport); cl != nil {
						return cl
					}
					// 无可用账号 → 回退默认逻辑
				}
				if apiKey != "" {
					if account, _ := s.GetAccountByToken(apiKey); account != nil {
						cl := cachedClient(account, timeoutDur, sharedTransport)
						if systemClient != nil && systemClient.PtKey != "" && systemClient.PtKey != "placeholder" && systemClient.UserID == account.UserID {
							cl.SetAnthropicPtKey(systemClient.PtKey)
						}
						return cl
					}
					if account, _ := s.GetAccount(apiKey); account != nil {
						cl := cachedClient(account, timeoutDur, sharedTransport)
						if systemClient != nil && systemClient.PtKey != "" && systemClient.PtKey != "placeholder" && systemClient.UserID == account.UserID {
							cl.SetAnthropicPtKey(systemClient.PtKey)
						}
						return cl
					}
				}
				if account, _ := s.GetDefaultAccount(); account != nil {
					cl := cachedClient(account, timeoutDur, sharedTransport)
					if systemClient != nil && systemClient.PtKey != "" && systemClient.PtKey != "placeholder" && systemClient.UserID == account.UserID {
						cl.SetAnthropicPtKey(systemClient.PtKey)
					}
					return cl
				}
				return systemClient
			}
			srv.Resolver = resolver
			anth.Resolver = resolver
		}

		// Background log cleanup goroutine (nil-guarded; s may be nil if store.Open failed)
		go func() {
			if s == nil {
				return
			}
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			cleanup := func() {
				if days := s.GetIntSetting("log_retention_days", 30); days > 0 {
					s.CleanupOldLogs(days)
				}
			}
			cleanup()
			for {
				select {
				case <-ticker.C:
					cleanup()
				case <-stopCh:
					return
				}
			}
		}()

		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		anth.RegisterRoutes(mux)

		// Register dashboard API routes + static file serving
		if s != nil {
			subFS, _ := fs.Sub(staticFiles, "static")
			dash := dashboard.NewHandler(s, subFS, keeper)
			dash.Version = Version
			dash.RegisterRoutes(mux)
			mux.HandleFunc("/", dash.ServeStatic)
		}

		var handler http.Handler = mux
		if s != nil {
			handler = auth.JWTMiddleware(s, handler)
			handler = requestLogMiddleware(handler, s)
		}
		if verbose {
			handler = loggingMiddleware(handler)
		}

		addr := fmt.Sprintf("%s:%d", serveHost, servePort)
		httpSrv := &http.Server{
			Addr:    addr,
			Handler: handler,
		}

		var tlsCfg *tls.Config
		scheme := "http"
		if serveTLS {
			tlsCfg, err = ensureTLS()
			if err != nil {
				log.Printf("Warning: TLS setup failed (%v), falling back to HTTP", err)
			} else {
				httpSrv.TLSConfig = tlsCfg
				scheme = "https"
			}
		}

		go func() {
			fmt.Println()
			fmt.Printf("  行云API %s\n", Version)
			fmt.Println("  ─────────────────────────────────────────────────")
			fmt.Println()
			fmt.Println("  Endpoints:")
			fmt.Println("    POST /v1/chat/completions  — Chat (OpenAI format)")
			fmt.Println("    POST /v1/messages          — Chat (Anthropic/Claude Code format)")
			fmt.Println("    POST /v1/web-search        — Web Search")
			fmt.Println("    POST /v1/rerank            — Rerank documents")
			fmt.Println("    GET  /v1/models            — Model list")
			fmt.Println("    GET  /health               — Health check")
			fmt.Println()
			fmt.Println("  Dashboard:")
			if tlsCfg != nil {
				fmt.Printf("    https://%s — Web UI (also accepts HTTP)\n", addr)
			} else {
				fmt.Printf("    http://%s — Web UI\n", addr)
			}
			fmt.Println()
			fmt.Println("  Claude Code setup:")
			fmt.Printf("    export ANTHROPIC_BASE_URL=http://%s\n", addr)
			fmt.Println("    export ANTHROPIC_API_KEY=joycode")
			if verbose {
				fmt.Println()
				fmt.Println("  Verbose logging: enabled")
			}
			fmt.Println()

			var listenErr error
			if tlsCfg != nil {
				httpSrv.TLSConfig = tlsCfg
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					log.Fatalf("Listen error: %v", err)
				}
				dualLn := newDualListener(ln, tlsCfg, handler)
				log.Printf("行云API running on %s://%s (also accepts HTTP)", scheme, addr)
				listenErr = httpSrv.Serve(dualLn)
			} else {
				log.Printf("行云API running on %s://%s", scheme, addr)
				listenErr = httpSrv.ListenAndServe()
			}
			if listenErr != nil && listenErr != http.ErrServerClosed {
				log.Fatalf("Server error: %v", listenErr)
			}
		}()

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("Shutting down server...")

		// Signal background goroutines to stop.
		close(stopCh)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
		keeper.Stop()
		if s != nil {
			s.Close()
		}
		log.Println("Server stopped")
		return nil
	},
}

func init() {
	serveCmd.Flags().StringVarP(&serveHost, "host", "H", "0.0.0.0", "绑定地址")
	serveCmd.Flags().IntVarP(&servePort, "port", "p", 34891, "绑定端口")
	serveCmd.Flags().BoolVar(&serveTLS, "tls", true, "启用 HTTPS（自签名证书）")
	rootCmd.AddCommand(serveCmd)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("-> %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		log.Printf("<- %s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func requestLogMiddleware(next http.Handler, s *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r = store.InitTokenUsage(r)
		r = store.InitModel(r)
		r = store.InitAccountModel(r)

		// Assign request ID for log correlation
		reqID := atomic.AddUint64(&requestCounter, 1)
		r = anthropic.WithRequestID(r, reqID)

		// Resolve account from API key/token (used for model, session tracking, logging)
		var resolvedAccount *store.Account
		if s != nil {
			ak := extractAPIKey(r)
			if ak != "" {
				if a, _ := s.GetAccountByToken(ak); a != nil {
					resolvedAccount = a
				} else if a, _ := s.GetAccount(ak); a != nil {
					resolvedAccount = a
				}
			}
			if resolvedAccount == nil {
				if a, _ := s.GetDefaultAccount(); a != nil {
					resolvedAccount = a
				}
			}
			if resolvedAccount != nil {
				store.SetAccountDefaultModel(r, resolvedAccount.DefaultModel)
			}
		}

		// Peek at body to extract model + session_id + stream flag before handler consumes it
		var model string
		var sessionID string
		var bodyStream bool
		if r.Method == "POST" && r.Body != nil {
			bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 100<<20))
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			var body map[string]interface{}
			if json.Unmarshal(bodyBytes, &body) == nil {
				if m, ok := body["model"].(string); ok {
					model = m
				}
				if s, ok := body["stream"].(bool); ok {
					bodyStream = s
				}
				// Extract session_id from Claude Code metadata
				if meta, ok := body["metadata"].(map[string]interface{}); ok {
					if sid, ok := meta["session_id"].(string); ok && sid != "" {
						sessionID = sid
					}
				}
			}
		}

		// Record session activity for /v1/ proxy requests
		if strings.HasPrefix(r.URL.Path, "/v1/") && resolvedAccount != nil {
			if sessionID == "" {
				sessionID = r.RemoteAddr // fallback: unique per client IP
			}
			proxy.RecordSession(resolvedAccount.UserID, sessionID)
		}

		rw := &responseWriter{ResponseWriter: w, statusCode: 200, bodyLimit: 64 << 10}
		next.ServeHTTP(rw, r)

		// Log /v1/ requests (skip GET /v1/models — it's a lightweight list
		// endpoint with no model/tokens and would pollute stats with empty rows).
		path := r.URL.Path
		if strings.HasPrefix(path, "/v1/") && !(r.Method == http.MethodGet && path == "/v1/models") {
			apiKey := extractAPIKey(r)
			if apiKey != "" {
				if account, _ := s.GetAccountByToken(apiKey); account != nil {
					apiKey = account.UserID
				}
			}
			if apiKey == "" {
				if a, _ := s.GetDefaultAccount(); a != nil {
					apiKey = a.UserID
				}
			}

			isStream := bodyStream || r.URL.Query().Get("stream") != ""
			latency := time.Since(start).Milliseconds()

			var errMsg string
			if rw.statusCode >= 400 {
				errMsg = fmt.Sprintf("HTTP %d on %s %s", rw.statusCode, r.Method, path)
				if body := strings.TrimSpace(rw.body.String()); body != "" {
					errMsg = fmt.Sprintf("%s\n%s", errMsg, body)
				}
				slog.Error("proxy error response",
					"request_id", reqID,
					"status", rw.statusCode,
					"method", r.Method,
					"path", path,
					"model", model,
					"latency_ms", latency,
					"api_key", apiKey,
					"error", errMsg,
				)
			}

			var inTk, outTk int
			inTk, outTk = store.GetTokenUsage(r)
			resolvedModel := store.GetModel(r)
				if resolvedModel != "" {
					model = resolvedModel
				}
				if s.GetSetting("enable_request_logging") != "false" {
					go s.LogRequest(apiKey, model, path, isStream, rw.statusCode, latency, errMsg, inTk, outTk)
				}
		}
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
	bodyLimit  int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(p []byte) (int, error) {
	if rw.bodyLimit > 0 && rw.body.Len() < rw.bodyLimit {
		remaining := rw.bodyLimit - rw.body.Len()
		if len(p) > remaining {
			rw.body.Write(p[:remaining])
		} else {
			rw.body.Write(p)
		}
	}
	return rw.ResponseWriter.Write(p)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// extractAPIKey pulls the caller's API key from either the x-api-key header
// (Anthropic convention) or the Authorization: Bearer <key> header (OpenAI
// convention). Returns "" if neither is present.
func extractAPIKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}

// setupLogRotation initializes rotating log writers for slog and log.
// Also truncates stdout.log if launchd has let it grow too large.
func setupLogRotation() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logDir := filepath.Join(home, logDir)

	cfg := logrot.DefaultConfig(logDir, "stderr")
	rw, err := logrot.New(cfg)
	if err != nil {
		log.Printf("Warning: log rotation init failed: %v", err)
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(rw, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log.SetOutput(rw)

	// Truncate stdout.log if launchd has let it grow too large
	stdoutPath := filepath.Join(logDir, "stdout.log")
	logrot.TruncateFileIfNeeded(stdoutPath, cfg.MaxFileSize)
}

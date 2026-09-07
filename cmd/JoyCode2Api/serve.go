package main

import (
	"bufio"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/veenyi/XingyunAPI/pkg/anthropic"
	"github.com/veenyi/XingyunAPI/pkg/auth"
	"github.com/veenyi/XingyunAPI/pkg/checkin"
	"github.com/veenyi/XingyunAPI/pkg/custom"
	"github.com/veenyi/XingyunAPI/pkg/dashboard"
	"github.com/veenyi/XingyunAPI/pkg/freepool"
	"github.com/veenyi/XingyunAPI/pkg/health"
	"github.com/veenyi/XingyunAPI/pkg/joycode"
	"github.com/veenyi/XingyunAPI/pkg/keepalive"
	"github.com/veenyi/XingyunAPI/pkg/keyed"
	"github.com/veenyi/XingyunAPI/pkg/keyfree"
	"github.com/veenyi/XingyunAPI/pkg/logrot"
	"github.com/veenyi/XingyunAPI/pkg/openai"
	"github.com/veenyi/XingyunAPI/pkg/provider"
	"github.com/veenyi/XingyunAPI/pkg/probe"
	"github.com/veenyi/XingyunAPI/pkg/proxy"
	"github.com/veenyi/XingyunAPI/pkg/qoder"
	"github.com/veenyi/XingyunAPI/pkg/qwenwork"
	"github.com/veenyi/XingyunAPI/pkg/route"
	"github.com/veenyi/XingyunAPI/pkg/store"
	"github.com/veenyi/XingyunAPI/pkg/traework"
	"github.com/veenyi/XingyunAPI/pkg/watchdog"
	"github.com/veenyi/XingyunAPI/pkg/workbuddy"
)

var (
	serveHost       string
	servePort       int
	serveTLS        bool
	requestCounter uint64
)

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
		if s != nil {
			// /v1/* 强制凭据：空/未知 key 一律 401（反向隧道下 loopback 与
			// 公网流量同源，无豁免；合法 key = 账号 token / user_id / 聚合 Key）。
			srv.KeyValidator = s.ValidAPIKey
			anth.KeyValidator = s.ValidAPIKey
		}

		// Start credential keepalive: check every 10min, refresh accounts older than 1h
		var keeper *keepalive.Keeper
		if s != nil {
			keeper = keepalive.NewKeeper(s, 1*time.Hour)
			keeper.Start(10 * time.Minute)
		}

		// Watchdog: relaunch keeper.sh when the keeper agent port stays unreachable
		// (e.g. after NAS reboot, since keeper.sh is not auto-started by the app center).
		watchdog.Start("http://127.0.0.1:34893/v1/models", 5*time.Minute)

		// Per-request client resolution from database accounts
		if s != nil {
			// Shared transport for connection pooling and limits
			sharedTransport := &http.Transport{
				MaxIdleConnsPerHost: 10,
				MaxConnsPerHost:     20,
				IdleConnTimeout:     90 * time.Second,
			}

			// Background goroutine to sync max_connections setting
			go func() {
				ticker := time.NewTicker(10 * time.Second)
				defer ticker.Stop()
				for range ticker.C {
					maxConns := s.GetIntSetting("max_connections", 20)
					if maxConns < 1 {
						maxConns = 1
					}
					sharedTransport.MaxConnsPerHost = maxConns
					idle := maxConns / 2
					if idle < 2 {
						idle = 2
					}
					sharedTransport.MaxIdleConnsPerHost = idle
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
				apiKey := r.Header.Get("x-api-key")
				if apiKey == "" {
					auth := r.Header.Get("Authorization")
					if strings.HasPrefix(auth, "Bearer ") {
						apiKey = strings.TrimPrefix(auth, "Bearer ")
					}
				}
				timeout := s.GetIntSetting("request_timeout", 1800)
				if timeout < 60 {
					timeout = 60
				}
				if apiKey != "" {
					if account, _ := s.GetAccountByToken(apiKey); account != nil {
						cl := joycode.NewClient(account.PtKey, account.UserID)
						if systemClient != nil && systemClient.PtKey != "" && systemClient.PtKey != "placeholder" && systemClient.UserID == account.UserID {
							cl.SetAnthropicPtKey(systemClient.PtKey)
						}
						cl.SetTimeout(time.Duration(timeout) * time.Second)
						return cl
					}
					if account, _ := s.GetAccount(apiKey); account != nil {
						cl := joycode.NewClient(account.PtKey, account.UserID)
						if systemClient != nil && systemClient.PtKey != "" && systemClient.PtKey != "placeholder" && systemClient.UserID == account.UserID {
							cl.SetAnthropicPtKey(systemClient.PtKey)
						}
						cl.SetTimeout(time.Duration(timeout) * time.Second)
						return cl
					}
				}
				if account, _ := s.GetDefaultAccount(); account != nil {
					cl := joycode.NewClient(account.PtKey, account.UserID)
					if systemClient != nil && systemClient.PtKey != "" && systemClient.PtKey != "placeholder" && systemClient.UserID == account.UserID {
						cl.SetAnthropicPtKey(systemClient.PtKey)
					}
					cl.SetTimeout(time.Duration(timeout) * time.Second)
					cl.SetTransport(sharedTransport)
					return cl
				}
				return systemClient
			}
			srv.Resolver = resolver
			anth.Resolver = resolver
		}

		// ---- 渠道/路由/探测装配：keyed 预设 + keyfree + freepool + custom
		// （qoder/workbuddy 账号池在 accountsSync 后追加），共享熔断注册表。
		hg := health.New()
		var customMgr *custom.Manager
		var qoderPool *qoder.Pool
		var wbPool *workbuddy.Pool
		var qwPool *qwenwork.Pool
		var twPool *traework.Pool
		var cm *checkin.Manager
		if s != nil {
			customMgr = custom.New(s)
			customMgr.SetRegistry(hg)
			if err := customMgr.Load(); err != nil {
				slog.Warn("加载自定义渠道失败", "error", err)
			}
			// v0.7.2 签到与聊天共用账号，重建版拆开后老库只有签到一半；
			// 启动时回填（幂等，池里已有的账号不动）。
			if raw := s.GetSetting("checkin_accounts"); strings.TrimSpace(raw) != "" {
				if n := workbuddy.BackfillCheckinAccounts(s, raw); n > 0 {
					slog.Info("workbuddy: 已从签到中心回填账号", "count", n)
				}
			}
			qoderPool = qoder.New(s, nil)
			wbPool = workbuddy.New(s, nil)
			cm = checkin.NewManager(s, nil)
			cm.Start()
			// QwenWork（千问办公）聊天池：凭据权威源 = 签到中心 blob，
			// 回调实时取（签到侧被动刷新令牌后自动生效）。
			qwPool = qwenwork.NewPool(func() []qwenwork.Credential {
				creds := cm.QwenWorkCredentials()
				out := make([]qwenwork.Credential, 0, len(creds))
				for _, c := range creds {
					out = append(out, qwenwork.Credential{
						ID: c.ID, Nickname: c.Nickname,
						AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
					})
				}
				return out
			}, nil)
			// TraeWork（TRAE SOLO CN）聊天池：凭据权威源 = 签到中心 blob，
			// 签到侧刷新令牌后自动生效。
			twPool = traework.NewPool(func() []traework.Credential {
				creds := cm.TraeWorkCredentials()
				out := make([]traework.Credential, 0, len(creds))
				for _, c := range creds {
					out = append(out, traework.Credential{
						ID: c.ID, Nickname: c.Nickname,
						AccessToken: c.AccessToken, RefreshToken: c.RefreshToken,
						UID: c.UID, DeviceID: c.DeviceID, MachineID: c.MachineID,
					})
				}
				return out
			}, nil)
		}
		keyless := func() []provider.Keyless {
			list := []provider.Keyless{}
			if s == nil {
				return list
			}
			for _, p := range keyed.Presets() {
				c := keyed.New(keyed.Options{Preset: p.ID, Settings: s})
				if c != nil && c.Enabled() {
					list = append(list, c)
				}
			}
			if kf := keyfree.New(s); kf != nil && kf.Enabled() {
				list = append(list, kf)
			}
			if fp := freepool.New(s); fp != nil && fp.Enabled() {
				list = append(list, fp)
			}
			if customMgr != nil {
				list = append(list, customMgr.KeylessList()...)
			}
			if qoderPool != nil && qoderPool.Enabled() {
				list = append(list, qoderPool)
			}
			if wbPool != nil && wbPool.Enabled() {
				list = append(list, wbPool)
			}
			if qwPool != nil && qwPool.Enabled() {
				list = append(list, qwPool)
			}
			if twPool != nil && twPool.Enabled() {
				list = append(list, twPool)
			}
			return list
		}
		srv.SetRouteDeps(keyless, hg)
		anth.SetRouteDeps(keyless, hg)

		// 全局路由器：openai 侧每请求补 Primary，anthropic 侧仅 keyless 兜底
		var rt *route.Router
		var pr *probe.Prober
		if s != nil {
			rt = &route.Router{Keyless: keyless, Health: hg, Store: s}
			pr = probe.New(rt, s)
			pr.Start()
		}

		// Background log cleanup goroutine
		go func() {
			ticker := time.NewTicker(1 * time.Hour)
			defer ticker.Stop()
			if days := s.GetIntSetting("log_retention_days", 30); days > 0 {
				s.CleanupOldLogs(days)
			}
			for range ticker.C {
				if days := s.GetIntSetting("log_retention_days", 30); days > 0 {
					s.CleanupOldLogs(days)
				}
			}
		}()

		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		anth.RegisterRoutes(mux)

		// Register dashboard API routes + static file serving
		var dashHandler *dashboard.Handler
		if s != nil {
			subFS, _ := fs.Sub(staticFiles, "static")
			dash := dashboard.NewHandler(s, subFS, keeper)
			dash.Version = Version
			presets := []map[string]string{}
			for _, p := range keyed.Presets() {
				presets = append(presets, map[string]string{
					"id": p.ID, "name": p.Name, "note": p.Note,
					"base_url": p.BaseURL, "key_prefix": p.KeyPrefix,
					"free_tier": fmt.Sprintf("%t", p.FreeTier),
				})
			}
			dash.SetChannelDeps(rt, presets, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				for _, ch := range rt.KeylessChannels() {
					if r, ok := ch.(provider.Catalog); ok {
						r.RefreshNow(ctx)
					}
				}
			})
			// 保存 custom 渠道后即时生效：重载渠道表并预热目录
			// （此前 PUT /api/settings 只落库，运行实例要重启才感知）。
			dash.SetSettingsSavedHook(func(saved map[string]string) {
				if _, ok := saved["custom_providers"]; !ok {
					return
				}
				if err := customMgr.Load(); err != nil {
					slog.Warn("重载自定义渠道失败", "error", err)
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				for _, ch := range customMgr.KeylessList() {
					if c, ok := ch.(provider.Catalog); ok {
						c.RefreshNow(ctx)
					}
				}
				slog.Info("自定义渠道已重载", "count", len(customMgr.KeylessList()))
			})
			dash.SetCheckinDeps(cm, func() {
				if qoderPool != nil {
					qoderPool.SyncAccounts()
				}
			}, func() {
				if wbPool != nil {
					wbPool.SyncAccounts()
				}
			})
			dash.RegisterRoutes(mux)
			dash.KeepDevCloudAlive()
			dash.StartTunnelIfEnabled()
			if home, err := os.UserHomeDir(); err == nil {
				dash.SetAuditFile(filepath.Join(home, logDir, "audit.log"))
			}
			dashHandler = dash
			mux.HandleFunc("/", dash.ServeStatic)
		}

		var handler http.Handler = mux
		if dashHandler != nil {
			handler = dashHandler.AuditMiddleware(handler)
		}
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
			fmt.Printf("  JoyCode Proxy %s\n", Version)
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
				log.Printf("JoyCode Proxy running on %s://%s (also accepts HTTP)", scheme, addr)
				listenErr = httpSrv.Serve(dualLn)
			} else {
				log.Printf("JoyCode Proxy running on %s://%s", scheme, addr)
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

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
		if pr != nil {
			pr.Stop()
		}
		if cm != nil {
			cm.Stop()
		}
		if keeper != nil {
			keeper.Stop()
		}
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
			ak := r.Header.Get("x-api-key")
			if ak == "" {
				if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
					ak = strings.TrimPrefix(auth, "Bearer ")
				}
			}
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

		// Peek at body to extract model + session_id before handler consumes it
		var model string
		var sessionID string
		if r.Method == "POST" && r.Body != nil {
			bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 100<<20))
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			var body map[string]interface{}
			if json.Unmarshal(bodyBytes, &body) == nil {
				if m, ok := body["model"].(string); ok {
					model = m
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

		// Log /v1/ requests
		path := r.URL.Path
		if strings.HasPrefix(path, "/v1/") {
			apiKey := r.Header.Get("x-api-key")
			if apiKey == "" {
				apiKey := r.Header.Get("Authorization")
				if strings.HasPrefix(apiKey, "Bearer ") {
					apiKey = strings.TrimPrefix(apiKey, "Bearer ")
				}
			}
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

			isStream := r.URL.Query().Get("stream") != "" || path == "/v1/messages"
			latency := time.Since(start).Milliseconds()

			var errMsg string
			if rw.statusCode >= 400 {
				reqID := atomic.AddUint64(&requestCounter, 1)
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

// Hijack lets WebSocket upgrades through the logging wrapper — without it
// gorilla's Upgrader fails with "response does not implement http.Hijacker".
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
	}
	return hj.Hijack()
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

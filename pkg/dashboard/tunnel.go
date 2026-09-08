// Package dashboard tunnel.go — 公网入口：动态发现运行中项目 + Go 原生 ssh -R
// 反向隧道把面板暴露到公网。项目 ID 不再硬编码，改为每次拨号前动态发现。
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/devcloud"
	"golang.org/x/crypto/ssh"
)

const (
	tunnelRemotePort  = 9000
	tunnelLocalTarget = "127.0.0.1:34891"
)

type tunnelState string

const (
	tunnelStopped    tunnelState = "stopped"
	tunnelConnecting tunnelState = "connecting"
	tunnelActive     tunnelState = "active"
	tunnelBackoff    tunnelState = "backoff"
)

// TunnelManager owns one reverse-tunnel SSH session with dynamic project
// discovery (multi-account, multi-project).
type TunnelManager struct {
	mu         sync.Mutex
	state      tunnelState
	carrier    string // native=本进程 SSH；external=外部隧道承载
	projectURL string // 当前承载项目的公网 URL
	lastErr    string
	since      time.Time
	cancel     context.CancelFunc
	handler    *Handler // back-reference for dynamic discovery
	autoTried  bool     // 自动建项目只尝试一次（避免反复扣额度）
}

func NewTunnelManager(h *Handler) *TunnelManager {
	return &TunnelManager{state: tunnelStopped, handler: h}
}

func (t *TunnelManager) Status() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]interface{}{
		"state":    string(t.state),
		"carrier":  t.carrier,
		"url":      t.projectURL,
		"last_err": t.lastErr,
		"since":    t.since.Format(time.RFC3339),
	}
}

// Start launches the tunnel goroutine (idempotent) and persists the intent.
func (t *TunnelManager) Start(persist func(bool)) error {
	t.mu.Lock()
	if t.state == tunnelConnecting || t.state == tunnelActive || t.state == tunnelBackoff {
		t.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.state = tunnelConnecting
	t.lastErr = ""
	t.since = time.Now()
	t.mu.Unlock()
	if persist != nil {
		persist(true)
	}
	go t.run(ctx)
	return nil
}

// Stop cancels the tunnel goroutine.
func (t *TunnelManager) Stop(persist func(bool)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.state = tunnelStopped
	t.carrier = ""
	t.since = time.Now()
	if persist != nil {
		persist(false)
	}
}

func (t *TunnelManager) setState(s tunnelState, errStr string) {
	t.mu.Lock()
	t.state = s
	t.lastErr = errStr
	t.since = time.Now()
	t.mu.Unlock()
}

// tunnelTarget is one (client, projectID, publicURL) triple.
type tunnelTarget struct {
	client    *devcloud.Client
	projectID int
	publicURL string
}

// autoCreateTunnelProject 用默认账号自动创建一个 Html 项目并等待启动。
func (h *Handler) autoCreateTunnelProject() (tunnelTarget, error) {
	client, _, err := h.devcloudClient("")
	if err != nil {
		return tunnelTarget{}, fmt.Errorf("无可用账号: %w", err)
	}
	langs, err := client.ListTemplates()
	if err != nil {
		return tunnelTarget{}, fmt.Errorf("获取模板目录失败: %w", err)
	}
	tmplID := 0
	for _, l := range langs {
		if strings.Contains(strings.ToLower(l.Name), "html") {
			for _, v := range l.Versions {
				tmplID = v.ID
				break
			}
			break
		}
	}
	if tmplID == 0 {
		return tunnelTarget{}, fmt.Errorf("未找到 Html 模板")
	}
	in := devcloud.CreateInput{
		Name:              "xingyun-tunnel",
		AbbrName:          "X",
		BgColor:           "#16A34A",
		DevMode:           1,
		ProjectTemplateID: tmplID,
		ResourceCPU:       1,
		ResourceMemory:    "2Gi",
		ResourceStorage:   "10G",
	}
	detail, err := client.CreateProject(in)
	if err != nil {
		return tunnelTarget{}, fmt.Errorf("创建项目失败: %w", err)
	}
	slog.Info("tunnel: auto-created project", "id", detail.ID, "name", detail.Name)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		d, err := client.GetProject(detail.ID)
		if err != nil { continue }
		if strings.EqualFold(d.Status, "Running") {
			return tunnelTarget{client: client, projectID: detail.ID, publicURL: d.URL}, nil
		}
	}
	return tunnelTarget{}, fmt.Errorf("项目 %d 创建成功但 90 秒内未启动", detail.ID)
}

// tunnelTargets discovers all running projects across every account.
func (h *Handler) tunnelTargets() []tunnelTarget {
	var out []tunnelTarget
	if h.store == nil {
		return out
	}
	accounts, _ := h.store.ListAccounts()
	seen := map[int]bool{}
	for _, info := range accounts {
		a, err := h.store.GetAccount(info.UserID)
		if err != nil || a == nil || a.PtKey == "" {
			continue
		}
		client := devcloud.NewClient(a.PtKey)
		projects, err := client.ListProjects()
		if err != nil {
			continue
		}
		for _, pr := range projects {
			if seen[pr.ID] || !strings.EqualFold(pr.Status, "running") {
				continue
			}
			seen[pr.ID] = true
			out = append(out, tunnelTarget{client: client, projectID: pr.ID, publicURL: pr.URL})
		}
	}
	return out
}

// run keeps the tunnel alive: discover targets → dial → listen → pump.
// 如果没有运行中项目且尚未尝试过自动建站，会自动用默认账号创建一个。
func (t *TunnelManager) run(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		targets := t.handler.tunnelTargets()
		if len(targets) == 0 && !t.autoTried {
			t.autoTried = true
			t.setState(tunnelConnecting, "没有运行中项目，自动创建云主机…")
			slog.Info("tunnel: no running projects, auto-creating one")
			if tgt, err := t.handler.autoCreateTunnelProject(); err == nil {
				targets = append(targets, tgt)
				slog.Info("tunnel: auto-created project", "id", tgt.projectID, "url", tgt.publicURL)
			} else {
				slog.Warn("tunnel: auto-create failed", "error", err)
				t.setState(tunnelBackoff, "自动建站失败: "+err.Error())
				if !sleepCtx(ctx, backoff) {
					return
				}
				continue
			}
		}
		if len(targets) == 0 {
			t.setState(tunnelBackoff, "没有运行中的云主机项目")
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}
		var sshc *ssh.Client
		var dialErr error
		var activeURL string
		var activeID int
		for _, tgt := range targets {
			sshc, _, dialErr = tgt.client.DialSSH(tgt.projectID)
			if dialErr == nil {
				activeURL = tgt.publicURL
				activeID = tgt.projectID
				break
			}
			slog.Warn("tunnel: dial failed", "project", tgt.projectID, "error", dialErr)
			sshc = nil
		}
		if sshc == nil {
			t.setState(tunnelBackoff, "ssh dial: "+dialErr.Error())
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}

		// 自愈：模板默认 nginx 只吐 hello world，必须把 location / 反代到
		// ssh -R 的 9000 才能从公网看到面板（2026-09-08 多用户报障根因）。
		ensurePanelNginx(sshc, activeID)

		ln, err := sshc.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", tunnelRemotePort))
		if err != nil {
			sshc.Close()
			if isPortBusyErr(err) && probePublicOK(activeURL) {
				t.mu.Lock()
				t.state = tunnelActive
				t.carrier = "external"
				t.projectURL = activeURL
				t.lastErr = ""
				t.since = time.Now()
				t.mu.Unlock()
				slog.Info("tunnel: served by external carrier")
				return
			}
			t.setState(tunnelBackoff, "remote listen: "+err.Error())
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}

		t.mu.Lock()
		t.state = tunnelActive
		t.carrier = "native"
		t.projectURL = activeURL
		t.lastErr = ""
		t.since = time.Now()
		t.mu.Unlock()
		backoff = 5 * time.Second

		pumpDone := make(chan struct{})
		go func() {
			defer close(pumpDone)
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go handleTunnelConn(conn, tunnelLocalTarget)
			}
		}()

		select {
		case <-ctx.Done():
			ln.Close()
			sshc.Close()
			return
		case <-pumpDone:
			sshc.Close()
			t.setState(tunnelBackoff, "ssh connection lost")
			if !sleepCtx(ctx, backoff) {
				return
			}
		}
	}
}

func handleTunnelConn(remote net.Conn, target string) {
	defer remote.Close()
	local, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		slog.Warn("tunnel: local dial failed", "error", err)
		return
	}
	defer local.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(local, remote); done <- struct{}{} }()
	go func() { io.Copy(remote, local); done <- struct{}{} }()
	<-done
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func isPortBusyErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "already in use") || strings.Contains(msg, "address already") ||
		strings.Contains(msg, "denied by peer") || strings.Contains(msg, "bind")
}

func probePublicOK(url string) bool {
	cli := &http.Client{Timeout: 6 * time.Second}
	resp, err := cli.Get(strings.TrimSuffix(url, "/") + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	return resp.StatusCode == http.StatusOK
}

// panelNginxConf 是隧道项目的 nginx 配置：location / 反代 ssh -R 的 9000
// （面板+API，SSE 关缓冲），保留 /app/ /svc/ 子路径玩法。
const panelNginxConf = `worker_processes auto;
error_log /dev/stderr;
pid /tmp/nginx.pid;

events {
    worker_connections 1024;
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    sendfile on;
    keepalive_timeout 65;
    access_log /dev/stdout;

    server {
        listen 8080;
        server_name localhost;
        client_max_body_size 100M;

        # xingyun-panel marker: managed by xingyun-api tunnel (do not edit)

        location / {
            proxy_pass http://127.0.0.1:9000;
            proxy_set_header Host $host;
            proxy_set_header X-Forwarded-For $remote_addr;
            proxy_http_version 1.1;
            proxy_set_header Connection "";
            proxy_buffering off;
            proxy_cache off;
            proxy_read_timeout 3600s;
            proxy_send_timeout 3600s;
            chunked_transfer_encoding on;
        }
    }
}
`

// ensurePanelNginx 检查容器 nginx 是否已配置面板反代（xinger marker），
// 没有则写入并 reload。模板默认 nginx 只会吐 hello world——r27 自动建站
// 漏了这步导致所有新建 tunnel 项目公网打不开面板（2026-09-08 修）。
// SSH 用户 abc 免密 sudo root；nginx 主配置即 /config/workspace/nginx.conf。
func ensurePanelNginx(sshc *ssh.Client, projectID int) {
	sess, err := sshc.NewSession()
	if err != nil {
		slog.Warn("tunnel: nginx check session failed", "error", err)
		return
	}
	defer sess.Close()
	out, err := sess.CombinedOutput("grep -q 'xingyun-panel marker' /config/workspace/nginx.conf 2>/dev/null && echo ok || echo missing")
	if err == nil && strings.Contains(string(out), "ok") {
		return
	}
	slog.Info("tunnel: provisioning panel nginx conf", "project", projectID)

	// 写配置（临时文件 + sudo install 覆盖，避免重定向权限问题）
	wsess, err := sshc.NewSession()
	if err != nil {
		slog.Warn("tunnel: nginx write session failed", "error", err)
		return
	}
	defer wsess.Close()
	wsess.Stdin = strings.NewReader(panelNginxConf)
	if err := wsess.Run("cat > /tmp/xingyun-nginx.conf && sudo install -m 644 /tmp/xingyun-nginx.conf /config/workspace/nginx.conf && rm -f /tmp/xingyun-nginx.conf"); err != nil {
		slog.Warn("tunnel: nginx conf write failed", "error", err)
		return
	}
	// 测试 + reload（root 的 nginx -c 用绝对路径；reload 失败回滚不处理——
	// 模板配置本就不可用，写坏也比 hello world 好；测试失败则保留原样）
	rsess, err := sshc.NewSession()
	if err != nil {
		return
	}
	defer rsess.Close()
	res, err := rsess.CombinedOutput("sudo /usr/sbin/nginx -t -c /config/workspace/nginx.conf 2>&1 && sudo /usr/sbin/nginx -c /config/workspace/nginx.conf -s reload 2>&1 && echo reloaded")
	slog.Info("tunnel: nginx reload", "project", projectID, "result", strings.TrimSpace(string(res)), "err", err)
}

// handleTunnel GET=status, POST {action: start|stop|restart}.
func (h *Handler) handleTunnel(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if h.tm == nil {
		writeError(w, http.StatusServiceUnavailable, "tunnel manager unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.tm.Status())
	case http.MethodPost:
		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad body")
			return
		}
		persist := func(on bool) {
			if h.store == nil {
				return
			}
			v := "false"
			if on {
				v = "true"
			}
			if err := h.store.SetSetting("cloud_tunnel_enabled", v); err != nil {
				slog.Warn("tunnel: persist enabled flag failed", "error", err)
			}
		}
		switch body.Action {
		case "start", "restart":
			if body.Action == "restart" {
				h.tm.Stop(nil)
				time.Sleep(500 * time.Millisecond)
			}
			if err := h.tm.Start(persist); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			h.writeAudit(map[string]interface{}{
				"ts": time.Now().Format("2006-01-02T15:04:05.000-07:00"), "method": r.Method,
				"path": r.URL.Path, "action": "tunnel_" + body.Action, "remote": r.RemoteAddr,
			})
			writeJSON(w, http.StatusOK, h.tm.Status())
		case "stop":
			h.tm.Stop(persist)
			h.writeAudit(map[string]interface{}{
				"ts": time.Now().Format("2006-01-02T15:04:05.000-07:00"), "method": r.Method,
				"path": r.URL.Path, "action": "tunnel_stop", "remote": r.RemoteAddr,
			})
			writeJSON(w, http.StatusOK, h.tm.Status())
		default:
			writeError(w, http.StatusBadRequest, "action must be start|stop|restart")
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// StartTunnelIfEnabled resumes the public tunnel at app startup.
func (h *Handler) StartTunnelIfEnabled() {
	if h.tm == nil || h.store == nil {
		return
	}
	if h.store.GetSetting("cloud_tunnel_enabled") != "true" {
		return
	}
	if err := h.tm.Start(nil); err != nil {
		slog.Warn("tunnel: auto-start failed", "error", err)
	} else {
		slog.Info("tunnel: auto-started (cloud_tunnel_enabled=true)")
	}
}

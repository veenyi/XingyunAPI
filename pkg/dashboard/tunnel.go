package dashboard

// 公网入口：把本机面板端口经 devcloud SSH 反向隧道暴露到云主机
// （云主机 nginx 已把 8080 → 127.0.0.1:9000）。Go 原生 ssh -R 等价实现，
// 状态机：stopped → connecting → active / backoff(重连) → stopped。

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
)

const (
	tunnelRemotePort  = 9000   // 云主机侧 nginx 反代到此端口
	tunnelProjectID   = 71408
	tunnelPublicURL   = "https://23a2vruijfcp9.dev.jcloudcs.com"
	tunnelLocalTarget = "127.0.0.1:34891" // 本机面板（net.Dial 用 host:port）
)

type tunnelState string

const (
	tunnelStopped    tunnelState = "stopped"
	tunnelConnecting tunnelState = "connecting"
	tunnelActive     tunnelState = "active"
	tunnelBackoff    tunnelState = "backoff" // 掉线重连等待中
)

// TunnelManager owns one reverse-tunnel SSH session.
type TunnelManager struct {
	mu      sync.Mutex
	state   tunnelState
	carrier string // native=本进程 SSH；external=云主机侧已有隧道进程承载
	lastErr string
	since   time.Time
	cancel  context.CancelFunc
}

func NewTunnelManager() *TunnelManager { return &TunnelManager{state: tunnelStopped} }

func (t *TunnelManager) Status() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := map[string]interface{}{
		"state":    string(t.state),
		"carrier":  t.carrier,
		"last_err": t.lastErr,
		"since":    t.since.Format(time.RFC3339),
		"url":      tunnelPublicURL,
	}
	return st
}

// Start launches the tunnel goroutine (idempotent) and persists the intent
// so the tunnel resumes automatically after app restarts (内建自动化铁律).
func (t *TunnelManager) Start(getClient func() (*devcloud.Client, error), persist func(bool)) error {
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

	go t.run(ctx, getClient)
	return nil
}

// Stop cancels the tunnel goroutine and drops the remote listener.
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

// run keeps the tunnel alive until ctx is cancelled: dial → listen remote →
// pump connections; on failure re-dial with 10s backoff.
func (t *TunnelManager) run(ctx context.Context, getClient func() (*devcloud.Client, error)) {
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		client, err := getClient()
		if err != nil {
			t.setState(tunnelBackoff, "devcloud client: "+err.Error())
			slog.Warn("tunnel: devcloud client failed", "error", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}
		sshc, _, err := client.DialSSH(tunnelProjectID)
		if err != nil {
			t.setState(tunnelBackoff, "ssh dial: "+err.Error())
			slog.Warn("tunnel: ssh dial failed", "error", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}

		ln, err := sshc.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", tunnelRemotePort))
		if err != nil {
			sshc.Close()
			// 远端端口已被占用：多半是 keeper 的 panel_tunnel.sh 还在承载。
			// 公网探活健康则认为入口可用，交给外部隧道，本进程退出重连循环。
			if isPortBusyErr(err) && probePublicOK() {
				t.mu.Lock()
				t.state = tunnelActive
				t.carrier = "external"
				t.lastErr = ""
				t.since = time.Now()
				t.mu.Unlock()
				slog.Info("tunnel: served by external carrier (port busy, public probe ok)")
				return
			}
			t.setState(tunnelBackoff, "remote listen: "+err.Error())
			slog.Warn("tunnel: remote listen failed", "error", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}

		t.mu.Lock()
		t.state = tunnelActive
		t.carrier = "native"
		t.lastErr = ""
		t.since = time.Now()
		t.mu.Unlock()
		slog.Info("tunnel: active", "remote", tunnelRemotePort, "project", tunnelProjectID)
		backoff = 5 * time.Second

		// Pump accepted remote connections to the local panel.
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
			// listener died: ssh connection lost
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

// isPortBusyErr matches sshd's "port already in use" / forward-denied errors
// (OpenSSH denies a second bind of the same remote port with a generic
// "tcpip-forward request denied by peer").
func isPortBusyErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "already in use") || strings.Contains(msg, "address already") ||
		strings.Contains(msg, "denied by peer") || strings.Contains(msg, "bind")
}

// probePublicOK checks the tunnel entry point through the public URL.
func probePublicOK() bool {
	cli := &http.Client{Timeout: 6 * time.Second}
	resp, err := cli.Get(tunnelPublicURL + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	return resp.StatusCode == http.StatusOK
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
		getClient := func() (*devcloud.Client, error) {
			c, _, err := h.devcloudClient("")
			return c, err
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
			if err := h.tm.Start(getClient, persist); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			h.writeAudit(map[string]interface{}{
				"ts": time.Now().Format("2006-01-02T15:04:05.000-07:00"), "method": r.Method,
				"path": r.URL.Path, "action": "tunnel_" + body.Action,
				"remote": r.RemoteAddr,
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

// StartTunnelIfEnabled resumes the public tunnel at app startup when the
// user left it enabled (auto-resume, no external automation involved).
func (h *Handler) StartTunnelIfEnabled() {
	if h.tm == nil || h.store == nil {
		return
	}
	if h.store.GetSetting("cloud_tunnel_enabled") != "true" {
		return
	}
	getClient := func() (*devcloud.Client, error) {
		c, _, err := h.devcloudClient("")
		return c, err
	}
	if err := h.tm.Start(getClient, nil); err != nil {
		slog.Warn("tunnel: auto-start failed", "error", err)
	} else {
		slog.Info("tunnel: auto-started (cloud_tunnel_enabled=true)")
	}
}

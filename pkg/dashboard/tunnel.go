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
func (t *TunnelManager) run(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		targets := t.handler.tunnelTargets()
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
		for _, tgt := range targets {
			sshc, _, dialErr = tgt.client.DialSSH(tgt.projectID)
			if dialErr == nil {
				activeURL = tgt.publicURL
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

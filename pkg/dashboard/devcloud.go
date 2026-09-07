package dashboard

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/veenyi/XingyunAPI/pkg/devcloud"
	"github.com/veenyi/XingyunAPI/pkg/store"
	"golang.org/x/crypto/ssh"
)

// devcloudClient builds a devcloud API client from the requested account
// (user_id) or the default account when none is specified.
func (h *Handler) devcloudClient(accountUID string) (*devcloud.Client, *store.Account, error) {
	var account *store.Account
	if accountUID != "" {
		a, err := h.store.GetAccount(accountUID)
		if err != nil {
			return nil, nil, err
		}
		if a == nil {
			return nil, nil, fmt.Errorf("account %q not found", accountUID)
		}
		account = a
	} else {
		a, err := h.store.GetDefaultAccount()
		if err != nil {
			return nil, nil, err
		}
		if a == nil {
			return nil, nil, fmt.Errorf("no accounts configured")
		}
		account = a
	}
	return devcloud.NewClient(account.PtKey), account, nil
}

// devcloudProjectItem matches the fields the /cloud page consumes.
type devcloudProjectItem struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	AccountUserId  string `json:"accountUserId"`
	AccountDisplay string `json:"accountDisplay"`
	TemplateName   string `json:"templateName"`
	ServerInfo     string `json:"serverInfo"`
	ExternalURL    string `json:"externalUrl"`
	InternalURL    string `json:"internalUrl"`
	PodName        string `json:"podName"`
	CreatedAt      string `json:"createdAt"`
}

// templateNames caches GET /templates id → "Html 1.0.0" (TTL 1h). The catalog
// doubles as the projectTemplateId table — official ids drift, never hardcode.
var (
	tmplMu       sync.Mutex
	tmplNames    map[int]string
	tmplLoadedAt time.Time
)

func templateNameLookup(client *devcloud.Client) map[int]string {
	tmplMu.Lock()
	defer tmplMu.Unlock()
	if tmplNames != nil && time.Since(tmplLoadedAt) < time.Hour {
		return tmplNames
	}
	langs, err := client.ListTemplates()
	if err != nil {
		slog.Warn("devcloud templates fetch failed", "error", err)
		return tmplNames
	}
	m := map[int]string{}
	for _, l := range langs {
		for _, v := range l.Versions {
			m[v.ID] = l.Name + " " + v.Name
		}
	}
	if len(m) > 0 {
		tmplNames = m
		tmplLoadedAt = time.Now()
	}
	return tmplNames
}

// handleDevcloudProjects aggregates cloud hosts across all accounts.
func (h *Handler) handleDevcloudProjects(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	accounts, _ := h.store.ListAccounts()
	var out []devcloudProjectItem
	type acctErr struct {
		UserId  string `json:"userId"`
		Message string `json:"message"`
	}
	errs := []acctErr{}
	seen := map[string]bool{}
	for _, info := range accounts {
		a, err := h.store.GetAccount(info.UserID)
		if err != nil || a == nil || a.PtKey == "" {
			continue
		}
		if seen[a.PtKey] {
			continue
		}
		seen[a.PtKey] = true
		display := info.DisplayName()
		client := devcloud.NewClient(a.PtKey)
		projects, err := client.ListProjects()
		if err != nil {
			slog.Warn("devcloud list projects", "account", a.UserID, "error", err)
			errs = append(errs, acctErr{UserId: a.UserID, Message: err.Error()})
			continue
		}
		for _, p := range projects {
			item := devcloudProjectItem{
				ID:             p.ID,
				Name:           p.Name,
				Status:         strings.ToLower(p.Status),
				AccountUserId:  a.UserID,
				AccountDisplay: display,
				ServerInfo:     p.ServerInfo,
				ExternalURL:    p.URL,
				CreatedAt:      p.CreatedAt,
			}
			// 详情轮询补齐 podName / internalUrl / templateName（本身也是保活路径之一）
			tid := p.ProjectTemplateID
			if d, err := client.GetProject(p.ID); err == nil {
				item.TemplateName = d.ProjectTemplateName
				item.InternalURL = d.InternalURL
				item.PodName = d.PodName
				if d.ExternalURL != "" {
					item.ExternalURL = d.ExternalURL
				}
				if d.ProjectTemplateID != 0 {
					tid = d.ProjectTemplateID
				}
			}
			// API 直接创建的项目详情里没有模板名——用官方目录把 id 翻译成 "Html 1.0.0" 之类
			if item.TemplateName == "" {
				if m := templateNameLookup(client); m != nil {
					item.TemplateName = m[tid]
				}
			}
			out = append(out, item)
		}
	}
	if out == nil {
		out = []devcloudProjectItem{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"projects": out,
		"errors":   errs,
	})
}

// handleDevcloudCreate proxies project creation to the official devcloud API.
func (h *Handler) handleDevcloudCreate(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		AccountUserId   string `json:"accountUserId"`
		Name            string `json:"name"`
		TemplateID      int    `json:"templateId"`
		TemplateVersion string `json:"templateVersion"`
	}
	if !readJSONBody(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "项目名称不能为空")
		return
	}
	// templateVersion 已弃用：版本由 templateId（官方目录的版本 id）唯一决定
	in := devcloud.CreateInput{
		Name:              body.Name,
		AbbrName:          strings.ToUpper(body.Name[:1]),
		BgColor:           "#4293AE",
		DevMode:           1,
		ProjectTemplateID: body.TemplateID,
		ResourceCPU:       1,
		ResourceMemory:    "2Gi",
		ResourceStorage:   "10G",
	}
	client, _, err := h.devcloudClient(body.AccountUserId)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := client.CreateProject(in); err != nil {
		slog.Warn("devcloud: 创建项目失败", "account", body.AccountUserId, "name", body.Name, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	slog.Info("devcloud: project created", "account", body.AccountUserId, "name", body.Name, "template", body.TemplateID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// KeepDevCloudAlive polls every account's project list on a fixed interval —
// the official console treats listing/status polling as activity, which keeps
// idle cloud hosts from being reclaimed. Gated by the
// devcloud_keepalive_enabled setting (default on).
func (h *Handler) KeepDevCloudAlive() {
	interval := 10 * time.Minute
	go func() {
		for {
			if h.store.GetSetting("devcloud_keepalive_enabled") != "false" {
				accounts, _ := h.store.ListAccounts()
				seen := map[string]bool{}
				for _, info := range accounts {
					a, err := h.store.GetAccount(info.UserID)
					if err != nil || a == nil || a.PtKey == "" || seen[a.PtKey] {
						continue
					}
					seen[a.PtKey] = true
					client := devcloud.NewClient(a.PtKey)
					if _, err := client.ListProjects(); err != nil {
						slog.Warn("devcloud: 保活轮询失败", "account", a.UserID, "error", err)
					}
				}
			}
			time.Sleep(interval)
		}
	}()
}

// handleDevcloudProjectAction dispatches /api/devcloud/projects/{id}/{action}.
func (h *Handler) handleDevcloudProjectAction(w http.ResponseWriter, r *http.Request) {
	setCors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/devcloud/projects/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	action := parts[1]
	if action == "ssh-config" && r.Method == http.MethodGet {
		h.devcloudSSHConfig(w, r, id)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	client, account, err := h.devcloudClient(r.URL.Query().Get("account"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := client.Power(id, action); err != nil {
		slog.Warn("devcloud power action", "account", account.UserID, "project", id, "action", action, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	slog.Info("devcloud power action", "account", account.UserID, "project", id, "action", action)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (h *Handler) devcloudSSHConfig(w http.ResponseWriter, r *http.Request, id int) {
	client, _, err := h.devcloudClient(r.URL.Query().Get("account"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := client.SSHConfig(id)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ssh_config": cfg})
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleDevcloudWSSSH bridges a browser terminal to the project's SSH shell.
// WS protocol: binary frames carry terminal bytes; text frames carry control
// JSON such as {"type":"resize","cols":N,"rows":N}.
func (h *Handler) handleDevcloudWSSSH(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectID, err := strconv.Atoi(q.Get("project"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or invalid project")
		return
	}

	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	var wmu sync.Mutex
	writeOut := func(s string) error {
		wmu.Lock()
		defer wmu.Unlock()
		return ws.WriteMessage(websocket.BinaryMessage, []byte(s))
	}
	// Failures must be readable in the terminal: browsers often drop the
	// close reason, so a control frame alone leaves the user on a black pane.
	fail := func(msg string) {
		slog.Warn("devcloud ws ssh", "project", projectID, "error", msg)
		writeOut("\r\n\x1b[31m✗ " + msg + "\x1b[0m\r\n")
		ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(4000, msg), time.Now().Add(2*time.Second))
	}
	info := func(s string) {
		writeOut("\x1b[36m" + s + "\x1b[0m\r\n")
	}

	info(fmt.Sprintf("行云API · 云主机终端\r\n项目 %d 连接中…\r\n", projectID))

	client, account, err := h.devcloudClient(q.Get("account"))
	if err != nil {
		fail(err.Error())
		return
	}
	info("账号 " + account.UserID)

	if err := h.waitDevcloudRunning(client, projectID, info); err != nil {
		fail(err.Error())
		return
	}

	sshClient, cfg, err := client.DialSSH(projectID)
	if err != nil {
		fail("SSH 连接失败: " + err.Error())
		return
	}
	defer sshClient.Close()
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	info(fmt.Sprintf("SSH %s@%s:%d 已建立\r\n", cfg.User, cfg.Host, port))

	sess, err := sshClient.NewSession()
	if err != nil {
		fail("创建会话失败: " + err.Error())
		return
	}
	defer sess.Close()

	cols, rows := 120, 32
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		fail("申请 PTY 失败: " + err.Error())
		return
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		fail(err.Error())
		return
	}
	if err := sess.Shell(); err != nil {
		fail("启动 shell 失败: " + err.Error())
		return
	}
	slog.Info("devcloud ws ssh connected", "account", account.UserID, "project", projectID,
		"ssh", fmt.Sprintf("%s@%s:%d", cfg.User, cfg.Host, port))

	writeBin := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return ws.WriteMessage(websocket.BinaryMessage, b)
	}

	done := make(chan struct{})
	// ws → ssh stdin (+ control frames)
	go func() {
		defer close(done)
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				stdin.Close()
				return
			}
			if mt == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
				var ctl struct {
					Type string `json:"type"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" && ctl.Cols > 0 && ctl.Rows > 0 {
					sess.WindowChange(ctl.Rows, ctl.Cols)
					continue
				}
			}
			if _, err := stdin.Write(data); err != nil {
				return
			}
		}
	}()
	// ssh stdout → ws
	go func() {
		buf := make([]byte, 8192)
		cdSent := false
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				if writeBin(buf[:n]) != nil {
					return
				}
				if !cdSent {
					cdSent = true
					// 官方 IDE 把远端工作目录固定为 /config/workspace。
					_, _ = stdin.Write([]byte("cd /config/workspace 2>/dev/null\r"))
				}
			}
			if err != nil {
				ws.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "会话已结束"),
					time.Now().Add(2*time.Second))
				return
			}
		}
	}()

	<-done
}

// waitDevcloudRunning mirrors the official client: a project must be Running
// before its ssh-config is usable, so start it and poll for up to 60s.
func (h *Handler) waitDevcloudRunning(client *devcloud.Client, projectID int, info func(string)) error {
	detail, err := client.GetProject(projectID)
	if err != nil {
		return fmt.Errorf("查询项目状态失败: %w", err)
	}
	if strings.EqualFold(detail.Status, "Running") {
		return nil
	}
	info(fmt.Sprintf("当前状态 %s，发送开机指令…", detail.Status))
	if err := client.Power(projectID, "start"); err != nil {
		return fmt.Errorf("开机失败: %w", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		time.Sleep(500 * time.Millisecond)
		d, err := client.GetProject(projectID)
		if err != nil {
			continue
		}
		if strings.EqualFold(d.Status, "Running") {
			info("开机完成")
			return nil
		}
		if i%10 == 9 {
			info(fmt.Sprintf("等待开机中…（%s）", d.Status))
		}
	}
	return fmt.Errorf("等待开机超时（60s），请稍后重试或点「开机」")
}

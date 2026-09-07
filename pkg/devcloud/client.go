// Package devcloud is a client for the JoyCode devcloud (云主机) control API.
// The upstream API is the same one the JoyCode devcloud web console uses:
// JSON envelope {code, message, data} with auth via the "ptKey" header.
package devcloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://joycode-devcloud.jd.com/api/v1"

// Project mirrors a devcloud project record from POST /projects/my.
type Project struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	AbbrName          string `json:"abbrName"`
	BgColor           string `json:"bgColor"`
	DevMode           int    `json:"devMode"`
	ProjectTemplateID int    `json:"projectTemplateId"`
	Description       string `json:"description"`
	ServerInfo        string `json:"serverInfo"`
	DevboxID          int    `json:"devboxId"`
	Status            string `json:"status"`
	URL               string `json:"url"`
	CreatedAt         string `json:"createdAt"`
}

// SSHConfig is returned by GET /projects/{id}/ssh-config.
type SSHConfig struct {
	Host       string `json:"host"`
	User       string `json:"user"`
	Port       int    `json:"port"`
	PrivateKey string `json:"privateKey"` // base64-encoded PEM RSA private key
}

// Detail is the full project record returned by GET /projects/{id}.
type Detail struct {
	ID                     int    `json:"id"`
	DevMode                int    `json:"devMode"`
	Name                   string `json:"name"`
	AbbrName               string `json:"abbrName"`
	Description            string `json:"description"`
	ProjectTemplateID      int    `json:"projectTemplateId"`
	ProjectTemplateName    string `json:"projectTemplateName"`
	ProjectTemplateVersion string `json:"projectTemplateVersion"`
	Port                   int    `json:"port"`
	URL                    string `json:"url"`
	Status                 string `json:"status"`
	ServerInfo             string `json:"serverInfo"`
	CreatedAt              string `json:"createdAt"`
	ExternalURL            string `json:"externalUrl"`
	InternalURL            string `json:"internalUrl"`
	PodName                string `json:"podName"`
	OriginExternalURL      string `json:"originExternalUrl"`
}

// CreateInput is the payload for POST /projects (verified against the official
// console bundle: resourceMemory is sent as "<n>Gi", resourceStorage as "10G").
type CreateInput struct {
	Name                   string `json:"name"`
	AbbrName               string `json:"abbrName"`
	BgColor                string `json:"bgColor"`
	Description            string `json:"description,omitempty"`
	DevMode                int    `json:"devMode"`
	ProjectTemplateID      int    `json:"projectTemplateId"`
	ProjectTemplateVersion string `json:"projectTemplateVersion,omitempty"`
	ResourceCPU            int    `json:"resourceCPU"`
	ResourceMemory         string `json:"resourceMemory"`
	ResourceStorage        string `json:"resourceStorage"`
	Command                string `json:"command,omitempty"`
	Args                   string `json:"args,omitempty"`
}

// ProjectPage is the paginated list response.
type ProjectPage struct {
	Records []Project `json:"records"`
	Total   int       `json:"total"`
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type Client struct {
	BaseURL string
	PTKey   string
	HTTP    *http.Client
}

func NewClient(ptKey string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		PTKey:   ptKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("ptKey", c.PTKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("devcloud %s %s: HTTP %d, invalid JSON: %w", method, path, resp.StatusCode, err)
	}
	if env.Code != 200 {
		return fmt.Errorf("devcloud %s %s: code=%d %s", method, path, env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("devcloud %s %s: decode data: %w", method, path, err)
		}
	}
	return nil
}

// Template is one language entry from GET /templates. Its version ids double
// as the projectTemplateId values accepted by POST /projects — the official
// catalog drifts over time, so always resolve ids from here, never hardcode.
type Template struct {
	Name     string `json:"name"`
	Versions []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"versions"`
}

// ListTemplates returns the official template catalog.
func (c *Client) ListTemplates() ([]Template, error) {
	var out struct {
		Languages []Template `json:"languages"`
	}
	if err := c.do("GET", "/templates", nil, &out); err != nil {
		return nil, err
	}
	return out.Languages, nil
}

// ListProjects returns all projects (cloud hosts) for the account.
// The upstream honours a page wrapper; without it only the first (small)
// default page is returned, which silently hides extra hosts.
func (c *Client) ListProjects() ([]Project, error) {
	var page ProjectPage
	body := map[string]any{"page": map[string]int{"num": 1, "size": 100}}
	if err := c.do("POST", "/projects/my", body, &page); err != nil {
		return nil, err
	}
	return page.Records, nil
}

// Power sends a power action ("start", "stop" or "shutdown") to a project.
func (c *Client) Power(projectID int, action string) error {
	action = strings.ToLower(action)
	switch action {
	case "start", "stop", "shutdown":
	default:
		return fmt.Errorf("invalid power action %q", action)
	}
	return c.do("POST", fmt.Sprintf("/projects/%d/%s", projectID, action), map[string]any{}, nil)
}

// GetProject fetches the full detail record of one project.
func (c *Client) GetProject(projectID int) (*Detail, error) {
	var d Detail
	if err := c.do("GET", fmt.Sprintf("/projects/%d", projectID), nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateProject provisions a new cloud workspace.
func (c *Client) CreateProject(in CreateInput) (*Detail, error) {
	var d Detail
	if err := c.do("POST", "/projects", in, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// SSHConfig fetches per-project SSH connection details.
func (c *Client) SSHConfig(projectID int) (*SSHConfig, error) {
	var cfg SSHConfig
	if err := c.do("GET", fmt.Sprintf("/projects/%d/ssh-config", projectID), nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

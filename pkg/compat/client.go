// Package compat implements the generic OpenAI-compatible upstream client
// shared by the keyed/keyfree/freepool/custom channels: chat (stream and
// non-stream), model catalog fetching with backoff, upstream error
// classification and context compression for tight free tiers.
package compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/veenyi/XingyunAPI/pkg/health"
)

// Options configures one compat-backed channel.
type Options struct {
	Name       string // provider name surfaced in the UI
	BaseURL    string // e.g. https://api.groq.com/openai/v1
	APIKey     string // may be empty for keyless pools
	Models     []string
	Allowed    func(model string) bool
	HTTPClient *http.Client
	Tier       int // dispatch tier for route.Router
}

// Client is a reusable OpenAI-compatible chat client.
type Client struct {
	opts Options
	http *http.Client

	mu             sync.Mutex
	cachedIDs      []string
	catalogAt      time.Time
	backoffUntil   time.Time
	catalogRefresh time.Time
}

// New builds a client from options.
func New(opts Options) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{opts: opts, http: hc}
}

// Name implements provider.Keyless.
func (c *Client) Name() string { return c.opts.Name }

// Enabled implements provider.Keyless (always true; gating lives in the
// owning package via its Settings).
func (c *Client) Enabled() bool { return true }

// Tier implements provider.Catalog.
func (c *Client) Tier() int { return c.opts.Tier }

// Supports implements provider.Keyless: static list membership or a
// matching catalog id (prefix-stripped).
func (c *Client) Supports(model string) bool {
	for _, m := range c.visibleCatalog() {
		if m == model {
			return true
		}
	}
	return false
}

// ListModels implements provider.Keyless.
func (c *Client) ListModels() []string {
	return c.visibleCatalog()
}

// ModelIDs returns the raw catalog ids (unfiltered view for diagnostics).
func (c *Client) ModelIDs() []string {
	c.ensureCatalog()
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.cachedIDs...)
}

// CatalogIDs is an alias kept for the provider.Catalog surface.
func (c *Client) CatalogIDs() []string { return c.ModelIDs() }

// RefreshNow forces a catalog re-fetch, ignoring the backoff window.
func (c *Client) RefreshNow(ctx context.Context) {
	c.mu.Lock()
	c.backoffUntil = time.Time{}
	c.mu.Unlock()
	c.fetchCatalog()
}

func (c *Client) filterAllowed(models []string) []string {
	if c.opts.Allowed == nil {
		return models
	}
	out := models[:0:0]
	for _, m := range models {
		if c.opts.Allowed(m) {
			out = append(out, m)
		}
	}
	return out
}

// visibleCatalog merges the static model list with the fetched catalog.
func (c *Client) visibleCatalog() []string {
	c.mu.Lock()
	static := append([]string(nil), c.opts.Models...)
	cached := append([]string(nil), c.cachedIDs...)
	c.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, m := range append(static, cached...) {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return c.filterAllowed(out)
}

const catalogBackoff = 5 * time.Minute

// catalogFetchTimeout bounds a single catalog GET; the shared http.Client
// carries the chat streaming timeout (10min), far too long for a directory
// pull on the settings-save / refresh paths.
const catalogFetchTimeout = 10 * time.Second

// ensureCatalog fetches the model catalog if empty (or refetches daily),
// falling back to the last good list inside the backoff window.
func (c *Client) ensureCatalog() {
	c.mu.Lock()
	need := len(c.cachedIDs) == 0 && time.Now().After(c.backoffUntil)
	refresh := len(c.cachedIDs) > 0 && time.Since(c.catalogRefresh) > 24*time.Hour && time.Now().After(c.backoffUntil)
	c.mu.Unlock()
	if need || refresh {
		c.fetchCatalog()
	}
}

// fetchCatalog GETs {base}/models and caches the ids.
func (c *Client) fetchCatalog() {
	c.mu.Lock()
	inBackoff := time.Now().Before(c.backoffUntil)
	c.mu.Unlock()
	if inBackoff {
		return
	}
	url := strings.TrimSuffix(c.opts.BaseURL, "/") + "/models"
	ctx, cancel := context.WithTimeout(context.Background(), catalogFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return
	}
	c.applyAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		c.startBackoff()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.startBackoff()
		return
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		c.startBackoff()
		return
	}
	ids := []string{}
	for _, m := range payload.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	c.mu.Lock()
	c.cachedIDs = ids
	c.catalogAt = time.Now()
	c.catalogRefresh = time.Now()
	c.mu.Unlock()
}

func (c *Client) startBackoff() {
	c.mu.Lock()
	c.backoffUntil = time.Now().Add(catalogBackoff)
	c.mu.Unlock()
}

func (c *Client) applyAuth(req *http.Request) {
	if c.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "XingyunAPI/0.6.21")
}

// newRequest builds the chat completions request.
func (c *Client) newRequest(ctx context.Context, stream bool, body map[string]interface{}) (*http.Request, error) {
	body["stream"] = stream
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(c.opts.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	return req, nil
}

// failResp converts a non-200 upstream response into a classified error.
func (c *Client) failResp(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	text := Snippet(raw)
	err := fmt.Errorf("upstream %s: HTTP %d: %s", c.opts.Name, resp.StatusCode, text)
	if ra := health.ParseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
		return UpstreamError{Err: err, Status: resp.StatusCode, RetryAfter: ra}
	}
	if resp.StatusCode == http.StatusNotFound {
		return NotFoundError{Err: err}
	}
	return err
}

// Chat implements provider.Keyless (non-streaming).
func (c *Client) Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error) {
	body = CompressMessages(body)
	req, err := c.newRequest(ctx, false, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.failResp(resp)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("upstream %s: invalid JSON: %w", c.opts.Name, err)
	}
	return out, nil
}

// ChatStream implements provider.Keyless; returns the raw SSE body.
func (c *Client) ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error) {
	body = CompressMessages(body)
	req, err := c.newRequest(ctx, true, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, c.failResp(resp)
	}
	if err := guardSSEHead(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// guardSSEHead turns HTTP-200 JSON error bodies (which carry no SSE events)
// into errors so the router can fail over, instead of relaying a non-SSE
// payload that OpenAI clients would parse as an empty stream.
func guardSSEHead(resp *http.Response) error {
	br := bufio.NewReader(resp.Body)
	head, _ := br.Peek(256)
	if len(head) == 0 {
		return nil
	}
	if _, err := br.Discard(len(head)); err != nil {
		return nil
	}
	resp.Body = &peekedBody{Reader: io.MultiReader(bytes.NewReader(head), br), Closer: resp.Body}
	t := bytes.TrimLeft(head, " \t\r\n")
	if len(t) == 0 || t[0] != '{' {
		return nil
	}
	rest, _ := io.ReadAll(io.LimitReader(br, 64<<10))
	resp.Body.Close()
	payload := append(append([]byte{}, t...), rest...)
	var obj struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &obj) == nil && obj.Error.Message != "" {
		return fmt.Errorf("upstream error: %s", obj.Error.Message)
	}
	return fmt.Errorf("upstream error: %s", Snippet(payload))
}

type peekedBody struct {
	io.Reader
	io.Closer
}

// ClassifyError implements provider.Catalog.
func (c *Client) ClassifyError(err error) string { return health.Classify(err) }

// --- Error types & helpers ---

// UpstreamError carries an HTTP status and optional retry delay.
type UpstreamError struct {
	Err        error
	Status     int
	RetryAfter time.Duration
}

func (e UpstreamError) Error() string { return e.Err.Error() }
func (e UpstreamError) Unwrap() error { return e.Err }

// NotFoundError marks a model the upstream does not know.
type NotFoundError struct{ Err error }

func (e NotFoundError) Error() string { return e.Err.Error() }
func (e NotFoundError) Unwrap() error { return e.Err }

// UpstreamError wraps err with status/retry metadata.
func UpstreamErr(err error, status int, retryAfter time.Duration) error {
	return UpstreamError{Err: err, Status: status, RetryAfter: retryAfter}
}

// ErrText flattens an error into a bounded string.
func ErrText(err error) string {
	if err == nil {
		return ""
	}
	return Snippet([]byte(err.Error()))
}

// Snippet bounds and sanitizes an upstream body/error text for surfacing.
func Snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// LooksLikeError reports whether a decoded upstream body carries an error
// object (some providers answer 200 with {"error":{...}}).
func LooksLikeError(out map[string]interface{}) error {
	if out == nil {
		return nil
	}
	e, ok := out["error"]
	if !ok {
		return nil
	}
	switch v := e.(type) {
	case map[string]interface{}:
		msg, _ := v["message"].(string)
		if msg == "" {
			raw, _ := json.Marshal(v)
			msg = string(raw)
		}
		return fmt.Errorf("upstream error: %s", msg)
	case string:
		return fmt.Errorf("upstream error: %s", v)
	}
	return fmt.Errorf("upstream error: %v", e)
}

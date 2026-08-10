package anthropic

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
)

// Regression tests for issue #2 ("模型自动断开"): a stream that starts but ends
// before an upstream finish_reason must surface an error event, not a fake
// clean completion (stop_reason=end_turn + message_stop).

// rtFunc adapts a function to http.RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// errAfterReader yields data then returns a non-EOF error, simulating a
// mid-stream upstream read failure (scanner.Err() != nil).
type errAfterReader struct {
	data []byte
	err  error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if len(e.data) > 0 {
		n := copy(p, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	return 0, e.err
}

func (e *errAfterReader) Close() error { return nil }

// runStream wires a Handler whose upstream client returns the given SSE body
// (built fresh per request) and returns the SSE bytes the client receives.
func runStream(t *testing.T, bodyFn func() io.ReadCloser) string {
	t.Helper()
	client := joycode.NewClient("pt-test", "user-test")
	client.SetHTTPClient(&http.Client{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: bodyFn()}, nil
		}),
	})
	h := NewHandler(client, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	reqBody := `{"model":"GLM-5.1","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Body.String()
}

func bodyOf(s string) func() io.ReadCloser {
	return func() io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
}

// Clean EOF mid-generation (no finish_reason): must emit an error event and
// NOT a fake message_stop/end_turn.
func TestStream_PrematureCloseSurfacesError(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n"
	out := runStream(t, bodyOf(sse))

	if !strings.Contains(out, "event: error") {
		t.Errorf("expected an error event on premature close, got:\n%s", out)
	}
	if strings.Contains(out, "message_stop") {
		t.Errorf("must not fake message_stop on premature close, got:\n%s", out)
	}
	// message_start carries "stop_reason":null; the fake-completion marker is
	// specifically "stop_reason":"end_turn", which must NOT appear.
	if strings.Contains(out, "\"stop_reason\":\"end_turn\"") {
		t.Errorf("must not fake a clean end_turn on premature close, got:\n%s", out)
	}
	// Partial content already streamed should still be present.
	if !strings.Contains(out, "Hello") {
		t.Errorf("expected streamed partial content, got:\n%s", out)
	}
}

// Normal completion (finish_reason present): must emit end_turn + message_stop
// and NO error event. Guards against over-eagerly erroring on good streams.
func TestStream_NormalCompletionUnchanged(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n"
	out := runStream(t, bodyOf(sse))

	if strings.Contains(out, "event: error") {
		t.Errorf("normal completion must not emit an error event, got:\n%s", out)
	}
	if !strings.Contains(out, "\"stop_reason\":\"end_turn\"") {
		t.Errorf("expected stop_reason end_turn, got:\n%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Errorf("expected message_stop, got:\n%s", out)
	}
}

// Mid-stream read error before any finish_reason: must surface an error event.
func TestStream_ReadErrorSurfacesError(t *testing.T) {
	lines := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" wor\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ld\"}}]}\n"
	out := runStream(t, func() io.ReadCloser {
		return &errAfterReader{data: []byte(lines), err: io.ErrUnexpectedEOF}
	})

	if !strings.Contains(out, "event: error") {
		t.Errorf("expected an error event on mid-stream read error, got:\n%s", out)
	}
	if strings.Contains(out, "message_stop") {
		t.Errorf("must not fake message_stop on read error, got:\n%s", out)
	}
}

// Mid-stream content_filter finish_reason: surfaced as an error, not a silent
// end_turn. The filter marker is placed on the 3rd line so the connect-time
// peek (first two lines) doesn't intercept it.
func TestStream_ContentFilterSurfacesError(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"tial\"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n"
	out := runStream(t, bodyOf(sse))

	if !strings.Contains(out, "event: error") {
		t.Errorf("expected content_filter to surface an error event, got:\n%s", out)
	}
	if strings.Contains(out, "\"stop_reason\":\"end_turn\"") {
		t.Errorf("content_filter must not be disguised as end_turn, got:\n%s", out)
	}
}

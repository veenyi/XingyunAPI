package joycode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newWarmTestClient(ts *httptest.Server) *Client {
	c := NewClient("pt-key-test", "user1")
	c.ColorBaseURL = ts.URL
	c.Tenant = "JOYCODE"
	c.LoginType = "PIN_JD_CLOUD"
	c.SetTimeout(5 * time.Second)
	return c
}

func TestGrayDeniedGated(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":{"code":"AI_GRAY_ACCESS_DENIED","message":"访问受限，请联系管理员开通"}}`)
	}))
	defer ts.Close()

	c := newWarmTestClient(ts)
	denied, err := c.GrayDenied()
	if err != nil {
		t.Fatal(err)
	}
	if !denied {
		t.Fatal("expected denied=true for gray gate response")
	}
}

func TestGrayDeniedOpen(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	}))
	defer ts.Close()

	c := newWarmTestClient(ts)
	denied, err := c.GrayDenied()
	if err != nil {
		t.Fatal(err)
	}
	if denied {
		t.Fatal("expected denied=false for SSE chat response")
	}
}

func TestWarmTelemetry(t *testing.T) {
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn := r.URL.Query().Get("functionId")
		if fn == "" {
			t.Error("missing functionId")
		}
		if r.URL.Query().Get("sign") == "" {
			t.Errorf("missing sign for %s", fn)
		}
		if r.Header.Get("ptKey") != "pt-key-test" {
			t.Errorf("ptKey header = %q", r.Header.Get("ptKey"))
		}
		if r.Header.Get("loginType") != "PIN_JD_CLOUD" {
			t.Errorf("loginType header = %q", r.Header.Get("loginType"))
		}
		if r.Header.Get("tenant") != "JOYCODE" {
			t.Errorf("tenant header = %q", r.Header.Get("tenant"))
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body decode: %v", err)
		}
		got = append(got, fn)
		io.WriteString(w, `{"code":0}`)
	}))
	defer ts.Close()

	c := newWarmTestClient(ts)
	if err := c.WarmTelemetry(); err != nil {
		t.Fatal(err)
	}

	want := []string{"telemetry", "telemetry", "telemetry", "code_generation_metrics", "log_history", "get_newpoint_ide"}
	if len(got) != len(want) {
		t.Fatalf("got %d calls, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %s, want %s", i, got[i], want[i])
		}
	}
}

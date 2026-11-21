package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"git-event-hub/internal/sink"
	"git-event-hub/internal/sink/sinktest"
	"git-event-hub/pkg/prevents"
)

type capture struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	failAt  int // 1-based request number that answers 503; 0 for never
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, b)
	c.headers = append(c.headers, r.Header.Clone())
	n := len(c.bodies)
	c.mu.Unlock()
	if c.failAt == n {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func build(t *testing.T, settings map[string]any) sink.EventSink {
	t.Helper()
	s, err := New(sink.Spec{Name: "w", Settings: settings, Env: func(k string) string { return map[string]string{"TOKEN": "t0ken"}[k] }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSingleModeWithHMAC(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	s := build(t, map[string]any{"url": srv.URL, "auth": map[string]any{"type": "hmac", "secretEnv": "TOKEN"}})
	outs, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("a"), sinktest.Event("b")})
	if err != nil || len(outs) != 2 || outs[0].State != sink.Acked || outs[1].State != sink.Acked {
		t.Fatalf("outs = %+v err = %v", outs, err)
	}
	if len(c.bodies) != 2 {
		t.Fatalf("requests = %d, want one per event", len(c.bodies))
	}
	var e prevents.Event
	if err := json.Unmarshal(c.bodies[0], &e); err != nil || e.ID != "a" || e.Kind != prevents.KindOpened {
		t.Errorf("body is not the contract event: %s", c.bodies[0])
	}
	if got, want := c.headers[0].Get("X-Signature-256"), "sha256="+sink.HMACHex([]byte("t0ken"), c.bodies[0]); got != want {
		t.Errorf("signature = %s, want %s", got, want)
	}
}

func TestBatchModeWithBearerHeaderName(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	s := build(t, map[string]any{"url": srv.URL, "mode": "batch", "auth": map[string]any{"type": "bearer", "header": "X-Api-Key", "secretEnv": "TOKEN"}})
	if _, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("a"), sinktest.Event("b")}); err != nil {
		t.Fatal(err)
	}
	if len(c.bodies) != 1 {
		t.Fatalf("requests = %d, want one", len(c.bodies))
	}
	var b prevents.Batch
	if err := json.Unmarshal(c.bodies[0], &b); err != nil || len(b.Events) != 2 {
		t.Errorf("batch = %+v err = %v", b, err)
	}
	if got := c.headers[0].Get("X-Api-Key"); got != "Bearer t0ken" {
		t.Errorf("header = %q", got)
	}
}

func TestSingleModeStopsAtFirstFailure(t *testing.T) {
	c := &capture{failAt: 2}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	s := build(t, map[string]any{"url": srv.URL})
	outs, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("a"), sinktest.Event("b"), sinktest.Event("c")})
	if err != nil {
		t.Fatal(err)
	}
	// a was taken, b must be retried, c was not tried and has no outcome.
	if len(outs) != 2 || outs[0].State != sink.Acked || outs[1].State != sink.Retry || outs[1].ID != "b" {
		t.Fatalf("outs = %+v", outs)
	}
	if len(c.bodies) != 2 {
		t.Errorf("requests = %d, want 2", len(c.bodies))
	}
	if s.Health(context.Background()) == nil {
		t.Error("health must show the failure")
	}
}

func TestBatchFailureFailsWholeBatch(t *testing.T) {
	c := &capture{failAt: 1}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	s := build(t, map[string]any{"url": srv.URL, "mode": "batch"})
	if _, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("a")}); err == nil {
		t.Fatal("a 503 must fail the batch")
	}
}

func TestNewChecksSettings(t *testing.T) {
	tests := map[string]map[string]any{
		"no url":        {},
		"bad mode":      {"url": "https://x.example", "mode": "stream"},
		"bad auth":      {"url": "https://x.example", "auth": map[string]any{"type": "basic"}},
		"hmac no env":   {"url": "https://x.example", "auth": map[string]any{"type": "hmac"}},
		"env not set":   {"url": "https://x.example", "auth": map[string]any{"type": "bearer", "secretEnv": "NOPE"}},
		"unknown key":   {"url": "https://x.example", "retries": 3},
		"unknown inner": {"url": "https://x.example", "auth": map[string]any{"type": "none", "secret": "x"}},
		"env no type":   {"url": "https://x.example", "auth": map[string]any{"secretEnv": "TOKEN"}},
		"header none":   {"url": "https://x.example", "auth": map[string]any{"type": "none", "header": "X-Api-Key"}},
	}
	for name, settings := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(sink.Spec{Name: "w", Settings: settings, Env: func(string) string { return "" }}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

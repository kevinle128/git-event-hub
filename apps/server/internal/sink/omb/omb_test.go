package omb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"git-event-hub/internal/sink"
	"git-event-hub/internal/sink/sinktest"
	"git-event-hub/pkg/prevents"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func build(t *testing.T, url string) sink.EventSink {
	t.Helper()
	s, err := New(sink.Spec{Name: "omb", Settings: map[string]any{"url": url, "secretEnv": "SECRET"}, Env: env(map[string]string{"SECRET": "shared"})})
	if err != nil {
		t.Fatal(err)
	}
	s.(*Sink).now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return s
}

func TestDeliverSignsAndReadsAcks(t *testing.T) {
	var gotBody []byte
	var gotSig, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig, gotTS = r.Header.Get("X-Omb-Signature"), r.Header.Get("X-Omb-Timestamp")
		_, _ = w.Write([]byte(`{"acked":["e1"],"rejected":["e2"]}`))
	}))
	defer srv.Close()
	s := build(t, srv.URL)
	outs, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("e1"), sinktest.Event("e2"), sinktest.Event("e3")})
	if err != nil {
		t.Fatal(err)
	}
	want := []sink.State{sink.Acked, sink.Rejected, sink.Retry}
	for i, o := range outs {
		if o.State != want[i] {
			t.Errorf("event %d state = %v, want %v", i, o.State, want[i])
		}
	}
	if gotTS != strconv.Itoa(1_800_000_000) {
		t.Errorf("timestamp = %s", gotTS)
	}
	if gotSig != "sha256="+sink.HMACHex([]byte("shared"), []byte(gotTS+"."+string(gotBody))) {
		t.Errorf("signature does not cover timestamp and body")
	}
	var b prevents.Batch
	if err := json.Unmarshal(gotBody, &b); err != nil || b.V != 1 || len(b.Events) != 3 || b.Events[0].ID != "e1" {
		t.Errorf("batch = %+v err = %v", b, err)
	}
	if s.Health(context.Background()) != nil {
		t.Error("health must be ok after a good delivery")
	}
}

func TestDeliverFailures(t *testing.T) {
	status, answer := 500, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	s := build(t, srv.URL)
	if _, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("e1")}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("status 500: err = %v", err)
	}
	if s.Health(context.Background()) == nil {
		t.Error("health must fail after a failed delivery")
	}
	status, answer = 200, "<html>proxy page</html>"
	if _, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("e1")}); err == nil {
		t.Fatal("an answer that is not the ack JSON must fail the batch")
	}
	answer = `{"status":"ok"}`
	if _, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("e1")}); err == nil {
		t.Fatal("a JSON answer without acked or rejected must fail the batch")
	}
	status, answer = http.StatusFound, ""
	if _, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("e1")}); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("a redirect must fail the batch: err = %v", err)
	}
	srv.Close()
	if _, err := s.Deliver(context.Background(), []prevents.Event{sinktest.Event("e1")}); err == nil {
		t.Fatal("a connection error must fail the batch")
	}
}

func TestNewChecksSettings(t *testing.T) {
	good := map[string]string{"S": "x"}
	tests := map[string]map[string]any{
		"no url":         {"secretEnv": "S"},
		"relative url":   {"url": "/hooks", "secretEnv": "S"},
		"no secret name": {"url": "https://x.example"},
		"secret not set": {"url": "https://x.example", "secretEnv": "MISSING"},
		"typo in key":    {"url": "https://x.example", "secretEnv": "S", "secret_env": "S"},
	}
	for name, settings := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(sink.Spec{Name: "omb", Settings: settings, Env: env(good)}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

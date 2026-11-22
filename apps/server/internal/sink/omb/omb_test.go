package omb

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/sink"
	"git-event-hub/internal/sink/sinktest"
	"git-event-hub/pkg/prevents"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func build(t *testing.T, url string) sink.EventSink {
	t.Helper()
	s, err := New(sink.Spec{
		Name:      "omb",
		Settings:  map[string]any{"url": url, "secretEnv": "SECRET", "connector": "github-event"},
		Env:       env(map[string]string{"SECRET": "shared"}),
		Providers: []string{"github"}, BatchSize: 20,
	})
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
	var b struct {
		V      int               `json:"v"`
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(gotBody, &b); err != nil || b.V != 1 || len(b.Events) != 3 {
		t.Fatalf("batch = %+v err = %v", b, err)
	}
	for _, raw := range b.Events {
		if err := checkOMB(t, raw, "github-event", "github"); err != nil {
			t.Errorf("event does not follow OMB event contract v1: %v", err)
		}
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

func TestDeliverSignatureMatchesOMBVerifier(t *testing.T) {
	var gotBody []byte
	var gotSig, gotTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig, gotTS = r.Header.Get("X-Omb-Signature"), r.Header.Get("X-Omb-Timestamp")
		_, _ = w.Write([]byte(`{"acked":["e1"],"rejected":[]}`))
	}))
	defer srv.Close()
	_, err := build(t, srv.URL).Deliver(context.Background(), []prevents.Event{sinktest.Event("e1")})
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^\d{1,12}$`), gotTS)
	assert.Regexp(t, regexp.MustCompile(`^sha256=[0-9a-f]{64}$`), gotSig)
	mac := hmac.New(sha256.New, []byte("shared"))
	mac.Write([]byte(gotTS + "."))
	mac.Write(gotBody)
	assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), gotSig)
}

func TestDeliverAgainstAnOMBDoor(t *testing.T) {
	var mu sync.Mutex
	var problems []string
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fail := func(msg string) {
			mu.Lock()
			problems = append(problems, msg)
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
		}
		ts := r.Header.Get("X-Omb-Timestamp")
		mac := hmac.New(sha256.New, []byte("shared"))
		mac.Write([]byte(ts + "."))
		mac.Write(body)
		if r.Header.Get("X-Omb-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			fail("bad signature")
			return
		}
		var b struct {
			V      int               `json:"v"`
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(body, &b); err != nil || b.V != 1 || b.Events == nil {
			fail("body is not {v:1, events:[...]}")
			return
		}
		acked := []string{}
		for _, raw := range b.Events {
			var id struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &id)
			if err := checkOMB(t, raw, "github-event", "github"); err != nil {
				fail(id.ID + ": " + err.Error())
				return
			}
			acked = append(acked, id.ID)
		}
		out, _ := json.Marshal(map[string]any{"acked": acked, "rejected": []string{}})
		_, _ = w.Write(out)
	}))
	defer door.Close()
	var batch []prevents.Event
	for _, c := range fixtureCases(prevents.GitHub) {
		batch = append(batch, c.event)
	}
	outs, err := build(t, door.URL).Deliver(context.Background(), batch)
	require.NoError(t, err, "door problems: %v", problems)
	require.Len(t, outs, 8)
	for _, o := range outs {
		assert.Equal(t, sink.Acked, o.State, o.ID)
	}
}

func TestDeliverReadsRejectedObjectsWithReasons(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"acked":["e1"],"rejected":[{"id":"e2","reason":"type x is not emitted"},"e3"]}`))
	}))
	defer srv.Close()
	outs, err := build(t, srv.URL).Deliver(context.Background(),
		[]prevents.Event{sinktest.Event("e1"), sinktest.Event("e2"), sinktest.Event("e3")})
	require.NoError(t, err)
	require.Len(t, outs, 3)
	assert.Equal(t, sink.Acked, outs[0].State)
	assert.Equal(t, sink.Rejected, outs[1].State)
	assert.Equal(t, "rejected by omb: type x is not emitted", outs[1].Err)
	assert.Equal(t, sink.Rejected, outs[2].State)
	assert.Equal(t, "rejected by omb", outs[2].Err)
}

func TestDeliverFailsOnAMalformedRejectedItem(t *testing.T) {
	for _, item := range []string{`42`, `null`, `""`, `{"id":""}`, `{"id":null}`} {
		t.Run(item, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"acked":[],"rejected":[` + item + `]}`))
			}))
			defer srv.Close()
			_, err := build(t, srv.URL).Deliver(context.Background(), []prevents.Event{sinktest.Event("e1")})
			require.Error(t, err)
		})
	}
}

func gitlabEvent(id string) prevents.Event {
	e := sinktest.Event(id)
	e.Provider = prevents.GitLab
	return e
}

func TestDeliverRejectsTheOtherForgeWithoutSending(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b struct {
			Events []struct {
				ID string `json:"id"`
			} `json:"events"`
		}
		_ = json.Unmarshal(body, &b)
		for _, e := range b.Events {
			seen = append(seen, e.ID)
		}
		_, _ = w.Write([]byte(`{"acked":["gh"],"rejected":[]}`))
	}))
	defer srv.Close()
	outs, err := build(t, srv.URL).Deliver(context.Background(), []prevents.Event{sinktest.Event("gh"), gitlabEvent("gl")})
	require.NoError(t, err)
	assert.Equal(t, []string{"gh"}, seen)
	require.Len(t, outs, 2)
	assert.Equal(t, sink.Acked, outs[0].State)
	assert.Equal(t, sink.Rejected, outs[1].State)
	assert.Contains(t, outs[1].Err, "provider gitlab does not belong to connector github-event")
}

func TestDeliverMakesNoCallWhenNothingMaps(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	outs, err := build(t, srv.URL).Deliver(context.Background(), []prevents.Event{gitlabEvent("a"), gitlabEvent("b")})
	require.NoError(t, err)
	assert.Zero(t, calls)
	require.Len(t, outs, 2)
	for _, o := range outs {
		assert.Equal(t, sink.Rejected, o.State)
	}
}

func TestNewChecksSettings(t *testing.T) {
	good := map[string]string{"S": "x"}
	base := func(over map[string]any) map[string]any {
		m := map[string]any{"url": "https://x.example", "secretEnv": "S", "connector": "github-event"}
		for k, v := range over {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	gh := []string{"github"}
	tests := map[string]struct {
		settings  map[string]any
		providers []string
		batch     int
		want      string
	}{
		"no url":                   {base(map[string]any{"url": nil}), gh, 20, ""},
		"relative url":             {base(map[string]any{"url": "/hooks"}), gh, 20, ""},
		"no secret name":           {base(map[string]any{"secretEnv": nil}), gh, 20, ""},
		"secret not set":           {base(map[string]any{"secretEnv": "MISSING"}), gh, 20, ""},
		"typo in key":              {base(map[string]any{"secret_env": "S"}), gh, 20, ""},
		"no connector":             {base(map[string]any{"connector": nil}), gh, 20, "connector must be github-event or gitlab-event"},
		"unknown connector":        {base(map[string]any{"connector": "omb-event"}), gh, 20, "connector must be github-event or gitlab-event"},
		"no providers filter":      {base(nil), nil, 20, "connector github-event needs filter.providers: [github] on this sink"},
		"providers of other forge": {base(nil), []string{"gitlab"}, 20, "connector github-event needs filter.providers: [github] on this sink"},
		"two providers":            {base(nil), []string{"github", "gitlab"}, 20, "needs filter.providers: [github]"},
		"batch size above 50":      {base(nil), gh, 51, "delivery.batchSize is 51; OMB takes at most 50 events per request"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := New(sink.Spec{Name: "omb", Settings: tc.settings, Env: env(good), Providers: tc.providers, BatchSize: tc.batch})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	for connector, forge := range map[string]string{"github-event": "github", "gitlab-event": "gitlab"} {
		t.Run("good "+connector, func(t *testing.T) {
			_, err := New(sink.Spec{Name: "omb", Settings: base(map[string]any{"connector": connector}), Env: env(good),
				Providers: []string{forge}, BatchSize: 50})
			require.NoError(t, err)
		})
	}
}

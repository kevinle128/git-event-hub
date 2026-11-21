// Package omb delivers events to OpenMausBot. It sends a batch as JSON, signs
// it with a shared secret, and reads which event ids OpenMausBot took.
//
// Request: POST url, body {"v":1,"events":[...]}, with the headers
//
//	X-Omb-Timestamp: <unix seconds>
//	X-Omb-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">
//
// Answer: 200 with {"acked":[ids],"rejected":[ids]}. An id in neither list
// is sent again later. Any other status sends the whole batch again.
package omb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"git-event-hub/internal/sink"
	"git-event-hub/pkg/prevents"
)

// Type is the adapter name for the configuration.
const Type = "omb"

func init() { sink.Register(Type, New) }

type settings struct {
	URL       string `mapstructure:"url"`
	SecretEnv string `mapstructure:"secretEnv"`
}

// Sink is the OpenMausBot adapter.
type Sink struct {
	sink.Tracker
	url    string
	secret []byte
	now    func() time.Time
}

// New builds the adapter.
func New(spec sink.Spec) (sink.EventSink, error) {
	var s settings
	if err := sink.DecodeSettings(spec.Settings, &s); err != nil {
		return nil, err
	}
	if err := sink.ValidateURL(s.URL); err != nil {
		return nil, err
	}
	if s.SecretEnv == "" || spec.Env(s.SecretEnv) == "" {
		return nil, fmt.Errorf("secretEnv must name an environment variable that is set")
	}
	return &Sink{url: s.URL, secret: []byte(spec.Env(s.SecretEnv)), now: time.Now}, nil
}

type answer struct {
	Acked    []string `json:"acked"`
	Rejected []string `json:"rejected"`
}

// Deliver sends the batch.
func (s *Sink) Deliver(ctx context.Context, batch []prevents.Event) ([]sink.Outcome, error) {
	out, err := s.deliver(ctx, batch)
	s.Record(err)
	return out, err
}

func (s *Sink) deliver(ctx context.Context, batch []prevents.Event) ([]sink.Outcome, error) {
	body, err := json.Marshal(prevents.Batch{V: prevents.Version, Events: batch})
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	sig := sink.HMACHex(s.secret, []byte(ts+"."+string(body)))
	h := http.Header{}
	h.Set("X-Omb-Timestamp", ts)
	h.Set("X-Omb-Signature", "sha256="+sig)
	status, data, err := sink.Post(ctx, s.url, body, h)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("omb answered status %d", status)
	}
	var a answer
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, errors.New("omb answer is not the expected JSON")
	}
	state := map[string]sink.State{}
	for _, id := range a.Acked {
		state[id] = sink.Acked
	}
	for _, id := range a.Rejected {
		state[id] = sink.Rejected
	}
	outs := make([]sink.Outcome, 0, len(batch))
	for _, e := range batch {
		st, ok := state[e.ID]
		o := sink.Outcome{ID: e.ID, State: st}
		if !ok {
			o.State, o.Err = sink.Retry, "not acknowledged"
		} else if st == sink.Rejected {
			o.Err = "rejected by omb"
		}
		outs = append(outs, o)
	}
	return outs, nil
}

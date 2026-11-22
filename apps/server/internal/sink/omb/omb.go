// Package omb delivers events to OpenMausBot. It maps each pr-event v1 event
// to OMB event contract v1 when it sends a batch, signs the batch with a
// shared secret, and reads which event ids OpenMausBot took. OMB owns the
// contract; the hub keeps a copy for its tests in testdata/omb-contract.
//
// One sink serves one OMB connector ("github-event" or "gitlab-event"), and
// each connector takes one forge. The configuration must therefore set
// filter.providers to that forge only, and delivery.batchSize to 50 or less,
// because OMB takes at most 50 events per request.
//
// Request: POST url, body {"v":1,"events":[<OMB event contract v1>...]},
// with the headers
//
//	X-Omb-Timestamp: <unix seconds>
//	X-Omb-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">
//
// Answer: 200 with {"acked":[ids],"rejected":[ids]}. A rejected item may also
// be {"id":..., "reason":...}. An id in neither list is sent again later.
// Any other status sends the whole batch again.
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

// maxBatch is the most events that OMB takes in one request. A comment body
// can be 32 KiB, so a larger batch can pass the body limit of OMB and get
// status 413 on every retry.
const maxBatch = 50

type settings struct {
	URL       string `mapstructure:"url"`
	SecretEnv string `mapstructure:"secretEnv"`
	Connector string `mapstructure:"connector"`
}

// Sink is the OpenMausBot adapter.
type Sink struct {
	sink.Tracker
	url       string
	secret    []byte
	connector string
	now       func() time.Time
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
	forge, ok := forgeOf[s.Connector]
	if !ok {
		return nil, errors.New("connector must be github-event or gitlab-event")
	}
	if len(spec.Providers) != 1 || spec.Providers[0] != string(forge) {
		return nil, fmt.Errorf("connector %s needs filter.providers: [%s] on this sink", s.Connector, forge)
	}
	if spec.BatchSize > maxBatch {
		return nil, fmt.Errorf("delivery.batchSize is %d; OMB takes at most %d events per request", spec.BatchSize, maxBatch)
	}
	return &Sink{url: s.URL, secret: []byte(spec.Env(s.SecretEnv)), connector: s.Connector, now: time.Now}, nil
}

// answer is the body of a 200 from omb. A field that is missing stays nil,
// so an unrelated JSON body is told apart from an empty answer. A rejected
// item is a string id, or an object with the id and a reason.
type answer struct {
	Acked    *[]string          `json:"acked"`
	Rejected *[]json.RawMessage `json:"rejected"`
}

type rejection struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// parseRejected reads one item of the rejected list.
func parseRejected(raw json.RawMessage) (rejection, error) {
	var id string
	if json.Unmarshal(raw, &id) == nil && id != "" {
		return rejection{ID: id}, nil
	}
	var r rejection
	if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" {
		return rejection{}, errors.New("omb answer is not the expected JSON")
	}
	return r, nil
}

// Deliver sends the batch.
func (s *Sink) Deliver(ctx context.Context, batch []prevents.Event) ([]sink.Outcome, error) {
	out, err := s.deliver(ctx, batch)
	s.Record(err)
	return out, err
}

func (s *Sink) deliver(ctx context.Context, batch []prevents.Event) ([]sink.Outcome, error) {
	local := map[string]sink.Outcome{} // events that cannot be mapped
	mapped := make([]ombEvent, 0, len(batch))
	for _, e := range batch {
		m, err := toOMB(e, s.connector)
		if err != nil {
			local[e.ID] = sink.Outcome{ID: e.ID, State: sink.Rejected, Err: err.Error()}
			continue
		}
		mapped = append(mapped, m)
	}
	remote := map[string]sink.Outcome{}
	if len(mapped) > 0 {
		var err error
		if remote, err = s.send(ctx, mapped); err != nil {
			return nil, err
		}
	}
	outs := make([]sink.Outcome, 0, len(batch))
	for _, e := range batch {
		if o, ok := local[e.ID]; ok {
			outs = append(outs, o)
		} else if o, ok := remote[e.ID]; ok {
			outs = append(outs, o)
		} else {
			outs = append(outs, sink.Outcome{ID: e.ID, State: sink.Retry, Err: "not acknowledged"})
		}
	}
	return outs, nil
}

// send posts the mapped events and reads the answer for each id.
func (s *Sink) send(ctx context.Context, events []ombEvent) (map[string]sink.Outcome, error) {
	body, err := json.Marshal(ombBatch{V: prevents.Version, Events: events})
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
	if err := json.Unmarshal(data, &a); err != nil || (a.Acked == nil && a.Rejected == nil) {
		return nil, errors.New("omb answer is not the expected JSON")
	}
	out := map[string]sink.Outcome{}
	if a.Acked != nil {
		for _, id := range *a.Acked {
			out[id] = sink.Outcome{ID: id, State: sink.Acked}
		}
	}
	if a.Rejected != nil {
		for _, raw := range *a.Rejected {
			r, err := parseRejected(raw)
			if err != nil {
				return nil, err
			}
			msg := "rejected by omb"
			if r.Reason != "" {
				msg += ": " + r.Reason
			}
			out[r.ID] = sink.Outcome{ID: r.ID, State: sink.Rejected, Err: msg}
		}
	}
	return out, nil
}

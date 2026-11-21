// Package webhook delivers events to any HTTP endpoint. Each request carries
// the contract event as JSON, either one event per request or a batch.
//
// Settings:
//
//	url:  absolute http or https URL
//	mode: single (default) sends the event itself; batch sends {"v":1,"events":[...]}
//	auth:
//	  type: none (default) | hmac | bearer
//	  header: header name; default X-Signature-256 for hmac, Authorization for bearer
//	  secretEnv: environment variable that holds the secret
//
// With hmac the header value is "sha256=" and the hex HMAC-SHA256 of the
// body. With bearer it is "Bearer <secret>". Any 2xx answer means the event
// was taken; any other answer means retry.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"git-event-hub/internal/sink"
	"git-event-hub/pkg/prevents"
)

// Type is the adapter name for the configuration.
const Type = "webhook"

func init() { sink.Register(Type, New) }

type settings struct {
	URL  string `mapstructure:"url"`
	Mode string `mapstructure:"mode"`
	Auth struct {
		Type      string `mapstructure:"type"`
		Header    string `mapstructure:"header"`
		SecretEnv string `mapstructure:"secretEnv"`
	} `mapstructure:"auth"`
}

// Sink is the generic webhook adapter.
type Sink struct {
	sink.Tracker
	url    string
	batch  bool
	auth   string
	header string
	secret []byte
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
	out := &Sink{url: s.URL, auth: s.Auth.Type, header: s.Auth.Header}
	switch s.Mode {
	case "", "single":
	case "batch":
		out.batch = true
	default:
		return nil, fmt.Errorf("mode %q is not single or batch", s.Mode)
	}
	switch out.auth {
	case "", "none":
		out.auth = "none"
	case "hmac", "bearer":
		if s.Auth.SecretEnv == "" || spec.Env(s.Auth.SecretEnv) == "" {
			return nil, errors.New("auth.secretEnv must name an environment variable that is set")
		}
		out.secret = []byte(spec.Env(s.Auth.SecretEnv))
		if out.header == "" {
			out.header = map[string]string{"hmac": "X-Signature-256", "bearer": "Authorization"}[out.auth]
		}
	default:
		return nil, fmt.Errorf("auth.type %q is not none, hmac or bearer", out.auth)
	}
	return out, nil
}

func (s *Sink) headers(body []byte) http.Header {
	h := http.Header{}
	switch s.auth {
	case "hmac":
		h.Set(s.header, "sha256="+sink.HMACHex(s.secret, body))
	case "bearer":
		h.Set(s.header, "Bearer "+string(s.secret))
	}
	return h
}

// Deliver sends the batch as one request or as one request per event.
func (s *Sink) Deliver(ctx context.Context, batch []prevents.Event) ([]sink.Outcome, error) {
	var (
		outs []sink.Outcome
		err  error
	)
	if s.batch {
		outs, err = s.sendBatch(ctx, batch)
	} else {
		outs = s.sendEach(ctx, batch)
	}
	s.Record(firstFailure(outs, err))
	return outs, err
}

func firstFailure(outs []sink.Outcome, err error) error {
	if err != nil {
		return err
	}
	for _, o := range outs {
		if o.State != sink.Acked {
			return errors.New(o.Err)
		}
	}
	return nil
}

func (s *Sink) post(ctx context.Context, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	status, _, err := sink.Post(ctx, s.url, body, s.headers(body))
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("endpoint answered status %d", status)
	}
	return nil
}

func (s *Sink) sendBatch(ctx context.Context, batch []prevents.Event) ([]sink.Outcome, error) {
	if err := s.post(ctx, prevents.Batch{V: prevents.Version, Events: batch}); err != nil {
		return nil, err
	}
	outs := make([]sink.Outcome, len(batch))
	for i, e := range batch {
		outs[i] = sink.Outcome{ID: e.ID, State: sink.Acked}
	}
	return outs, nil
}

// sendEach stops at the first failure and leaves the rest without an
// outcome, so they stay in order for the next try.
func (s *Sink) sendEach(ctx context.Context, batch []prevents.Event) []sink.Outcome {
	outs := make([]sink.Outcome, 0, len(batch))
	for _, e := range batch {
		if err := s.post(ctx, e); err != nil {
			outs = append(outs, sink.Outcome{ID: e.ID, State: sink.Retry, Err: err.Error()})
			break
		}
		outs = append(outs, sink.Outcome{ID: e.ID, State: sink.Acked})
	}
	return outs
}

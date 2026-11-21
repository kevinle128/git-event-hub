// Package githubapp reads webhooks of a GitHub App. GitHub signs each body
// with the webhook secret of the App (X-Hub-Signature-256).
package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"git-event-hub/internal/normalize"
	"git-event-hub/internal/source"
	"git-event-hub/pkg/prevents"
)

// Type is the adapter name for the configuration.
const Type = "githubapp"

func init() { source.Register(Type, New) }

// Source is the GitHub App adapter.
type Source struct {
	id     string
	secret []byte
}

// New builds the adapter. The secret is required.
func New(s source.Settings) (source.WebhookSource, error) {
	if s.Secret == "" {
		return nil, source.ErrNoSecret
	}
	return &Source{id: s.ID, secret: []byte(s.Secret)}, nil
}

func (s *Source) ID() string                  { return s.id }
func (s *Source) Provider() prevents.Provider { return prevents.GitHub }

// Verify checks X-Hub-Signature-256 against HMAC-SHA256 of the raw body.
func (s *Source) Verify(r *source.Request) error {
	got := r.Header.Get("X-Hub-Signature-256")
	if got == "" {
		return source.ErrNoSignature
	}
	hexSum, ok := strings.CutPrefix(got, "sha256=")
	if !ok {
		return source.ErrBadSignature
	}
	sum, err := hex.DecodeString(hexSum)
	if err != nil {
		return source.ErrBadSignature
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(r.Body)
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return source.ErrBadSignature
	}
	return nil
}

// DeliveryID returns X-GitHub-Delivery. A redelivery keeps the same value.
func (s *Source) DeliveryID(r *source.Request) (string, error) {
	if id := r.Header.Get("X-GitHub-Delivery"); id != "" {
		return id, nil
	}
	return "", source.ErrNoDeliveryID
}

// Parse maps the delivery to events.
func (s *Source) Parse(r *source.Request, c normalize.Context) ([]prevents.Event, error) {
	return normalize.GitHub(r.Header.Get("X-GitHub-Event"), r.Body, c)
}

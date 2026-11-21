// Package gitlab reads GitLab project and group webhooks. GitLab sends the
// secret token that you set on the webhook in the X-Gitlab-Token header.
package gitlab

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"git-event-hub/internal/normalize"
	"git-event-hub/internal/source"
	"git-event-hub/pkg/prevents"
)

// Type is the adapter name for the configuration.
const Type = "gitlab"

func init() { source.Register(Type, New) }

// Source is the GitLab adapter.
type Source struct {
	id     string
	tokens [][32]byte // SHA-256 of each accepted token
}

// New builds the adapter. The secret holds one token, or several separated by
// commas, one for each webhook. Which merge requests pass is decided later by
// the subscribers, not by the source.
func New(s source.Settings) (source.WebhookSource, error) {
	if s.Secret == "" {
		return nil, source.ErrNoSecret
	}
	src := &Source{id: s.ID}
	for _, t := range strings.Split(s.Secret, ",") {
		if t = strings.TrimSpace(t); t != "" {
			src.tokens = append(src.tokens, sha256.Sum256([]byte(t)))
		}
	}
	if len(src.tokens) == 0 {
		return nil, source.ErrNoSecret
	}
	return src, nil
}

func (s *Source) ID() string                  { return s.id }
func (s *Source) Provider() prevents.Provider { return prevents.GitLab }

// Verify compares X-Gitlab-Token with each accepted token in constant time.
func (s *Source) Verify(r *source.Request) error {
	got := r.Header.Get("X-Gitlab-Token")
	if got == "" {
		return source.ErrNoSignature
	}
	sum := sha256.Sum256([]byte(got))
	ok := 0
	for _, t := range s.tokens {
		ok |= subtle.ConstantTimeCompare(sum[:], t[:])
	}
	if ok != 1 {
		return source.ErrBadSignature
	}
	return nil
}

// DeliveryID returns Idempotency-Key or X-Gitlab-Event-UUID, which GitLab
// keeps the same for a retry. An old GitLab sends neither; then the id comes
// from the body, so an identical retry is still recognised.
func (s *Source) DeliveryID(r *source.Request) (string, error) {
	for _, h := range []string{"Idempotency-Key", "X-Gitlab-Event-UUID"} {
		if id := r.Header.Get(h); id != "" {
			return id, nil
		}
	}
	sum := sha256.Sum256(r.Body)
	return "body-" + hex.EncodeToString(sum[:16]), nil
}

// Parse maps the delivery to events.
func (s *Source) Parse(r *source.Request, c normalize.Context) ([]prevents.Event, error) {
	return normalize.GitLab(r.Body, c)
}

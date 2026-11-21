// Package source defines the inbound port of the hub: a WebhookSource
// checks and reads the webhooks of one forge. Each adapter lives in its own
// package and registers itself, so a new forge needs one package and one
// blank import in internal/app.
package source

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"git-event-hub/internal/normalize"
	"git-event-hub/pkg/prevents"
)

// Request is a webhook request as the adapter sees it.
type Request struct {
	Method string
	Header http.Header
	// Body is the raw body. The signature covers these exact bytes.
	Body []byte
}

// Verify errors. The handler answers 401 for all of them.
var (
	ErrNoSecret     = errors.New("the source has no secret configured")
	ErrNoSignature  = errors.New("the request has no signature")
	ErrBadSignature = errors.New("the signature does not match")
	ErrNoDeliveryID = errors.New("the request has no delivery id")
)

// WebhookSource is the port for one forge.
type WebhookSource interface {
	// ID is the source id from the configuration; it is the URL segment of
	// /hooks/<id>.
	ID() string
	// Provider is the forge that the source reads.
	Provider() prevents.Provider
	// Verify checks the signature or token of the request. It must not read
	// the body as JSON.
	Verify(r *Request) error
	// DeliveryID returns the id that the forge gives the delivery. A repeated
	// delivery has the same id.
	DeliveryID(r *Request) (string, error)
	// Parse turns a verified request into events. A request that holds
	// nothing of interest gives no events, and the adapter says why through
	// c.Drop. A payload with the wrong shape gives an error that wraps
	// normalize.ErrPayload.
	Parse(r *Request, c normalize.Context) ([]prevents.Event, error)
}

// Settings are the values that an adapter needs from the configuration.
type Settings struct {
	ID     string
	Secret string
}

// Factory builds a source from its settings.
type Factory func(Settings) (WebhookSource, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds an adapter type. Adapters call it from init.
func Register(typ string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[typ]; dup {
		panic("source type registered twice: " + typ)
	}
	factories[typ] = f
}

// New builds a source of the given adapter type.
func New(typ string, s Settings) (WebhookSource, error) {
	mu.RLock()
	f, ok := factories[typ]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown source type %q (known: %v)", typ, Types())
	}
	return f(s)
}

// Types lists the registered adapter types.
func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Package sink defines the outbound port of the hub: an EventSink takes
// batches of events and says, event by event, whether it took them. Each
// adapter lives in its own package and registers itself, so a new consumer
// needs one package and one blank import in internal/app.
package sink

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/go-viper/mapstructure/v2"

	"git-event-hub/pkg/prevents"
)

// State is what a sink says about one event.
type State int

const (
	// Acked means the consumer has the event. The hub never sends it again.
	Acked State = iota
	// Retry means the consumer does not have the event yet. The hub sends
	// it again after a backoff.
	Retry
	// Rejected means the consumer refuses the event for good, for example
	// because it is invalid. The hub stops sending it.
	Rejected
)

func (s State) String() string {
	switch s {
	case Acked:
		return "acked"
	case Rejected:
		return "rejected"
	}
	return "retry"
}

// Outcome is the answer for one event of a batch.
type Outcome struct {
	ID    string
	State State
	// Err says why the state is Retry or Rejected.
	Err string
}

// EventSink is the port for one consumer.
type EventSink interface {
	// Deliver sends the batch. It returns one Outcome per event; the hub
	// treats an event without an Outcome as Retry, unless an earlier event of
	// the batch did not finish: then the event counts as untried and keeps
	// its attempts. A non-nil error means the
	// whole batch failed and must be sent again. Deliver must be safe to
	// call again with events it has already seen: the consumer dedupes by
	// event id.
	Deliver(ctx context.Context, batch []prevents.Event) ([]Outcome, error)
	// Health says whether the last delivery worked. It does not call the
	// consumer, so a quiet sink does not cause traffic.
	Health(ctx context.Context) error
}

// Spec holds the configuration of one sink for its factory.
type Spec struct {
	Name string
	// Settings are the keys of the sink entry that the hub does not
	// know. Use DecodeSettings to read them.
	Settings map[string]any
	// Env reads an environment variable. Secrets come only from here.
	Env func(string) string
}

// Factory builds a sink. It must reject settings it does not understand.
type Factory func(Spec) (EventSink, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds an adapter type. Adapters call it from init.
func Register(typ string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[typ]; dup {
		panic("sink type registered twice: " + typ)
	}
	factories[typ] = f
}

// New builds a sink of the given adapter type.
func New(typ string, s Spec) (EventSink, error) {
	mu.RLock()
	f, ok := factories[typ]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown sink type %q (known: %v)", typ, Types())
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

// DecodeSettings reads the settings map into a struct with mapstructure tags
// and rejects unknown keys, so a typo in the configuration fails at load time.
// Key names match without regard to case, because the config loader
// lowercases them.
func DecodeSettings(settings map[string]any, into any) error {
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:      into,
		ErrorUnused: true,
		DecodeHook:  mapstructure.StringToTimeDurationHookFunc(),
	})
	if err != nil {
		return err
	}
	return dec.Decode(settings)
}

// Tracker remembers the result of the last delivery. Adapters embed it to
// implement Health.
type Tracker struct {
	last atomic.Pointer[error]
}

// Record stores the result of a delivery.
func (t *Tracker) Record(err error) { t.last.Store(&err) }

// Health returns the error of the last delivery, or nil.
func (t *Tracker) Health(context.Context) error {
	if p := t.last.Load(); p != nil {
		return *p
	}
	return nil
}

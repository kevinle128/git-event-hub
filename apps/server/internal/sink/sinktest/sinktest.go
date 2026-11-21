// Package sinktest holds helpers for tests of sinks and of the outbox.
package sinktest

import (
	"context"
	"errors"
	"sync"
	"time"

	"git-event-hub/internal/sink"
	"git-event-hub/pkg/prevents"
)

// Event returns a valid event with the given id.
func Event(id string) prevents.Event {
	head := "0bc93cc2e4122a993e4a73c4531beae821cddd67"
	return prevents.Event{
		V: 1, ID: id, Provider: prevents.GitHub, Repo: "kevinle128/demo", Number: 7, Kind: prevents.KindOpened,
		HeadSHA: &head, OccurredAt: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC),
		Data: prevents.OpenedData{Title: "demo", URL: "https://example.test/pr/7"},
	}
}

// Fake is an EventSink that tests control. While down, every delivery fails.
// Received lists the id of every event that Deliver saw, in order, also those
// of failed calls: a real consumer may have stored them.
type Fake struct {
	sink.Tracker
	mu       sync.Mutex
	down     bool
	received []string
	acked    map[string]bool
	calls    int
	// Hook runs at the start of Deliver after the call is counted. Tests use
	// it to simulate a consumer that takes the events and then loses the
	// answer. A non-nil error from the hook fails the call.
	Hook func(batch []prevents.Event) error
}

// NewFake returns a working fake sink.
func NewFake() *Fake { return &Fake{acked: map[string]bool{}} }

// SetDown makes the sink fail or work.
func (f *Fake) SetDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}

// Received returns the ids that Deliver saw, in order.
func (f *Fake) Received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

// Acked returns the set of ids that the sink acknowledged.
func (f *Fake) Acked() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k, v := range f.acked {
		out[k] = v
	}
	return out
}

// Calls returns the number of Deliver calls.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Deliver implements sink.EventSink.
func (f *Fake) Deliver(_ context.Context, batch []prevents.Event) ([]sink.Outcome, error) {
	f.mu.Lock()
	f.calls++
	for _, e := range batch {
		f.received = append(f.received, e.ID)
	}
	down, hook := f.down, f.Hook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(batch); err != nil {
			f.Record(err)
			return nil, err
		}
	}
	if down {
		err := errors.New("consumer is down")
		f.Record(err)
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	outs := make([]sink.Outcome, len(batch))
	for i, e := range batch {
		f.acked[e.ID] = true
		outs[i] = sink.Outcome{ID: e.ID, State: sink.Acked}
	}
	f.Record(nil)
	return outs, nil
}

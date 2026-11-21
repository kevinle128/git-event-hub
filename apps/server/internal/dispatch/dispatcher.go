// Package dispatch delivers the outbox. Each sink has its own dispatcher,
// outbox rows and backoff, so a failing sink delays no other.
package dispatch

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"git-event-hub/internal/config"
	"git-event-hub/internal/metrics"
	"git-event-hub/internal/sink"
	"git-event-hub/internal/store"
	"git-event-hub/pkg/prevents"
)

// Dispatcher delivers the outbox of one sink, oldest event first.
type Dispatcher struct {
	key     string
	store   *store.Store
	sink    sink.EventSink
	cfg     config.Delivery
	poll    time.Duration
	metrics *metrics.Registry
	log     *zap.Logger
	now     func() time.Time
	wake    chan struct{}
}

// NewDispatcher builds a dispatcher for the sink with queue name key.
func NewDispatcher(key string, st *store.Store, s sink.EventSink, cfg config.Delivery, m *metrics.Registry, log *zap.Logger) *Dispatcher {
	return &Dispatcher{
		key: key, store: st, sink: s, cfg: cfg, poll: time.Second, metrics: m,
		log: log.With(zap.String("sink", key)), now: time.Now, wake: make(chan struct{}, 1),
	}
}

// Key returns the queue name.
func (d *Dispatcher) Key() string { return d.key }

// Wake tells the dispatcher that there may be new events.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run delivers until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	for ctx.Err() == nil {
		n, wait := d.Step(ctx)
		if n > 0 {
			continue // more events may be waiting
		}
		if wait <= 0 || wait > d.poll {
			wait = d.poll
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-d.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Step makes one delivery attempt. It returns the number of events that it
// tried to deliver and, when no event was due, how long to wait.
func (d *Dispatcher) Step(ctx context.Context) (int, time.Duration) {
	now := d.now()
	if n, err := d.store.Expire(ctx, d.key, now.Add(-d.cfg.MaxAge)); err != nil {
		d.log.Error("expire failed", zap.Error(err))
	} else if n > 0 {
		d.metrics.Add("hub_expired_total", uint64(n), "sink", d.key)
		d.log.Warn("gave up on events that waited too long", zap.Int("count", n))
	}
	items, wait, err := d.store.Claim(ctx, d.key, d.cfg.BatchSize, now)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("claim failed", zap.Error(err))
		}
		return 0, d.poll
	}
	if len(items) == 0 {
		return 0, wait
	}
	events := make([]prevents.Event, len(items))
	for i, it := range items {
		events[i] = it.Event
	}
	started := time.Now()
	outcomes, derr := d.deliver(ctx, events)
	took := time.Since(started)
	byID := make(map[string]sink.Outcome, len(outcomes))
	for _, o := range outcomes {
		byID[o.ID] = o
	}
	results := make([]store.Settlement, 0, len(items))
	// halted is set once an event of the batch did not finish. A sink that
	// stops there (webhook single mode) leaves the later events untried, so
	// they keep their attempts and wait behind it.
	halted := false
	for _, it := range items {
		o, ok := byID[it.Event.ID]
		switch {
		case derr != nil:
			o = sink.Outcome{State: sink.Retry, Err: derr.Error()}
		case !ok && halted:
			continue
		case !ok:
			o = sink.Outcome{State: sink.Retry, Err: "sink gave no answer for the event"}
		}
		if o.State == sink.Retry {
			halted = true
		}
		attempt := it.Attempts + 1
		r := store.Settlement{Seq: it.Seq, EventID: it.Event.ID, Attempt: attempt, Err: o.Err, Duration: took}
		switch {
		case o.State == sink.Acked:
			r.State, r.Outcome, r.Err = store.StateDone, store.OutcomeAcked, ""
			d.metrics.Inc("hub_delivered_total", "sink", d.key)
		case o.State == sink.Rejected:
			r.State, r.Outcome = store.StateRejected, store.OutcomeRejected
			d.metrics.Inc("hub_rejected_total", "sink", d.key)
			d.log.Warn("sink rejected an event", zap.String("event", it.Event.ID), zap.String("reason", o.Err))
		case attempt >= d.cfg.MaxAttempts:
			r.State, r.Outcome = store.StateFailed, store.OutcomeFailed
			d.metrics.Inc("hub_failed_total", "sink", d.key)
			d.log.Error("gave up on an event after the last attempt", zap.String("event", it.Event.ID), zap.Int("attempts", attempt), zap.String("error", o.Err))
		default:
			r.State, r.Outcome, r.NextAt = store.StatePending, store.OutcomeRetry, now.Add(d.backoff(it.Attempts))
			d.metrics.Inc("hub_retried_total", "sink", d.key)
		}
		results = append(results, r)
	}
	// The settle must not stop with the run context: the sink has already
	// seen the events.
	if err := d.store.Settle(context.WithoutCancel(ctx), d.key, results); err != nil {
		d.log.Error("settle failed; the events will be sent again", zap.Error(err))
	}
	if derr != nil {
		d.log.Warn("delivery failed", zap.Error(derr), zap.Int("events", len(items)), zap.Int("attempt", items[0].Attempts+1))
	}
	return len(items), 0
}

func (d *Dispatcher) deliver(ctx context.Context, events []prevents.Event) (outs []sink.Outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			outs, err = nil, fmt.Errorf("sink panicked: %v", r)
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	return d.sink.Deliver(cctx, events)
}

// backoff doubles from the initial wait up to the maximum.
func (d *Dispatcher) backoff(attempts int) time.Duration {
	w := d.cfg.Backoff.Initial
	for i := 0; i < attempts && w < d.cfg.Backoff.Max; i++ {
		w *= 2
	}
	return min(w, d.cfg.Backoff.Max)
}

// Status is the state of a sink for the health and metrics endpoints.
type Status struct {
	Key       string
	Healthy   bool
	Error     string
	Pending   int
	OldestSec float64
	LastError string
	// StatsErr is set when the queue could not be read; Pending, OldestSec
	// and LastError are then unknown.
	StatsErr  error
}

// Status reads the sink health and the queue.
func (d *Dispatcher) Status(ctx context.Context) Status {
	st := Status{Key: d.key, Healthy: true}
	if err := d.sink.Health(ctx); err != nil {
		st.Healthy, st.Error = false, err.Error()
	}
	q, err := d.store.Stats(ctx, d.key)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("queue stats failed", zap.Error(err))
		}
		st.StatsErr = err
		return st
	}
	st.Pending, st.OldestSec, st.LastError = q.Pending, q.OldestAge.Seconds(), q.LastError
	return st
}

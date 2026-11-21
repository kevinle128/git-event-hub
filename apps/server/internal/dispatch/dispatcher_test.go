package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"git-event-hub/internal/config"
	"git-event-hub/internal/metrics"
	"git-event-hub/internal/route"
	"git-event-hub/internal/sink"
	"git-event-hub/internal/sink/sinktest"
	"git-event-hub/internal/store"
	"git-event-hub/internal/store/storetest"
	"git-event-hub/pkg/prevents"
)

var ctx = context.Background()

func policy() config.Delivery {
	return config.Delivery{BatchSize: 10, Timeout: time.Second, Backoff: config.Backoff{Initial: time.Second, Max: 4 * time.Second}, MaxAttempts: 3, MaxAge: time.Hour}
}

func queue(t *testing.T, s *store.Store, ids []string, targets ...route.Target) {
	t.Helper()
	for _, id := range ids {
		e := sinktest.Event(id)
		_, err := s.Accept(ctx, store.Acceptance{Source: "github", DeliveryID: id, Remember: []prevents.Event{e}, Queue: []store.Queued{{Event: e, Targets: targets}}})
		require.NoError(t, err)
	}
}

// clock is a manual clock for the dispatcher.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newDispatcher(key string, s *store.Store, k sink.EventSink, c *clock, m *metrics.Registry) *Dispatcher {
	d := NewDispatcher(key, s, k, policy(), m, zap.NewNop())
	d.now = c.now
	return d
}

func TestFanOutOneSinkFailsTheOtherKeepsGoing(t *testing.T) {
	s := storetest.New(t).Store()
	a, b := route.Target{Subscriber: "kevin", Sink: "omb"}, route.Target{Subscriber: "lan", Sink: "hermes"}
	queue(t, s, []string{"e1", "e2"}, a, b)
	good, bad := sinktest.NewFake(), sinktest.NewFake()
	bad.SetDown(true)
	c := &clock{t: time.Now()}
	m := metrics.New()
	dGood := newDispatcher(a.Key(), s, good, c, m)
	dBad := newDispatcher(b.Key(), s, bad, c, m)

	n, _ := dGood.Step(ctx)
	assert.Equal(t, 2, n)
	n, _ = dBad.Step(ctx)
	assert.Equal(t, 2, n)
	assert.Equal(t, map[string]bool{"e1": true, "e2": true}, good.Acked())
	assert.Empty(t, bad.Acked())
	assert.Error(t, bad.Health(ctx))

	// The failing sink waits for its backoff, then catches up in order.
	_, wait := dBad.Step(ctx)
	assert.InDelta(t, time.Second, wait, float64(time.Millisecond))
	bad.SetDown(false)
	c.advance(time.Second)
	n, _ = dBad.Step(ctx)
	assert.Equal(t, 2, n)
	assert.Equal(t, []string{"e1", "e2", "e1", "e2"}, bad.Received())
	assert.Equal(t, uint64(2), m.Value("hub_delivered_total", "sink", b.Key()))
	assert.NoError(t, bad.Health(ctx))
}

func TestMaxAttemptsParksTheEventAndFreesTheSink(t *testing.T) {
	s := storetest.New(t).Store()
	a := route.Target{Subscriber: "kevin", Sink: "omb"}
	queue(t, s, []string{"poison"}, a)
	f := sinktest.NewFake()
	f.SetDown(true)
	c := &clock{t: time.Now()}
	d := newDispatcher(a.Key(), s, f, c, metrics.New())
	for i := 0; i < 3; i++ {
		n, _ := d.Step(ctx)
		require.Equal(t, 1, n, "attempt %d", i+1)
		c.advance(time.Minute)
	}
	queue(t, s, []string{"next"}, a)
	f.SetDown(false)
	n, _ := d.Step(ctx)
	require.Equal(t, 1, n)
	assert.Equal(t, map[string]bool{"next": true}, f.Acked(), "the parked event must not block the sink")

	log, err := s.ListDeliveries(ctx, store.DeliveryQuery{EventID: "poison"})
	require.NoError(t, err)
	require.Len(t, log, 3)
	assert.Equal(t, store.OutcomeFailed, log[0].Outcome)
	assert.Equal(t, 3, log[0].Attempt)
	events, err := s.ListEvents(ctx, store.EventQuery{})
	require.NoError(t, err)
	states := map[string]string{}
	for _, e := range events {
		states[e.Event.ID] = e.Targets[0].State
	}
	assert.Equal(t, map[string]string{"poison": store.StateFailed, "next": store.StateDone}, states)
}

func TestBackoffDoublesUpToMax(t *testing.T) {
	d := &Dispatcher{cfg: policy()}
	var got []time.Duration
	for i := 0; i < 5; i++ {
		got = append(got, d.backoff(i))
	}
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}, got)
}

type panicky struct{ sink.Tracker }

func (*panicky) Deliver(context.Context, []prevents.Event) ([]sink.Outcome, error) {
	panic("bad adapter")
}

func TestSinkPanicIsARetry(t *testing.T) {
	s := storetest.New(t).Store()
	a := route.Target{Subscriber: "kevin", Sink: "omb"}
	queue(t, s, []string{"e1"}, a)
	d := newDispatcher(a.Key(), s, &panicky{}, &clock{t: time.Now()}, metrics.New())
	n, _ := d.Step(ctx)
	assert.Equal(t, 1, n)
	log, err := s.ListDeliveries(ctx, store.DeliveryQuery{})
	require.NoError(t, err)
	require.Len(t, log, 1)
	assert.Equal(t, store.OutcomeRetry, log[0].Outcome)
	assert.Contains(t, log[0].Error, "panicked")
}

func TestRestartMidDeliveryDeliversAgain(t *testing.T) {
	db := storetest.New(t)
	s := db.Store()
	a := route.Target{Subscriber: "kevin", Sink: "omb"}
	queue(t, s, []string{"e1"}, a)
	// The consumer takes the event, but the answer is lost (the process
	// dies before it records the result).
	f := sinktest.NewFake()
	f.Hook = func([]prevents.Event) error { return errors.New("connection reset") }
	c := &clock{t: time.Now()}
	newDispatcher(a.Key(), s, f, c, metrics.New()).Step(ctx)

	s = db.Reopen()
	f.Hook = nil
	c.advance(time.Minute)
	n, _ := newDispatcher(a.Key(), s, f, c, metrics.New()).Step(ctx)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"e1", "e1"}, f.Received(), "at-least-once: the consumer dedupes by event id")
	assert.True(t, f.Acked()["e1"])
}

func TestRunStopsWithContext(t *testing.T) {
	s := storetest.New(t).Store()
	a := route.Target{Subscriber: "kevin", Sink: "omb"}
	f := sinktest.NewFake()
	d := NewDispatcher(a.Key(), s, f, policy(), metrics.New(), zap.NewNop())
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { d.Run(runCtx); close(done) }()
	queue(t, s, []string{"e1"}, a)
	d.Wake()
	require.Eventually(t, func() bool { return f.Acked()["e1"] }, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// headFails answers only for the first event, as a single-mode webhook does
// when its first request fails.
type headFails struct {
	sink.Tracker
	received []string
}

func (h *headFails) Deliver(_ context.Context, b []prevents.Event) ([]sink.Outcome, error) {
	for _, e := range b {
		h.received = append(h.received, e.ID)
	}
	return []sink.Outcome{{ID: b[0].ID, State: sink.Retry, Err: "down"}}, nil
}

func TestUntriedEventsKeepTheirAttempts(t *testing.T) {
	s := storetest.New(t).Store()
	a := route.Target{Subscriber: "kevin", Sink: "omb"}
	queue(t, s, []string{"poison", "e2", "e3"}, a)
	c := &clock{t: time.Now()}
	d := newDispatcher(a.Key(), s, &headFails{}, c, metrics.New())
	for i := 0; i < 3; i++ {
		n, _ := d.Step(ctx)
		require.Equal(t, 3, n, "attempt %d", i+1)
		c.advance(time.Minute)
	}
	events, err := s.ListEvents(ctx, store.EventQuery{})
	require.NoError(t, err)
	states := map[string]store.TargetState{}
	for _, e := range events {
		states[e.Event.ID] = e.Targets[0]
	}
	assert.Equal(t, store.StateFailed, states["poison"].State)
	for _, id := range []string{"e2", "e3"} {
		assert.Equal(t, store.StatePending, states[id].State, id)
		assert.Equal(t, 0, states[id].Attempts, id)
		log, err := s.ListDeliveries(ctx, store.DeliveryQuery{EventID: id})
		require.NoError(t, err)
		assert.Empty(t, log, id)
	}
}

// slow fails after the clock has moved past the first backoff.
type slow struct {
	sink.Tracker
	c *clock
}

func (s *slow) Deliver(context.Context, []prevents.Event) ([]sink.Outcome, error) {
	s.c.advance(5 * time.Second)
	return nil, errors.New("timeout")
}

func TestBackoffCountsFromTheEndOfTheAttempt(t *testing.T) {
	s := storetest.New(t).Store()
	a := route.Target{Subscriber: "kevin", Sink: "omb"}
	queue(t, s, []string{"e1"}, a)
	c := &clock{t: time.Now()}
	d := newDispatcher(a.Key(), s, &slow{c: c}, c, metrics.New())
	n, _ := d.Step(ctx)
	require.Equal(t, 1, n)
	n, wait := d.Step(ctx)
	assert.Equal(t, 0, n, "the retry must wait for its backoff")
	assert.InDelta(t, time.Second, wait, float64(time.Millisecond))
}

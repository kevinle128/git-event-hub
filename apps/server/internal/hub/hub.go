// Package hub is the core of git-event-hub: it takes a webhook, checks and
// normalizes it, routes each event to the sinks of its subscribers, stores
// everything in one transaction, and keeps one dispatcher running per sink.
// It knows no HTTP framework; the HTTP layer calls Receive.
package hub

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"git-event-hub/internal/config"
	"git-event-hub/internal/dispatch"
	"git-event-hub/internal/metrics"
	"git-event-hub/internal/normalize"
	"git-event-hub/internal/source"
	"git-event-hub/internal/store"
)

// Hub accepts webhooks and runs the sink dispatchers.
type Hub struct {
	store   *store.Store
	cfg     *config.Store
	metrics *metrics.Registry
	log     *zap.Logger
	getenv  func(string) string

	rt atomic.Pointer[runtime]

	mu          sync.Mutex
	dispatchers map[string]*running
	baseCtx     context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	// pollInterval is how often the config file is checked; tests shorten it.
	pollInterval time.Duration
}

type running struct {
	d      *dispatch.Dispatcher
	cfg    config.Sink
	cancel context.CancelFunc
	done   chan struct{}
}

// New builds a hub from the current configuration. Call Start to run the
// dispatchers.
func New(st *store.Store, cfg *config.Store, m *metrics.Registry, log *zap.Logger, getenv func(string) string) (*Hub, error) {
	rt, err := buildRuntime(cfg.Current(), getenv)
	if err != nil {
		return nil, err
	}
	h := &Hub{
		store: st, cfg: cfg, metrics: m, log: log, getenv: getenv,
		dispatchers: map[string]*running{}, pollInterval: 2 * time.Second,
	}
	h.rt.Store(rt)
	return h, nil
}

// SetPollInterval changes how often the config file is checked. Call it
// before Start.
func (h *Hub) SetPollInterval(d time.Duration) { h.pollInterval = d }

// Start runs the dispatchers, the config watcher and the pruning loop. They
// stop with Stop.
func (h *Hub) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.baseCtx, h.cancel = ctx, cancel
	h.mu.Unlock()
	if err := h.apply(h.cfg.Current()); err != nil {
		cancel()
		return err
	}
	h.cfg.OnReloadError = func(error) { h.metrics.Inc("hub_config_reload_failed_total") }
	h.wg.Add(2)
	go func() {
		defer h.wg.Done()
		h.cfg.Watch(ctx, h.pollInterval, func(c *config.Config) {
			if err := h.apply(c); err != nil {
				h.log.Error("applying the new configuration failed", zap.Error(err))
				h.metrics.Inc("hub_config_reload_failed_total")
				return
			}
			h.metrics.Inc("hub_config_reloaded_total")
		})
	}()
	go func() { defer h.wg.Done(); h.pruneLoop(ctx) }()
	return nil
}

// Stop ends the dispatchers and loops and waits for them.
func (h *Hub) Stop() {
	h.mu.Lock()
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	h.wg.Wait()
	h.stopAll()
}

// Apply swaps in a configuration now, as a reload does. Tests use it.
func (h *Hub) Apply(c *config.Config) error { return h.apply(c) }

// apply swaps in the runtime of c and reconciles the dispatchers. A sink
// whose entry did not change keeps running.
func (h *Hub) apply(c *config.Config) error {
	rt, err := buildRuntime(c, h.getenv)
	if err != nil {
		return err
	}
	want := map[string]sinkSpec{}
	for _, sp := range enabledSinks(c) {
		want[sp.key] = sp
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Build every new adapter first, so a bad entry changes nothing.
	built := map[string]*dispatch.Dispatcher{}
	for key, sp := range want {
		if r, ok := h.dispatchers[key]; ok && reflect.DeepEqual(r.cfg, sp.cfg) {
			continue
		}
		adapter, err := newSink(sp, h.getenv)
		if err != nil {
			return err
		}
		built[key] = dispatch.NewDispatcher(key, h.store, adapter, sp.cfg.Delivery, h.metrics, h.log)
	}
	h.rt.Store(rt)
	for key, r := range h.dispatchers {
		if _, keep := want[key]; keep && built[key] == nil {
			continue
		}
		r.cancel()
		<-r.done
		delete(h.dispatchers, key)
		h.log.Info("sink stopped", zap.String("sink", key))
	}
	for key, d := range built {
		ctx, cancel := context.WithCancel(h.baseCtx)
		r := &running{d: d, cfg: want[key].cfg, cancel: cancel, done: make(chan struct{})}
		h.dispatchers[key] = r
		go func() { defer close(r.done); d.Run(ctx) }()
		h.log.Info("sink started", zap.String("sink", key), zap.String("type", want[key].cfg.Type))
	}
	return nil
}

func (h *Hub) stopAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, r := range h.dispatchers {
		r.cancel()
		<-r.done
		delete(h.dispatchers, key)
	}
}

func (h *Hub) wake(keys []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range keys {
		if r, ok := h.dispatchers[k]; ok {
			r.d.Wake()
		}
	}
}

func (h *Hub) pruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		rt := h.rt.Load()
		if err := h.store.Prune(ctx, rt.cfg.Retention, rt.router.Keys()); err != nil && ctx.Err() == nil {
			h.log.Error("prune failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Result is the answer to a webhook.
type Result struct {
	// Status is the HTTP status for the forge.
	Status    int
	Accepted  int    `json:"accepted"`
	Dropped   int    `json:"dropped"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Ignored   string `json:"ignored,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MaxBodyBytes returns the body limit in force.
func (h *Hub) MaxBodyBytes() int64 { return h.rt.Load().cfg.Server.MaxBodyBytes }

// CheckSource answers for a source that is not active, before the body is
// read. ok is true when the source with the given name is active.
func (h *Hub) CheckSource(name string) (res Result, ok bool) {
	return h.checkSource(h.rt.Load(), name)
}

func (h *Hub) checkSource(rt *runtime, name string) (Result, bool) {
	if _, ok := rt.sources[name]; ok {
		return Result{}, true
	}
	if rt.disabled[name] {
		// 202, so that the forge does not mark the webhook as failing.
		h.metrics.Inc("hub_dropped_total", "source", name, "reason", "source_disabled")
		return Result{Status: http.StatusAccepted, Ignored: "source is disabled"}, false
	}
	return Result{Status: http.StatusNotFound, Error: "unknown source"}, false
}

// Receive handles one webhook delivery for the source with the given name.
// The body must be the raw bytes; the signature covers them.
func (h *Hub) Receive(ctx context.Context, name string, header http.Header, body []byte) Result {
	rt := h.rt.Load()
	if res, ok := h.checkSource(rt, name); !ok {
		return res
	}
	src := rt.sources[name]
	req := &source.Request{Method: http.MethodPost, Header: header, Body: body}
	if err := src.Verify(req); err != nil {
		reason := "signature"
		if errors.Is(err, source.ErrNoSignature) {
			reason = "no_signature"
		}
		h.metrics.Inc("hub_refused_total", "source", name, "reason", reason)
		h.log.Warn("webhook refused", zap.String("source", name), zap.String("reason", reason))
		return Result{Status: http.StatusUnauthorized, Error: "unauthorized"}
	}
	h.metrics.Inc("hub_received_total", "source", name)
	deliveryID, err := src.DeliveryID(req)
	if err != nil {
		h.metrics.Inc("hub_refused_total", "source", name, "reason", "no_delivery_id")
		return Result{Status: http.StatusBadRequest, Error: err.Error()}
	}
	log := h.log.With(zap.String("source", name), zap.String("delivery", deliveryID))
	seen, err := h.store.Seen(ctx, name, deliveryID)
	if err != nil {
		log.Error("store failed", zap.Error(err))
		return Result{Status: http.StatusInternalServerError, Error: "storage error"}
	}
	if seen {
		h.metrics.Inc("hub_deduped_total", "source", name)
		return Result{Status: http.StatusOK, Duplicate: true}
	}

	var drops []string
	idx := h.store.Index(ctx)
	events, err := src.Parse(req, normalize.Context{
		DeliveryID: deliveryID, ReceivedAt: time.Now().UTC(), Index: idx,
		Drop: func(reason string) { drops = append(drops, reason) },
	})
	if ierr := idx.Err(); ierr != nil {
		// Without the memory the events may lose their owner; the forge
		// must send the delivery again rather than have it dropped.
		log.Error("store failed", zap.Error(ierr))
		return Result{Status: http.StatusInternalServerError, Error: "storage error"}
	}
	if err != nil {
		h.metrics.Inc("hub_refused_total", "source", name, "reason", "payload")
		log.Warn("payload refused", zap.Error(err))
		return Result{Status: http.StatusBadRequest, Error: "payload does not match the forge format"}
	}
	if len(events) == 0 && len(drops) == 0 {
		drops = append(drops, "no_events")
	}

	acc := store.Acceptance{Source: name, DeliveryID: deliveryID, EventType: eventType(header), Remember: events}
	var wake []string
	for _, e := range events {
		targets, reason := rt.router.Route(e)
		if reason != "" {
			log.Debug("event dropped", zap.String("event", e.ID), zap.String("kind", string(e.Kind)), zap.String("reason", reason))
			drops = append(drops, reason)
			continue
		}
		acc.Queue = append(acc.Queue, store.Queued{Event: e, Targets: targets})
		for _, t := range targets {
			wake = append(wake, t.Key())
		}
	}
	dropped := len(drops)
	acc.Dropped = dropped
	duplicate, err := h.store.Accept(ctx, acc)
	if err != nil {
		log.Error("store failed", zap.Error(err))
		return Result{Status: http.StatusInternalServerError, Error: "storage error"}
	}
	if duplicate {
		h.metrics.Inc("hub_deduped_total", "source", name)
		return Result{Status: http.StatusOK, Duplicate: true}
	}
	// Count only once the delivery is stored: a failed or duplicate one
	// comes again and must not be counted twice.
	for _, reason := range drops {
		h.metrics.Inc("hub_dropped_total", "source", name, "reason", reason)
	}
	for _, q := range acc.Queue {
		for _, t := range q.Targets {
			h.metrics.Inc("hub_queued_total", "subscriber", t.Subscriber, "sink", t.Key())
		}
	}
	h.wake(wake)
	log.Info("webhook accepted", zap.Int("events", len(events)), zap.Int("queued", len(acc.Queue)), zap.Int("dropped", dropped))
	return Result{Status: http.StatusAccepted, Accepted: len(acc.Queue), Dropped: dropped}
}

func eventType(h http.Header) string {
	for _, k := range []string{"X-GitHub-Event", "X-Gitlab-Event"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// Statuses returns the state of every running sink, sorted by queue name.
func (h *Hub) Statuses(ctx context.Context) []dispatch.Status {
	h.mu.Lock()
	ds := make([]*dispatch.Dispatcher, 0, len(h.dispatchers))
	for _, r := range h.dispatchers {
		ds = append(ds, r.d)
	}
	h.mu.Unlock()
	out := make([]dispatch.Status, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Status(ctx))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Ping checks the database.
func (h *Hub) Ping(ctx context.Context) error { return h.store.Ping(ctx) }

// Store returns the store, for the read API.
func (h *Hub) Store() *store.Store { return h.store }

// Metrics returns the counter registry.
func (h *Hub) Metrics() *metrics.Registry { return h.metrics }

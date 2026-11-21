// Package httpapi is the HTTP face of the hub: Echo routes generated from
// api/openapi.yaml, the bearer check of the read endpoints, and the request
// log.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"

	"git-event-hub/internal/api"
	"git-event-hub/internal/config"
	"git-event-hub/internal/hub"
	"git-event-hub/internal/metrics"
	"git-event-hub/internal/store"
	"git-event-hub/pkg/prevents"
)

// Server implements api.ServerInterface.
type Server struct {
	hub     *hub.Hub
	cfg     *config.Store
	getenv  func(string) string
	log     *zap.Logger
	version string
	started time.Time
}

var _ api.ServerInterface = (*Server)(nil)

// New returns the Echo instance with every route.
func New(h *hub.Hub, cfg *config.Store, getenv func(string) string, log *zap.Logger, version string) *echo.Echo {
	s := &Server{hub: h, cfg: cfg, getenv: getenv, log: log, version: version, started: time.Now()}
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.Use(middleware.Recover())
	e.Use(s.requestLog)
	api.RegisterHandlers(&guardedRouter{e: e, guard: s.bearer}, s)
	return e
}

// guardedRouter adds the bearer check to the read endpoints while the
// generated code registers its routes.
type guardedRouter struct {
	e     *echo.Echo
	guard echo.MiddlewareFunc
}

func needsToken(path string) bool { return path == "/metrics" || strings.HasPrefix(path, "/api/") }

func (g *guardedRouter) add(method, path string, h echo.HandlerFunc, m []echo.MiddlewareFunc) *echo.Route {
	if needsToken(path) {
		m = append([]echo.MiddlewareFunc{g.guard}, m...)
	}
	return g.e.Add(method, path, h, m...)
}

func (g *guardedRouter) CONNECT(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodConnect, p, h, m)
}
func (g *guardedRouter) DELETE(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodDelete, p, h, m)
}
func (g *guardedRouter) GET(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodGet, p, h, m)
}
func (g *guardedRouter) HEAD(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodHead, p, h, m)
}
func (g *guardedRouter) OPTIONS(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodOptions, p, h, m)
}
func (g *guardedRouter) PATCH(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodPatch, p, h, m)
}
func (g *guardedRouter) POST(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodPost, p, h, m)
}
func (g *guardedRouter) PUT(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodPut, p, h, m)
}
func (g *guardedRouter) TRACE(p string, h echo.HandlerFunc, m ...echo.MiddlewareFunc) *echo.Route {
	return g.add(http.MethodTrace, p, h, m)
}

// bearer refuses a request without the configured token. When the variable
// is empty, every request is refused: an unset token must not open the read
// endpoints, which carry repository names and comment bodies.
func (s *Server) bearer(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		want := s.getenv(s.cfg.Current().API.TokenEnv)
		got, ok := strings.CutPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
		if want == "" || !ok || !equal(got, want) {
			return c.JSON(http.StatusUnauthorized, api.Error{Error: "missing or wrong bearer token"})
		}
		return next(c)
	}
}

func equal(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func (s *Server) requestLog(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		err := next(c)
		if err != nil {
			c.Error(err)
		}
		s.log.Debug("http request",
			zap.String("method", c.Request().Method), zap.String("path", c.Path()),
			zap.Int("status", c.Response().Status), zap.Duration("took", time.Since(start)))
		return nil
	}
}

// ReceiveHook implements POST /hooks/{source}.
func (s *Server) ReceiveHook(c echo.Context, src string) error {
	// Check the name first: an unknown source must not cost a body read or
	// add a metric series.
	if !s.hub.Known(src) {
		return c.JSON(http.StatusNotFound, api.HookResult{Error: ptr("unknown source")})
	}
	r := c.Request()
	r.Body = http.MaxBytesReader(c.Response(), r.Body, s.hub.MaxBodyBytes())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.hub.Metrics().Inc("hub_refused_total", "source", src, "reason", "body_too_large")
			return c.JSON(http.StatusRequestEntityTooLarge, api.HookResult{Error: ptr("body too large")})
		}
		return c.JSON(http.StatusBadRequest, api.HookResult{Error: ptr("body unreadable")})
	}
	// Storage must finish even when the forge hangs up: the answer is then
	// lost, but the forge's retry is deduplicated.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	res := s.hub.Receive(ctx, src, r.Header, body)
	out := api.HookResult{Accepted: &res.Accepted, Dropped: &res.Dropped}
	if res.Duplicate {
		out.Duplicate = &res.Duplicate
	}
	if res.Ignored != "" {
		out.Ignored = &res.Ignored
	}
	if res.Error != "" {
		out = api.HookResult{Error: &res.Error}
	}
	return c.JSON(res.Status, out)
}

// GetHealth implements GET /healthz. A failing sink does not make the hub
// unhealthy, because a restart would not help it; sink detail is in the
// metrics.
func (s *Server) GetHealth(c echo.Context) error {
	h := api.Health{
		Status: api.Ok, Version: s.version, Contract: prevents.Version,
		UptimeSeconds: int(time.Since(s.started).Seconds()),
	}
	if err := s.hub.Ping(c.Request().Context()); err != nil {
		h.Status = api.DatabaseUnavailable
		return c.JSON(http.StatusServiceUnavailable, h)
	}
	for _, st := range s.hub.Statuses(c.Request().Context()) {
		h.Sinks.Total++
		if st.Healthy {
			h.Sinks.Healthy++
		}
		h.Sinks.Pending += st.Pending
	}
	return c.JSON(http.StatusOK, h)
}

// GetMetrics implements GET /metrics.
func (s *Server) GetMetrics(c echo.Context) error {
	var gauges []metrics.Gauge
	for _, st := range s.hub.Statuses(c.Request().Context()) {
		healthy := 0.0
		if st.Healthy {
			healthy = 1
		}
		gauges = append(gauges,
			metrics.Gauge{Name: "hub_queue_pending", Labels: []string{"sink", st.Key}, Value: float64(st.Pending)},
			metrics.Gauge{Name: "hub_queue_oldest_seconds", Labels: []string{"sink", st.Key}, Value: st.OldestSec},
			metrics.Gauge{Name: "hub_sink_healthy", Labels: []string{"sink", st.Key}, Value: healthy},
		)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/plain; version=0.0.4")
	c.Response().WriteHeader(http.StatusOK)
	return s.hub.Metrics().Write(c.Response(), gauges)
}

// ListEvents implements GET /api/events.
func (s *Server) ListEvents(c echo.Context, p api.ListEventsParams) error {
	q := store.EventQuery{
		Subscriber: val(p.Subscriber), Repo: val(p.Repo), Kind: val(p.Kind),
		Number: val(p.Number), Before: val(p.Before), Limit: val(p.Limit),
	}
	if p.Provider != nil {
		if !p.Provider.Valid() {
			return c.JSON(http.StatusBadRequest, api.Error{Error: "provider must be github or gitlab"})
		}
		q.Provider = string(*p.Provider)
	}
	if q.Kind != "" && !prevents.ValidKind(prevents.Kind(q.Kind)) {
		return c.JSON(http.StatusBadRequest, api.Error{Error: "kind is not an event kind"})
	}
	rows, err := s.hub.Store().ListEvents(c.Request().Context(), q)
	if err != nil {
		s.log.Error("list events failed", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, api.Error{Error: "storage error"})
	}
	page := api.EventPage{Items: make([]api.StoredEvent, 0, len(rows))}
	for _, r := range rows {
		item := api.StoredEvent{Seq: r.Seq, ReceivedAt: r.ReceivedAt, Event: r.Event, Targets: make([]api.TargetState, 0, len(r.Targets))}
		for _, t := range r.Targets {
			ts := api.TargetState{Subscriber: t.Subscriber, Sink: t.Sink, State: api.TargetStateState(t.State), Attempts: t.Attempts, UpdatedAt: t.UpdatedAt}
			if t.LastError != "" {
				ts.LastError = ptr(t.LastError)
			}
			item.Targets = append(item.Targets, ts)
		}
		page.Items = append(page.Items, item)
	}
	if n := len(rows); n > 0 && n == store.PageSize(q.Limit) {
		page.NextBefore = ptr(rows[n-1].Seq)
	}
	return c.JSON(http.StatusOK, page)
}

// ListDeliveries implements GET /api/deliveries.
func (s *Server) ListDeliveries(c echo.Context, p api.ListDeliveriesParams) error {
	q := store.DeliveryQuery{
		Subscriber: val(p.Subscriber), Sink: val(p.Sink), EventID: val(p.EventId),
		Before: val(p.Before), Limit: val(p.Limit),
	}
	if p.Outcome != nil {
		if !p.Outcome.Valid() {
			return c.JSON(http.StatusBadRequest, api.Error{Error: "outcome must be acked, retry, rejected, failed or expired"})
		}
		q.Outcome = string(*p.Outcome)
	}
	rows, err := s.hub.Store().ListDeliveries(c.Request().Context(), q)
	if err != nil {
		s.log.Error("list deliveries failed", zap.Error(err))
		return c.JSON(http.StatusInternalServerError, api.Error{Error: "storage error"})
	}
	page := api.DeliveryPage{Items: make([]api.Delivery, 0, len(rows))}
	for _, r := range rows {
		d := api.Delivery{
			Id: r.ID, Subscriber: r.Subscriber, Sink: r.Sink, EventSeq: r.EventSeq, EventId: r.EventID,
			Attempt: r.Attempt, Outcome: api.DeliveryOutcome(r.Outcome), DurationMs: r.DurationMs, At: r.At,
		}
		if r.Error != "" {
			d.Error = ptr(r.Error)
		}
		page.Items = append(page.Items, d)
	}
	if n := len(rows); n > 0 && n == store.PageSize(q.Limit) {
		page.NextBefore = ptr(rows[n-1].ID)
	}
	return c.JSON(http.StatusOK, page)
}

func ptr[T any](v T) *T { return &v }

func val[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

package store

import (
	"context"
	"encoding/json"
	"time"

	"git-event-hub/pkg/prevents"
)

// MaxPage is the largest page that a list query returns.
const MaxPage = 200

// EventQuery selects stored events, newest first. Zero fields match all.
type EventQuery struct {
	Subscriber string
	Provider   string
	Repo       string
	Number     int
	Kind       string
	// Before returns only events with a smaller sequence number (paging).
	Before int64
	Limit  int
}

// TargetState is the outbox state of one event for one sink.
type TargetState struct {
	Subscriber string    `json:"subscriber"`
	Sink       string    `json:"sink"`
	State      string    `json:"state"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"lastError,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// StoredEvent is an event with its delivery state.
type StoredEvent struct {
	Seq        int64          `json:"seq"`
	ReceivedAt time.Time      `json:"receivedAt"`
	Event      prevents.Event `json:"event"`
	Targets    []TargetState  `json:"targets"`
}

// PageSize returns the page size that a list query uses for limit n.
func PageSize(n int) int {
	if n <= 0 || n > MaxPage {
		return 50
	}
	return n
}

// ListEvents returns events and their per-sink state.
func (s *Store) ListEvents(ctx context.Context, q EventQuery) ([]StoredEvent, error) {
	db := s.db.WithContext(ctx).Model(&eventRow{})
	if q.Subscriber != "" {
		db = db.Where("seq IN (?)", s.db.Model(&outboxRow{}).Select("seq").Where("subscriber = ?", q.Subscriber))
	}
	if q.Provider != "" {
		db = db.Where("provider = ?", q.Provider)
	}
	if q.Repo != "" {
		db = db.Where("repo = ?", q.Repo)
	}
	if q.Number > 0 {
		db = db.Where("number = ?", q.Number)
	}
	if q.Kind != "" {
		db = db.Where("kind = ?", q.Kind)
	}
	if q.Before > 0 {
		db = db.Where("seq < ?", q.Before)
	}
	var rows []eventRow
	if err := db.Order("seq DESC").Limit(PageSize(q.Limit)).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []StoredEvent{}, nil
	}
	seqs := make([]int64, len(rows))
	for i, r := range rows {
		seqs[i] = r.Seq
	}
	var targets []outboxRow
	tq := s.db.WithContext(ctx).Where("seq IN ?", seqs)
	if q.Subscriber != "" {
		// A subscriber sees only its own sinks, never who else got the event.
		tq = tq.Where("subscriber = ?", q.Subscriber)
	}
	if err := tq.Order("seq, sink_key").Find(&targets).Error; err != nil {
		return nil, err
	}
	bySeq := map[int64][]TargetState{}
	for _, t := range targets {
		bySeq[t.Seq] = append(bySeq[t.Seq], TargetState{
			Subscriber: t.Subscriber, Sink: t.Sink, State: t.State, Attempts: t.Attempts,
			LastError: t.LastError, UpdatedAt: time.UnixMilli(t.UpdatedAt).UTC(),
		})
	}
	out := make([]StoredEvent, 0, len(rows))
	for _, r := range rows {
		var e prevents.Event
		if err := json.Unmarshal([]byte(r.Payload), &e); err != nil {
			continue
		}
		ts := bySeq[r.Seq]
		if ts == nil {
			ts = []TargetState{}
		}
		out = append(out, StoredEvent{Seq: r.Seq, ReceivedAt: time.UnixMilli(r.CreatedAt).UTC(), Event: e, Targets: ts})
	}
	return out, nil
}

// DeliveryQuery selects delivery log entries, newest first.
type DeliveryQuery struct {
	Subscriber string
	Sink       string
	EventID    string
	Outcome    string
	Before     int64
	Limit      int
}

// Delivery is one delivery attempt.
type Delivery struct {
	ID         int64     `json:"id"`
	Subscriber string    `json:"subscriber"`
	Sink       string    `json:"sink"`
	EventSeq   int64     `json:"eventSeq"`
	EventID    string    `json:"eventId"`
	Attempt    int       `json:"attempt"`
	Outcome    string    `json:"outcome"`
	Error      string    `json:"error,omitempty"`
	DurationMs int       `json:"durationMs"`
	At         time.Time `json:"at"`
}

// ListDeliveries returns delivery log entries.
func (s *Store) ListDeliveries(ctx context.Context, q DeliveryQuery) ([]Delivery, error) {
	db := s.db.WithContext(ctx).Model(&logRow{})
	if q.Subscriber != "" {
		db = db.Where("subscriber = ?", q.Subscriber)
	}
	if q.Sink != "" {
		db = db.Where("sink = ?", q.Sink)
	}
	if q.EventID != "" {
		db = db.Where("event_id = ?", q.EventID)
	}
	if q.Outcome != "" {
		db = db.Where("outcome = ?", q.Outcome)
	}
	if q.Before > 0 {
		db = db.Where("id < ?", q.Before)
	}
	var rows []logRow
	if err := db.Order("id DESC").Limit(PageSize(q.Limit)).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Delivery, len(rows))
	for i, r := range rows {
		out[i] = Delivery{
			ID: r.ID, Subscriber: r.Subscriber, Sink: r.Sink, EventSeq: r.Seq, EventID: r.EventID,
			Attempt: r.Attempt, Outcome: r.Outcome, Error: r.Error, DurationMs: r.DurationMs, At: time.UnixMilli(r.At).UTC(),
		}
	}
	return out, nil
}

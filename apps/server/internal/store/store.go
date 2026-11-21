// Package store keeps the hub's state in the database: the webhook receipts
// that drop repeated deliveries, the accepted events, one outbox row per event
// and sink, the delivery log, and the memory of pull requests that the
// normalizers use.
//
// An event is stored before the hub answers the forge, so a crash or a down
// consumer loses nothing. Each sink has its own outbox rows, so one slow or
// down sink never holds back another. The SQL is plain enough to run on
// Postgres (production) and SQLite (tests).
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"git-event-hub/internal/config"
	"git-event-hub/internal/normalize"
	"git-event-hub/internal/route"
	"git-event-hub/pkg/prevents"
)

// Outbox row states.
const (
	StatePending  = "pending"
	StateDone     = "done"
	StateRejected = "rejected"
	StateFailed   = "failed"
	StateExpired  = "expired"
)

// Delivery log outcomes.
const (
	OutcomeAcked    = "acked"
	OutcomeRetry    = "retry"
	OutcomeRejected = "rejected"
	OutcomeFailed   = "failed"
	OutcomeExpired  = "expired"
)

type receiptRow struct {
	Source     string `gorm:"primaryKey"`
	DeliveryID string `gorm:"primaryKey"`
	EventType  string
	ReceivedAt int64
	Events     int
	Queued     int
	Dropped    int
}

func (receiptRow) TableName() string { return "webhook_receipts" }

type eventRow struct {
	Seq       int64 `gorm:"primaryKey;autoIncrement"`
	ID        string
	Provider  string
	Repo      string
	Number    int
	Kind      string
	Author    string
	Payload   string
	CreatedAt int64 `gorm:"autoCreateTime:false"`
}

func (eventRow) TableName() string { return "events" }

type outboxRow struct {
	SinkKey    string `gorm:"primaryKey"`
	Seq        int64  `gorm:"primaryKey;autoIncrement:false"`
	Subscriber string
	Sink       string
	State      string
	Attempts   int
	NextAt     int64
	LastError  string
	UpdatedAt  int64 `gorm:"autoUpdateTime:false"`
}

func (outboxRow) TableName() string { return "outbox" }

type logRow struct {
	ID         int64 `gorm:"primaryKey;autoIncrement"`
	SinkKey    string
	Subscriber string
	Sink       string
	Seq        int64
	EventID    string
	Attempt    int
	Outcome    string
	Error      string
	DurationMs int
	At         int64
}

func (logRow) TableName() string { return "delivery_log" }

type indexRow struct {
	Provider string `gorm:"primaryKey"`
	Repo     string `gorm:"primaryKey"`
	Number   int    `gorm:"primaryKey;autoIncrement:false"`
	Author   string
	Branch   string
	BaseRef  string
	HeadSHA  string `gorm:"column:head_sha"`
	// State is "open", "merged" or "closed", or empty when the hub has seen
	// no lifecycle event of the pull request. An empty state counts as open.
	State     string
	UpdatedAt int64 `gorm:"autoUpdateTime:false"`
}

func (indexRow) TableName() string { return "pr_index" }

// Store is the database state of the hub.
type Store struct {
	db  *gorm.DB
	now func() time.Time
}

// New wraps an open database whose schema is up to date.
func New(db *gorm.DB) *Store { return &Store{db: db, now: time.Now} }

// SetClock replaces the clock. Tests use it.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

// Seen reports whether the delivery has been accepted before.
func (s *Store) Seen(ctx context.Context, source, deliveryID string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&receiptRow{}).
		Where("source = ? AND delivery_id = ?", source, deliveryID).Count(&n).Error
	return n > 0, err
}

// Queued is an event and the sinks that must receive it.
type Queued struct {
	Event   prevents.Event
	Targets []route.Target
}

// Acceptance is everything that one webhook delivery changes. Accept writes
// it in one transaction.
type Acceptance struct {
	Source     string
	DeliveryID string
	EventType  string
	// Remember updates the pull request memory. It lists every parsed event,
	// also those that routing drops, so a later pipeline or comment of the
	// same pull request still finds its author.
	Remember []prevents.Event
	Queue    []Queued
	Dropped  int
}

// Accept stores the delivery. It returns duplicate=true and writes nothing
// when the delivery was accepted before.
func (s *Store) Accept(ctx context.Context, a Acceptance) (duplicate bool, err error) {
	now := s.now().UnixMilli()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rc := receiptRow{
			Source: a.Source, DeliveryID: a.DeliveryID, EventType: a.EventType, ReceivedAt: now,
			Events: len(a.Remember), Queued: len(a.Queue), Dropped: a.Dropped,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rc)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			duplicate = true
			return nil
		}
		for _, e := range a.Remember {
			if err := remember(tx, e, now); err != nil {
				return err
			}
		}
		for _, q := range a.Queue {
			payload, err := json.Marshal(q.Event)
			if err != nil {
				return err
			}
			ev := eventRow{
				ID: q.Event.ID, Provider: string(q.Event.Provider), Repo: q.Event.Repo, Number: q.Event.Number,
				Kind: string(q.Event.Kind), Author: deref(q.Event.Author), Payload: string(payload), CreatedAt: now,
			}
			res := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&ev)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				continue // the same event id is already stored
			}
			for _, t := range q.Targets {
				row := outboxRow{
					SinkKey: t.Key(), Seq: ev.Seq, Subscriber: t.Subscriber, Sink: t.Sink,
					State: StatePending, NextAt: now, UpdatedAt: now,
				}
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if duplicate {
		return true, nil
	}
	return false, err
}

// prState is the pull request state that an event proves, or "" when the
// event says nothing about it.
func prState(k prevents.Kind) string {
	switch k {
	case prevents.KindOpened, prevents.KindPushed, prevents.KindConflict:
		return "open"
	case prevents.KindMerged:
		return "merged"
	case prevents.KindClosed:
		return "closed"
	}
	return ""
}

func remember(tx *gorm.DB, e prevents.Event, now int64) error {
	row := indexRow{
		Provider: string(e.Provider), Repo: e.Repo, Number: e.Number, Author: deref(e.Author),
		Branch: deref(e.Branch), BaseRef: deref(e.BaseRef), HeadSHA: deref(e.HeadSHA),
		State: prState(e.Kind), UpdatedAt: now,
	}
	keep := func(col string) clause.Expr {
		return clause.Expr{SQL: fmt.Sprintf("COALESCE(NULLIF(excluded.%[1]s, ''), pr_index.%[1]s)", col)}
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "provider"}, {Name: "repo"}, {Name: "number"}},
		DoUpdates: clause.Set{
			{Column: clause.Column{Name: "author"}, Value: keep("author")},
			{Column: clause.Column{Name: "branch"}, Value: keep("branch")},
			{Column: clause.Column{Name: "base_ref"}, Value: keep("base_ref")},
			{Column: clause.Column{Name: "head_sha"}, Value: keep("head_sha")},
			{Column: clause.Column{Name: "state"}, Value: keep("state")},
			{Column: clause.Column{Name: "updated_at"}, Value: clause.Expr{SQL: "excluded.updated_at"}},
		},
	}).Create(&row).Error
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Item is one event that a sink has to deliver.
type Item struct {
	Seq      int64
	Event    prevents.Event
	Attempts int
}

// Claim returns the oldest pending events of a sink, at most limit, in the
// order they were stored. When the oldest one waits for its backoff, Claim
// returns no items and the time left: later events wait behind it, so the
// sink sees them in order. Rows whose stored event cannot be read are marked
// rejected.
func (s *Store) Claim(ctx context.Context, key string, limit int, now time.Time) ([]Item, time.Duration, error) {
	type joined struct {
		Seq      int64
		Attempts int
		NextAt   int64
		Payload  string
	}
	var rows []joined
	err := s.db.WithContext(ctx).Table("outbox AS q").
		Select("q.seq, q.attempts, q.next_at, e.payload").
		Joins("JOIN events e ON e.seq = q.seq").
		Where("q.sink_key = ? AND q.state = ?", key, StatePending).
		Order("q.seq").Limit(limit).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, 0, err
	}
	if wait := time.UnixMilli(rows[0].NextAt).Sub(now); wait > 0 {
		return nil, wait, nil
	}
	var (
		items []Item
		bad   []Settlement
	)
	for _, r := range rows {
		var e prevents.Event
		if err := json.Unmarshal([]byte(r.Payload), &e); err != nil {
			bad = append(bad, Settlement{Seq: r.Seq, State: StateRejected, Outcome: OutcomeRejected, Attempt: r.Attempts + 1, Err: "stored event is unreadable: " + err.Error()})
			continue
		}
		items = append(items, Item{Seq: r.Seq, Event: e, Attempts: r.Attempts})
	}
	if len(bad) > 0 {
		if err := s.Settle(ctx, key, bad); err != nil {
			return nil, 0, err
		}
	}
	return items, 0, nil
}

// Settlement is the result of one delivery attempt for one outbox row.
type Settlement struct {
	Seq     int64
	EventID string
	// State is the new outbox state: StatePending (retry later), StateDone,
	// StateRejected or StateFailed.
	State string
	// Outcome is written to the delivery log.
	Outcome string
	Attempt int
	Err     string
	// NextAt is the earliest time of the next attempt, for StatePending.
	NextAt   time.Time
	Duration time.Duration
}

// Settle records the results of a delivery attempt and logs them, in one
// transaction.
func (s *Store) Settle(ctx context.Context, key string, results []Settlement) error {
	sub, sink := splitKey(key)
	now := s.now().UnixMilli()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, r := range results {
			upd := map[string]any{"state": r.State, "last_error": r.Err, "updated_at": now, "attempts": r.Attempt}
			if r.State == StatePending {
				upd["next_at"] = r.NextAt.UnixMilli()
			}
			if err := tx.Model(&outboxRow{}).Where("sink_key = ? AND seq = ?", key, r.Seq).Updates(upd).Error; err != nil {
				return err
			}
			l := logRow{
				SinkKey: key, Subscriber: sub, Sink: sink, Seq: r.Seq, EventID: r.EventID, Attempt: r.Attempt,
				Outcome: r.Outcome, Error: r.Err, DurationMs: int(r.Duration.Milliseconds()), At: now,
			}
			if err := tx.Create(&l).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func splitKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// Expire gives up on the pending events of a sink that were stored before
// cutoff, and logs each one.
func (s *Store) Expire(ctx context.Context, key string, cutoff time.Time) (int, error) {
	type pending struct {
		Seq      int64
		Attempts int
		ID       string
	}
	var rows []pending
	err := s.db.WithContext(ctx).Table("outbox AS q").Select("q.seq, q.attempts, e.id").
		Joins("JOIN events e ON e.seq = q.seq").
		Where("q.sink_key = ? AND q.state = ? AND e.created_at < ?", key, StatePending, cutoff.UnixMilli()).
		Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	results := make([]Settlement, len(rows))
	for i, r := range rows {
		results[i] = Settlement{Seq: r.Seq, EventID: r.ID, State: StateExpired, Outcome: OutcomeExpired, Attempt: r.Attempts, Err: "waited longer than maxAge"}
	}
	return len(rows), s.Settle(ctx, key, results)
}

// QueueStats describes the pending events of one sink.
type QueueStats struct {
	Pending   int
	OldestAge time.Duration // age of the oldest pending event
	LastError string
}

// Stats returns the queue state of a sink.
func (s *Store) Stats(ctx context.Context, key string) (QueueStats, error) {
	var agg struct {
		N      int
		Oldest *int64
	}
	err := s.db.WithContext(ctx).Table("outbox AS q").Select("COUNT(*) AS n, MIN(e.created_at) AS oldest").
		Joins("JOIN events e ON e.seq = q.seq").
		Where("q.sink_key = ? AND q.state = ?", key, StatePending).Scan(&agg).Error
	if err != nil {
		return QueueStats{}, err
	}
	st := QueueStats{Pending: agg.N}
	if agg.Oldest != nil {
		st.OldestAge = s.now().Sub(time.UnixMilli(*agg.Oldest))
	}
	var last []string
	err = s.db.WithContext(ctx).Model(&outboxRow{}).
		Where("sink_key = ? AND state = ? AND last_error <> ''", key, StatePending).
		Order("updated_at DESC").Limit(1).Pluck("last_error", &last).Error
	if len(last) > 0 {
		st.LastError = last[0]
	}
	return st, err
}

// Prune deletes old data. Outbox rows that are no longer pending go after
// EventsDays, and so do the rows of sinks that are no longer configured, the
// delivery log, and events without outbox rows.
func (s *Store) Prune(ctx context.Context, ret config.Retention, keys []string) error {
	now := s.now()
	day := 24 * time.Hour
	evCut := now.Add(-time.Duration(ret.EventsDays) * day).UnixMilli()
	db := s.db.WithContext(ctx)
	steps := []*gorm.DB{
		db.Where("state <> ? AND updated_at < ?", StatePending, evCut).Delete(&outboxRow{}),
		db.Where("at < ?", evCut).Delete(&logRow{}),
		db.Where("received_at < ?", now.Add(-time.Duration(ret.ReceiptsDays)*day).UnixMilli()).Delete(&receiptRow{}),
		db.Where("updated_at < ?", now.Add(-time.Duration(ret.IndexDays)*day).UnixMilli()).Delete(&indexRow{}),
	}
	if len(keys) > 0 {
		steps = append(steps, db.Where("updated_at < ? AND sink_key NOT IN ?", evCut, keys).Delete(&outboxRow{}))
	}
	steps = append(steps, db.Where("created_at < ? AND seq NOT IN (?)", evCut, db.Model(&outboxRow{}).Select("seq")).Delete(&eventRow{}))
	for _, st := range steps {
		if st.Error != nil {
			return st.Error
		}
	}
	return nil
}

// Get implements normalize.Index.
func (s *Store) Get(p prevents.Provider, repo string, number int) (normalize.Entry, bool) {
	var rows []indexRow
	if err := s.db.Where("provider = ? AND repo = ? AND number = ?", string(p), repo, number).Limit(1).Find(&rows).Error; err != nil || len(rows) == 0 {
		return normalize.Entry{}, false
	}
	return entryOf(rows[0]), true
}

// ByBranch implements normalize.Index. It finds the open pull request that
// has the branch as its source, and finds none when two match.
func (s *Store) ByBranch(p prevents.Provider, repo, branch string) (normalize.Entry, bool) {
	if branch == "" {
		return normalize.Entry{}, false
	}
	return s.lookupOpen(s.db.Where("provider = ? AND repo = ? AND branch = ?", string(p), repo, branch))
}

// BySHA implements normalize.Index. It finds the open pull request that has
// the commit as its head, and finds none when two match.
func (s *Store) BySHA(p prevents.Provider, repo, sha string) (normalize.Entry, bool) {
	if sha == "" {
		return normalize.Entry{}, false
	}
	return s.lookupOpen(s.db.Where("provider = ? AND repo = ? AND head_sha = ?", string(p), repo, sha))
}

// lookupOpen finds the one open pull request that the query matches. A
// branch or commit that more than one open pull request shares does not name
// an owner; routing by a guess would hand one member's events to another.
// A merged or closed pull request never matches. An empty state counts as
// open, because such a pull request has shown no lifecycle event.
func (s *Store) lookupOpen(q *gorm.DB) (normalize.Entry, bool) {
	var rows []indexRow
	if err := q.Where("state NOT IN ?", []string{"merged", "closed"}).Limit(2).Find(&rows).Error; err != nil || len(rows) != 1 {
		return normalize.Entry{}, false
	}
	return entryOf(rows[0]), true
}

func entryOf(r indexRow) normalize.Entry {
	return normalize.Entry{Number: r.Number, Author: r.Author, Branch: r.Branch, BaseRef: r.BaseRef, HeadSHA: r.HeadSHA}
}

var _ normalize.Index = (*Store)(nil)

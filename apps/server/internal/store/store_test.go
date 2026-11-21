package store_test

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/config"
	"git-event-hub/internal/route"
	"git-event-hub/internal/sink/sinktest"
	"git-event-hub/internal/store"
	"git-event-hub/internal/store/storetest"
	"git-event-hub/migrations"
	"git-event-hub/pkg/prevents"
)

var ctx = context.Background()

func kevin(sink string) route.Target { return route.Target{Subscriber: "kevin", Sink: sink} }

func accept(t *testing.T, s *store.Store, delivery string, q ...store.Queued) bool {
	t.Helper()
	var remember []prevents.Event
	for _, x := range q {
		remember = append(remember, x.Event)
	}
	dup, err := s.Accept(ctx, store.Acceptance{Source: "github", DeliveryID: delivery, Remember: remember, Queue: q})
	require.NoError(t, err)
	return dup
}

func TestMigrationDirectoriesMatch(t *testing.T) {
	names := func(f fs.FS, dir string) []string {
		entries, err := fs.ReadDir(f, dir)
		require.NoError(t, err)
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	assert.Equal(t, names(migrations.Postgres, "postgres"), names(migrations.SQLite, "sqlite"))
}

func TestAcceptDedupesByDeliveryID(t *testing.T) {
	s := storetest.New(t).Store()
	e := sinktest.Event("github:d1:0")
	assert.False(t, accept(t, s, "d1", store.Queued{Event: e, Targets: []route.Target{kevin("omb")}}))
	assert.True(t, accept(t, s, "d1", store.Queued{Event: e, Targets: []route.Target{kevin("omb")}}))
	seen, err := s.Seen(ctx, "github", "d1")
	require.NoError(t, err)
	assert.True(t, seen)
	st, err := s.Stats(ctx, "kevin/omb")
	require.NoError(t, err)
	assert.Equal(t, 1, st.Pending, "a repeated delivery must not queue the event twice")
}

func TestAcceptQueuesOneRowPerSink(t *testing.T) {
	s := storetest.New(t).Store()
	e := sinktest.Event("github:d1:0")
	accept(t, s, "d1", store.Queued{Event: e, Targets: []route.Target{kevin("omb"), {Subscriber: "lan", Sink: "hermes"}}})
	for _, key := range []string{"kevin/omb", "lan/hermes"} {
		items, _, err := s.Claim(ctx, key, 10, time.Now())
		require.NoError(t, err)
		require.Len(t, items, 1, key)
		assert.Equal(t, e.ID, items[0].Event.ID)
	}
	items, _, err := s.Claim(ctx, "kevin/other", 10, time.Now())
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestPullRequestMemoryKeepsKnownValues(t *testing.T) {
	s := storetest.New(t).Store()
	author, branch, head := "kevinle128", "fix/x", "aaaa"
	e := sinktest.Event("github:d1:0")
	e.Author, e.Branch, e.HeadSHA = &author, &branch, &head
	accept(t, s, "d1", store.Queued{Event: e})

	// A later event without author or branch must not erase them.
	newHead := "bbbb"
	later := sinktest.Event("github:d2:0")
	later.Author, later.Branch, later.HeadSHA = nil, nil, &newHead
	accept(t, s, "d2", store.Queued{Event: later})

	got, ok := s.Get(prevents.GitHub, e.Repo, e.Number)
	require.True(t, ok)
	assert.Equal(t, "kevinle128", got.Author)
	assert.Equal(t, "fix/x", got.Branch)
	assert.Equal(t, "bbbb", got.HeadSHA)
	byBranch, ok := s.ByBranch(prevents.GitHub, e.Repo, "fix/x")
	require.True(t, ok)
	assert.Equal(t, e.Number, byBranch.Number)
	bySHA, ok := s.BySHA(prevents.GitHub, e.Repo, "bbbb")
	require.True(t, ok)
	assert.Equal(t, e.Number, bySHA.Number)
	_, ok = s.BySHA(prevents.GitHub, e.Repo, "aaaa")
	assert.False(t, ok)
}

func TestClaimKeepsOrderAndWaitsForBackoff(t *testing.T) {
	s := storetest.New(t).Store()
	for _, id := range []string{"a", "b", "c"} {
		accept(t, s, id, store.Queued{Event: sinktest.Event(id), Targets: []route.Target{kevin("omb")}})
	}
	now := time.Now()
	items, _, err := s.Claim(ctx, "kevin/omb", 2, now)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, []string{"a", "b"}, []string{items[0].Event.ID, items[1].Event.ID})

	require.NoError(t, s.Settle(ctx, "kevin/omb", []store.Settlement{
		{Seq: items[0].Seq, EventID: "a", State: store.StatePending, Outcome: store.OutcomeRetry, Attempt: 1, Err: "boom", NextAt: now.Add(time.Minute)},
		{Seq: items[1].Seq, EventID: "b", State: store.StateDone, Outcome: store.OutcomeAcked, Attempt: 1},
	}))
	items, wait, err := s.Claim(ctx, "kevin/omb", 10, now)
	require.NoError(t, err)
	assert.Empty(t, items, "c must wait behind a")
	assert.InDelta(t, time.Minute.Seconds(), wait.Seconds(), 1)

	items, _, err = s.Claim(ctx, "kevin/omb", 10, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "a", items[0].Event.ID)
	assert.Equal(t, 1, items[0].Attempts)

	log, err := s.ListDeliveries(ctx, store.DeliveryQuery{Subscriber: "kevin"})
	require.NoError(t, err)
	require.Len(t, log, 2)
	assert.Equal(t, store.OutcomeAcked, log[0].Outcome)
	assert.Equal(t, store.OutcomeRetry, log[1].Outcome)
	assert.Equal(t, "boom", log[1].Error)
	assert.Equal(t, "omb", log[1].Sink)
}

func TestAcceptedEventSurvivesRestart(t *testing.T) {
	db := storetest.New(t)
	s := db.Store()
	accept(t, s, "d1", store.Queued{Event: sinktest.Event("x"), Targets: []route.Target{kevin("omb")}})

	s = db.Reopen()
	items, _, err := s.Claim(ctx, "kevin/omb", 10, time.Now())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "x", items[0].Event.ID)
	assert.True(t, accept(t, s, "d1"), "the receipt must survive the restart too")
}

func TestExpireGivesUpOnOldEvents(t *testing.T) {
	s := storetest.New(t).Store()
	start := time.Now()
	s.SetClock(func() time.Time { return start })
	accept(t, s, "d1", store.Queued{Event: sinktest.Event("old"), Targets: []route.Target{kevin("omb")}})
	n, err := s.Expire(ctx, "kevin/omb", start.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	items, _, err := s.Claim(ctx, "kevin/omb", 10, start.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, items)
	log, err := s.ListDeliveries(ctx, store.DeliveryQuery{Outcome: store.OutcomeExpired})
	require.NoError(t, err)
	assert.Len(t, log, 1)
}

func TestPruneKeepsPendingAndRemovesOld(t *testing.T) {
	s := storetest.New(t).Store()
	start := time.Now()
	s.SetClock(func() time.Time { return start })
	accept(t, s, "d1", store.Queued{Event: sinktest.Event("done"), Targets: []route.Target{kevin("omb")}})
	accept(t, s, "d2", store.Queued{Event: sinktest.Event("waiting"), Targets: []route.Target{kevin("omb")}})
	items, _, err := s.Claim(ctx, "kevin/omb", 1, start)
	require.NoError(t, err)
	require.NoError(t, s.Settle(ctx, "kevin/omb", []store.Settlement{{Seq: items[0].Seq, EventID: "done", State: store.StateDone, Outcome: store.OutcomeAcked, Attempt: 1}}))

	s.SetClock(func() time.Time { return start.Add(40 * 24 * time.Hour) })
	require.NoError(t, s.Prune(ctx, config.Retention{ReceiptsDays: 14, EventsDays: 30, IndexDays: 60}, []string{"kevin/omb"}))

	events, err := s.ListEvents(ctx, store.EventQuery{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "waiting", events[0].Event.ID)
	seen, err := s.Seen(ctx, "github", "d1")
	require.NoError(t, err)
	assert.False(t, seen, "old receipts are pruned")
	_, ok := s.Get(prevents.GitHub, "kevinle128/demo", 7)
	assert.True(t, ok, "the pull request memory lasts longer")
}

func TestPruneRemovesPendingRowsOfRemovedSinks(t *testing.T) {
	for name, keys := range map[string][]string{"one sink left": {"kevin/omb"}, "no sink left": nil} {
		t.Run(name, func(t *testing.T) {
			s := storetest.New(t).Store()
			start := time.Now()
			s.SetClock(func() time.Time { return start })
			accept(t, s, "d1", store.Queued{Event: sinktest.Event("kept"), Targets: []route.Target{kevin("omb")}})
			accept(t, s, "d2", store.Queued{Event: sinktest.Event("gone"), Targets: []route.Target{{Subscriber: "removed", Sink: "sink"}}})

			s.SetClock(func() time.Time { return start.Add(40 * 24 * time.Hour) })
			require.NoError(t, s.Prune(ctx, config.Retention{ReceiptsDays: 14, EventsDays: 30, IndexDays: 60}, keys))

			events, err := s.ListEvents(ctx, store.EventQuery{})
			require.NoError(t, err)
			ids := make([]string, 0, len(events))
			for _, e := range events {
				ids = append(ids, e.Event.ID)
			}
			if keys == nil {
				assert.Empty(t, ids, "with no sink configured, every old pending row is pruned")
			} else {
				assert.Equal(t, []string{"kept"}, ids)
			}
		})
	}
}

func TestListEventsIsolatesSubscribers(t *testing.T) {
	s := storetest.New(t).Store()
	accept(t, s, "d1", store.Queued{Event: sinktest.Event("both"), Targets: []route.Target{kevin("omb"), {Subscriber: "lan", Sink: "hermes"}}})
	accept(t, s, "d2", store.Queued{Event: sinktest.Event("lan-only"), Targets: []route.Target{{Subscriber: "lan", Sink: "hermes"}}})

	got, err := s.ListEvents(ctx, store.EventQuery{Subscriber: "kevin"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "both", got[0].Event.ID)
	require.Len(t, got[0].Targets, 1, "kevin must not see lan's sink")
	assert.Equal(t, "omb", got[0].Targets[0].Sink)
	assert.Equal(t, store.StatePending, got[0].Targets[0].State)

	all, err := s.ListEvents(ctx, store.EventQuery{Limit: 1})
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "lan-only", all[0].Event.ID, "newest first")
	next, err := s.ListEvents(ctx, store.EventQuery{Before: all[0].Seq})
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.Equal(t, "both", next[0].Event.ID)
	assert.Len(t, next[0].Targets, 2)
}

func TestClaimLogsTheEventIDOfAnUnreadablePayload(t *testing.T) {
	db := storetest.New(t)
	s := db.Store()
	accept(t, s, "d1", store.Queued{Event: sinktest.Event("broken"), Targets: []route.Target{kevin("omb")}})
	require.NoError(t, db.Gorm().Exec(`UPDATE events SET payload = 'not json' WHERE id = ?`, "broken").Error)
	items, _, err := s.Claim(ctx, "kevin/omb", 10, time.Now())
	require.NoError(t, err)
	assert.Empty(t, items)
	log, err := s.ListDeliveries(ctx, store.DeliveryQuery{EventID: "broken"})
	require.NoError(t, err)
	require.Len(t, log, 1)
	assert.Equal(t, store.OutcomeRejected, log[0].Outcome)
}

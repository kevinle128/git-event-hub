package store_test

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/config"
	"git-event-hub/internal/normalize"
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

const glRepo = "vtc/3q-chess/3q-chess-cms"

// glMergeRequest builds a merge request webhook of one actor. The actor is
// the author, so the hub learns the author name from the payload.
func glMergeRequest(iid, authorID int, author, action string) []byte {
	return []byte(fmt.Sprintf(`{"object_kind":"merge_request","user":{"id":%[1]d,"username":%[2]q},
		"project":{"web_url":"https://git.example.test/%[4]s","path_with_namespace":%[4]q},
		"object_attributes":{"iid":%[3]d,"title":"t","url":"https://git.example.test/mr/%[3]d","action":%[5]q,
		"source_branch":"develop","target_branch":"dev","author_id":%[1]d,"last_commit":{"id":"c0ffee%[3]d"}}}`,
		authorID, author, iid, glRepo, action))
}

func glBranchPipeline(actor string) []byte {
	return []byte(fmt.Sprintf(`{"object_kind":"pipeline","user":{"id":9,"username":%q},
		"project":{"web_url":"https://git.example.test/%[2]s","path_with_namespace":%[2]q},
		"object_attributes":{"id":501,"ref":"develop","sha":"deadbeef","status":"failed"},
		"merge_request":null,"builds":[]}`, actor, glRepo))
}

// glDeliver normalizes a GitLab payload with the store as the index and
// stores the events, like the webhook handler does.
func glDeliver(t *testing.T, s *store.Store, delivery string, body []byte) (events []prevents.Event, drops []string) {
	t.Helper()
	events, err := normalize.GitLab(body, normalize.Context{
		DeliveryID: delivery, ReceivedAt: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC), Index: s,
		Drop: func(r string) { drops = append(drops, r) },
	})
	require.NoError(t, err)
	if len(events) > 0 {
		_, err = s.Accept(ctx, store.Acceptance{Source: "gitlab", DeliveryID: delivery, Remember: events})
		require.NoError(t, err)
	}
	return events, drops
}

func ghEvent(id string, number int, kind prevents.Kind, head string) prevents.Event {
	e := sinktest.Event(id)
	e.Number, e.Kind = number, kind
	e.HeadSHA = &head
	return e
}

func storedState(t *testing.T, db *storetest.DB, provider prevents.Provider, repo string, number int) string {
	t.Helper()
	var state string
	require.NoError(t, db.Gorm().Raw("SELECT state FROM pr_index WHERE provider = ? AND repo = ? AND number = ?", string(provider), repo, number).Scan(&state).Error)
	return state
}

func TestBranchPipelineSkipsMergedMergeRequest(t *testing.T) {
	s := storetest.New(t).Store()
	glDeliver(t, s, "d1", glMergeRequest(3, 11, "lan.nguyen", "open"))
	glDeliver(t, s, "d2", glMergeRequest(3, 11, "lan.nguyen", "merge"))

	events, drops := glDeliver(t, s, "d3", glBranchPipeline("kevin.le"))
	assert.Empty(t, events, "a merged merge request must not own a later push to its branch")
	assert.Equal(t, []string{normalize.DropNoPullRequest}, drops)
}

func TestBranchPipelineFindsOpenMergeRequest(t *testing.T) {
	s := storetest.New(t).Store()
	glDeliver(t, s, "d1", glMergeRequest(3, 11, "lan.nguyen", "open"))

	events, drops := glDeliver(t, s, "d2", glBranchPipeline("kevin.le"))
	assert.Empty(t, drops)
	require.Len(t, events, 1)
	assert.Equal(t, 3, events[0].Number)
	require.NotNil(t, events[0].Author)
	assert.Equal(t, "lan.nguyen", *events[0].Author)
}

func TestReopenedPullRequestIsFoundAgain(t *testing.T) {
	s := storetest.New(t).Store()
	glDeliver(t, s, "d1", glMergeRequest(3, 11, "lan.nguyen", "open"))
	glDeliver(t, s, "d2", glMergeRequest(3, 11, "lan.nguyen", "close"))
	_, ok := s.ByBranch(prevents.GitLab, glRepo, "develop")
	assert.False(t, ok, "a closed merge request is not found")

	glDeliver(t, s, "d3", glMergeRequest(3, 11, "lan.nguyen", "reopen"))
	got, ok := s.ByBranch(prevents.GitLab, glRepo, "develop")
	require.True(t, ok)
	assert.Equal(t, 3, got.Number)
}

func TestBranchSharedByTwoOpenMergeRequestsFindsNone(t *testing.T) {
	s := storetest.New(t).Store()
	glDeliver(t, s, "d1", glMergeRequest(3, 11, "lan.nguyen", "open"))
	glDeliver(t, s, "d2", glMergeRequest(4, 12, "minh.tran", "open"))
	_, ok := s.ByBranch(prevents.GitLab, glRepo, "develop")
	assert.False(t, ok, "two open merge requests do not name an owner")

	glDeliver(t, s, "d3", glMergeRequest(4, 12, "minh.tran", "close"))
	got, ok := s.ByBranch(prevents.GitLab, glRepo, "develop")
	require.True(t, ok, "the one open merge request that is left owns the branch")
	assert.Equal(t, 3, got.Number)
}

func TestCommitSharedByTwoOpenPullRequestsFindsNone(t *testing.T) {
	s := storetest.New(t).Store()
	const sha = "aaaa1111"
	accept(t, s, "d1", store.Queued{Event: ghEvent("github:d1:0", 7, prevents.KindOpened, sha)})
	accept(t, s, "d2", store.Queued{Event: ghEvent("github:d2:0", 8, prevents.KindOpened, sha)})
	_, ok := s.BySHA(prevents.GitHub, "kevinle128/demo", sha)
	assert.False(t, ok, "two open pull requests do not name an owner")

	accept(t, s, "d3", store.Queued{Event: ghEvent("github:d3:0", 7, prevents.KindMerged, sha)})
	got, ok := s.BySHA(prevents.GitHub, "kevinle128/demo", sha)
	require.True(t, ok)
	assert.Equal(t, 8, got.Number)
}

func TestMergedAndClosedPullRequestsAreNotFoundBySHA(t *testing.T) {
	s := storetest.New(t).Store()
	accept(t, s, "d1", store.Queued{Event: ghEvent("github:d1:0", 7, prevents.KindOpened, "bbbb2222")})
	accept(t, s, "d2", store.Queued{Event: ghEvent("github:d2:0", 7, prevents.KindMerged, "bbbb2222")})
	_, ok := s.BySHA(prevents.GitHub, "kevinle128/demo", "bbbb2222")
	assert.False(t, ok)

	accept(t, s, "d3", store.Queued{Event: ghEvent("github:d3:0", 9, prevents.KindOpened, "cccc3333")})
	accept(t, s, "d4", store.Queued{Event: ghEvent("github:d4:0", 9, prevents.KindClosed, "cccc3333")})
	_, ok = s.BySHA(prevents.GitHub, "kevinle128/demo", "cccc3333")
	assert.False(t, ok)

	accept(t, s, "d5", store.Queued{Event: ghEvent("github:d5:0", 9, prevents.KindOpened, "cccc3333")})
	got, ok := s.BySHA(prevents.GitHub, "kevinle128/demo", "cccc3333")
	require.True(t, ok, "a reopened pull request is found again")
	assert.Equal(t, 9, got.Number)
}

func TestPullRequestWithoutStateCountsAsOpen(t *testing.T) {
	db := storetest.New(t)
	s := db.Store()
	// A comment event is the first thing the hub sees of this pull request.
	accept(t, s, "d1", store.Queued{Event: ghEvent("github:d1:0", 7, prevents.KindComment, "dddd4444")})
	assert.Equal(t, "", storedState(t, db, prevents.GitHub, "kevinle128/demo", 7))
	_, ok := s.BySHA(prevents.GitHub, "kevinle128/demo", "dddd4444")
	assert.True(t, ok)
}

func TestCommentDoesNotChangeTheState(t *testing.T) {
	db := storetest.New(t)
	s := db.Store()
	accept(t, s, "d1", store.Queued{Event: ghEvent("github:d1:0", 7, prevents.KindOpened, "eeee5555")})
	accept(t, s, "d2", store.Queued{Event: ghEvent("github:d2:0", 7, prevents.KindMerged, "eeee5555")})
	accept(t, s, "d3", store.Queued{Event: ghEvent("github:d3:0", 7, prevents.KindComment, "eeee5555")})
	accept(t, s, "d4", store.Queued{Event: ghEvent("github:d4:0", 7, prevents.KindCIFinished, "eeee5555")})
	assert.Equal(t, "merged", storedState(t, db, prevents.GitHub, "kevinle128/demo", 7))
}

func TestLateCIRunDoesNotMoveTheHead(t *testing.T) {
	s := storetest.New(t).Store()
	accept(t, s, "d1", store.Queued{Event: ghEvent("github:d1:0", 7, prevents.KindOpened, "aaaa")})
	accept(t, s, "d2", store.Queued{Event: ghEvent("github:d2:0", 7, prevents.KindPushed, "bbbb")})
	// The run of the old commit finishes after the push.
	accept(t, s, "d3", store.Queued{Event: ghEvent("github:d3:0", 7, prevents.KindCIFinished, "aaaa")})

	got, ok := s.Get(prevents.GitHub, "kevinle128/demo", 7)
	require.True(t, ok)
	assert.Equal(t, "bbbb", got.HeadSHA)
	_, ok = s.BySHA(prevents.GitHub, "kevinle128/demo", "bbbb")
	assert.True(t, ok)
}

func TestCIRunFillsAnUnknownHead(t *testing.T) {
	s := storetest.New(t).Store()
	e := ghEvent("github:d1:0", 7, prevents.KindComment, "")
	e.HeadSHA = nil
	accept(t, s, "d1", store.Queued{Event: e})
	accept(t, s, "d2", store.Queued{Event: ghEvent("github:d2:0", 7, prevents.KindCIFinished, "cccc")})

	got, ok := s.Get(prevents.GitHub, "kevinle128/demo", 7)
	require.True(t, ok)
	assert.Equal(t, "cccc", got.HeadSHA)
}

func TestPruneWithoutSinksRemovesOldRowsOfGoneSinks(t *testing.T) {
	s := storetest.New(t).Store()
	start := time.Now()
	s.SetClock(func() time.Time { return start })
	accept(t, s, "d1", store.Queued{Event: sinktest.Event("stuck"), Targets: []route.Target{kevin("omb")}})

	s.SetClock(func() time.Time { return start.Add(40 * 24 * time.Hour) })
	require.NoError(t, s.Prune(ctx, config.Retention{ReceiptsDays: 14, EventsDays: 30, IndexDays: 60}, nil))

	events, err := s.ListEvents(ctx, store.EventQuery{})
	require.NoError(t, err)
	assert.Empty(t, events, "a pending row of a sink that is no longer configured goes after EventsDays")
}

func TestLookupReportsStorageErrors(t *testing.T) {
	s := storetest.New(t).Store()
	l := s.Lookup(ctx)
	_, ok := l.BySHA(prevents.GitHub, "kevinle128/demo", "aaaa")
	assert.False(t, ok)
	assert.NoError(t, l.Err(), "not found is not an error")

	gone, cancel := context.WithCancel(ctx)
	cancel()
	l = s.Lookup(gone)
	_, ok = l.Get(prevents.GitHub, "kevinle128/demo", 7)
	assert.False(t, ok)
	assert.Error(t, l.Err(), "a failed read must be reported, not taken as not found")
}

func TestTagPipelineIsNotTakenForABranch(t *testing.T) {
	s := storetest.New(t).Store()
	glDeliver(t, s, "d1", glMergeRequest(3, 11, "lan.nguyen", "open"))
	// A tag that has the name of the merge request's source branch.
	body := []byte(fmt.Sprintf(`{"object_kind":"pipeline","user":{"id":9,"username":"kevin.le"},
		"project":{"web_url":"https://git.example.test/%[1]s","path_with_namespace":%[1]q},
		"object_attributes":{"id":502,"ref":"develop","tag":true,"sha":"deadbeef","status":"failed"},
		"merge_request":null,"builds":[]}`, glRepo))
	events, drops := glDeliver(t, s, "d2", body)
	assert.Empty(t, events)
	assert.Equal(t, []string{normalize.DropNoPullRequest}, drops)
}

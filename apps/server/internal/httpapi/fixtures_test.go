package httpapi_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/store"
	"git-event-hub/internal/testutil"
)

const kevinSinks = "kevin/flaky,kevin/omb"

// fixtureExpect says what each fixture must produce: one "kind>sinks" entry
// per queued event, or the reason the payload is dropped. Every file in
// testdata must have an entry.
var fixtureExpect = map[string]struct {
	events []string
	drop   string
}{
	"github/pull_request.opened.json":                          {events: []string{"opened>" + kevinSinks}},
	"github/pull_request.synchronize.json":                     {events: []string{"pushed>" + kevinSinks}},
	"github/pull_request.synchronize-conflict.json":            {events: []string{"pushed>" + kevinSinks, "conflict>" + kevinSinks}},
	"github/pull_request.closed-merged.json":                   {events: []string{"merged>" + kevinSinks}},
	"github/pull_request.closed-unmerged.json":                 {events: []string{"closed>" + kevinSinks}},
	"github/check_run.completed-failure.json":                  {events: []string{"ci.finished>" + kevinSinks}},
	"github/check_run.completed-success.json":                  {drop: "check_run_not_failed"},
	"github/check_suite.completed-github-actions.json":         {drop: "covered_by_workflow_run"},
	"github/check_suite.completed-third-party.json":            {events: []string{"ci.finished>" + kevinSinks}},
	"github/check_suite.completed-third-party-foreign-pr.json": {drop: "no_pull_request"},
	"github/workflow_run.completed-failure.json":               {events: []string{"ci.finished>" + kevinSinks}},
	"github/status.ci-failure.json":                            {events: []string{"ci.finished>" + kevinSinks}},
	"github/status.ci-pending.json":                            {drop: "not_finished"},
	"github/status.ocr-review-success.json":                    {drop: "review_status_context"},
	"github/issue_comment.ocr-summary.json":                    {events: []string{"review.finished>" + kevinSinks}},
	"github/issue_comment.ocr-summary-edited.json":             {events: []string{"review.finished>" + kevinSinks}},
	"github/issue_comment.ocr-summary-edited-by-person.json":   {drop: "review_author_out_of_scope"},
	"github/issue_comment.ocr-round.json":                      {events: []string{"comment>" + kevinSinks}},
	"github/issue_comment.human.json":                          {drop: "comment_author_out_of_scope"},
	"github/issue_comment.fake-marker-by-human.json":           {drop: "review_author_out_of_scope"},
	"github/issue_comment.on-issue.json":                       {drop: "not_a_pull_request"},

	"gitlab/merge_request.open.json":                      {events: []string{"opened>" + kevinSinks}},
	"gitlab/merge_request.reopen.json":                    {events: []string{"opened>" + kevinSinks}},
	"gitlab/merge_request.update-push.json":               {events: []string{"pushed>" + kevinSinks}},
	"gitlab/merge_request.update-push-by-other-user.json": {events: []string{"pushed>" + kevinSinks}},
	"gitlab/merge_request.update-conflict.json":           {events: []string{"conflict>" + kevinSinks}},
	"gitlab/merge_request.update-title.json":              {drop: "no_head_change"},
	"gitlab/merge_request.approved.json":                  {drop: "unsupported_action"},
	"gitlab/merge_request.merge.json":                     {events: []string{"merged>" + kevinSinks}},
	"gitlab/merge_request.close.json":                     {events: []string{"closed>" + kevinSinks}},
	"gitlab/merge_request.open-by-other-author.json":      {events: []string{"opened>minh/hook"}},
	"gitlab/pipeline.mr-failed.json":                      {events: []string{"ci.finished>" + kevinSinks}},
	"gitlab/pipeline.mr-success.json":                     {events: []string{"ci.finished>" + kevinSinks}},
	"gitlab/pipeline.mr-running.json":                     {drop: "not_finished"},
	"gitlab/pipeline.branch-failed.json":                  {events: []string{"ci.finished>" + kevinSinks}},
	"gitlab/note.mr-ocr-summary.json":                     {events: []string{"review.finished>" + kevinSinks}},
	"gitlab/note.mr-ocr-round.json":                       {events: []string{"comment>" + kevinSinks}},
	"gitlab/note.mr-human.json":                           {drop: "comment_author_out_of_scope"},
	"gitlab/note.system.json":                             {drop: "system_note"},
	"gitlab/note.issue.json":                              {drop: "not_a_pull_request"},
	// Sent to a hub that has not seen the merge request: a note names the
	// author only by id, so the hub cannot tell the owner and drops it.
	"gitlab/note.mr-by-other-mr-author.json": {drop: "no_owner"},
}

// freshHub lists the fixtures that go to an empty hub.
var freshHub = map[string]bool{"gitlab/note.mr-by-other-mr-author.json": true}

// fixtureOrder sends the "opened" fixtures first, as a forge would, so the
// hub knows each pull request before its checks and comments arrive.
func fixtureOrder(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, dir := range []string{"github", "gitlab"} {
		entries, err := os.ReadDir(filepath.Join(testutil.Root(), dir))
		require.NoError(t, err)
		for _, e := range entries {
			names = append(names, dir+"/"+e.Name())
		}
	}
	first := map[string]int{"github/pull_request.opened.json": 0, "gitlab/merge_request.open.json": 1}
	sort.SliceStable(names, func(i, j int) bool {
		fi, oki := first[names[i]]
		fj, okj := first[names[j]]
		switch {
		case oki && okj:
			return fi < fj
		case oki != okj:
			return oki
		}
		return names[i] < names[j]
	})
	return names
}

func send(h *testHub, name string) hookResult {
	body := testutil.Fixture(h.t, name)
	if strings.HasPrefix(name, "github/") {
		return h.github(strings.SplitN(filepath.Base(name), ".", 2)[0], name, body)
	}
	return h.gitlab(name, body)
}

func queued(t *testing.T, h *testHub, name string) []string {
	t.Helper()
	evs, err := h.hub.Store().ListEvents(context.Background(), store.EventQuery{Limit: store.MaxPage})
	require.NoError(t, err)
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if !strings.Contains(e.Event.ID, ":"+name+":") {
			continue
		}
		var keys []string
		for _, x := range e.Targets {
			keys = append(keys, x.Subscriber+"/"+x.Sink)
		}
		sort.Strings(keys)
		out = append(out, string(e.Event.Kind)+">"+strings.Join(keys, ","))
	}
	return out
}

func TestEveryFixtureThroughTheHub(t *testing.T) {
	h := newHub(t, teamConfig)
	fresh := newHub(t, teamConfig)
	for _, name := range fixtureOrder(t) {
		t.Run(name, func(t *testing.T) {
			want, ok := fixtureExpect[name]
			require.True(t, ok, "fixture %s has no expectation", name)
			target := h
			if freshHub[name] {
				target = fresh
			}
			src := strings.SplitN(name, "/", 2)[0]
			before := target.hub.Metrics().Value("hub_dropped_total", "source", src, "reason", want.drop)
			r := send(target, name)
			require.Equal(t, http.StatusAccepted, r.Status, "%+v", r)
			assert.Equal(t, len(want.events), r.Accepted)
			if want.drop != "" {
				assert.Equal(t, 1, r.Dropped)
				assert.Equal(t, before+1, target.hub.Metrics().Value("hub_dropped_total", "source", src, "reason", want.drop))
				assert.Empty(t, queued(t, target, name))
				return
			}
			assert.Equal(t, want.events, queued(t, target, name))
		})
	}
	assert.Len(t, fixtureExpect, len(fixtureOrder(t)), "an expectation names a fixture that does not exist")
}

package omb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/pkg/prevents"
)

const (
	testHead = "9f2c1d7e4b6a8035c1e2d4f6a8b0c2e4d6f81a3b"
	prURL    = "https://github.com/acme/widgets/pull/42"
	runURL   = "https://github.com/acme/widgets/actions/runs/777/job/888"
)

func sp(s string) *string { return &s }

// fixtureCase is a pr-event v1 event that carries the same facts as one
// valid fixture of OMB.
type fixtureCase struct {
	fixture string
	event   prevents.Event
}

func fixtureCases(provider prevents.Provider) []fixtureCase {
	mk := func(n int, kind prevents.Kind, data any) prevents.Event {
		return prevents.Event{
			V: 1, ID: "github:delivery-000" + string(rune('0'+n)) + ":0", Provider: provider,
			Repo: "acme/widgets", Number: 42, Kind: kind,
			HeadSHA: sp(testHead), BaseRef: sp("main"), Branch: sp("feat/widget"), Author: sp("octocat"),
			OccurredAt: time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC), Data: data,
		}
	}
	return []fixtureCase{
		{"pr.opened", mk(1, prevents.KindOpened, prevents.OpenedData{Title: "Add the widget", URL: prURL})},
		{"pr.pushed", mk(2, prevents.KindPushed, prevents.PushedData{
			Before: sp("1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"), After: testHead, URL: prURL})},
		{"pr.ci.finished", mk(3, prevents.KindCIFinished, prevents.CIFinishedData{
			Source: "github-actions", Scope: prevents.ScopeJob, Name: "test", Status: prevents.CIFailed,
			URL: runURL, RunID: "777",
			FailedJobs: []prevents.FailedJob{{ID: "888", Name: "test", URL: runURL, Stage: "build", Conclusion: "failure"}}})},
		{"pr.review.finished", mk(4, prevents.KindReviewFinished, prevents.ReviewFinishedData{
			Source: "ocr", Status: prevents.ReviewNeedsPerson, ReviewedSHA: testHead, Rounds: 2, MaxRounds: 3,
			Items:     []prevents.ReviewItem{{Round: 2, Scope: "full", Real: 2, Fixed: 1, Unresolved: 1}},
			Commenter: "ocr-serve[bot]", CommentID: "4001", CommentURL: prURL + "#issuecomment-4001"})},
		{"pr.comment.created", mk(5, prevents.KindComment, prevents.CommentData{
			CommentID: "4002", URL: prURL + "#issuecomment-4002", Commenter: "octocat", Body: "Looks good to me."})},
		{"pr.conflict.detected", mk(6, prevents.KindConflict, prevents.ConflictData{State: "dirty", URL: prURL})},
		{"pr.merged", mk(7, prevents.KindMerged, prevents.MergedData{
			MergeCommitSHA: sp("c0ffee1234567890abcdef1234567890abcdef12"), MergedBy: sp("octocat"), URL: prURL})},
		{"pr.closed", mk(8, prevents.KindClosed, prevents.ClosedData{ClosedBy: sp("octocat"), URL: prURL})},
	}
}

func asMap(t testing.TB, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func readFixture(t testing.TB, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "valid", name+".json"))
	require.NoError(t, err)
	return asMap(t, raw)
}

// prURLFromOtherURL names the kinds whose event URL is not the PR URL.
var prURLFromOtherURL = map[string]bool{"pr.ci.finished": true, "pr.review.finished": true, "pr.comment.created": true}

func mapAndMarshal(t testing.TB, e prevents.Event, connector string) []byte {
	t.Helper()
	m, err := toOMB(e, connector)
	require.NoError(t, err)
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	return raw
}

func TestMappingMatchesOMBFixturesForGitHub(t *testing.T) {
	for _, c := range fixtureCases(prevents.GitHub) {
		t.Run(c.fixture, func(t *testing.T) {
			raw := mapAndMarshal(t, c.event, "github-event")
			want := readFixture(t, c.fixture)
			if prURLFromOtherURL[c.fixture] {
				delete(want["data"].(map[string]any), "url")
				want["data"].(map[string]any)["url"] = nil
				delete(want["correlation"].(map[string]any), "prUrl")
			}
			assert.Equal(t, want, asMap(t, raw))
			require.NoError(t, checkOMB(t, raw, "github-event", "github"))
		})
	}
}

func TestMappingMatchesOMBFixturesForGitLab(t *testing.T) {
	for _, c := range fixtureCases(prevents.GitLab) {
		t.Run(c.fixture, func(t *testing.T) {
			raw := mapAndMarshal(t, c.event, "gitlab-event")
			want := readFixture(t, c.fixture)
			want["source"].(map[string]any)["connector"] = "gitlab-event"
			want["subject"].(map[string]any)["forge"] = "gitlab"
			if prURLFromOtherURL[c.fixture] {
				want["data"].(map[string]any)["url"] = nil
				delete(want["correlation"].(map[string]any), "prUrl")
			}
			assert.Equal(t, want, asMap(t, raw))
			require.NoError(t, checkOMB(t, raw, "gitlab-event", "gitlab"))
		})
	}
}

func TestMappingLeavesPRURLNullWhenTheKindCarriesAnotherURL(t *testing.T) {
	for _, c := range fixtureCases(prevents.GitHub) {
		if !prURLFromOtherURL[c.fixture] {
			continue
		}
		t.Run(c.fixture, func(t *testing.T) {
			got := asMap(t, mapAndMarshal(t, c.event, "github-event"))
			data := got["data"].(map[string]any)
			v, present := data["url"]
			assert.True(t, present, "data.url must be written")
			assert.Nil(t, v)
			assert.NotContains(t, got["correlation"], "prUrl")
		})
	}
	got := asMap(t, mapAndMarshal(t, fixtureCases(prevents.GitHub)[2].event, "github-event"))
	assert.Equal(t, runURL, got["data"].(map[string]any)["check"].(map[string]any)["url"])
	got = asMap(t, mapAndMarshal(t, fixtureCases(prevents.GitHub)[3].event, "github-event"))
	assert.Equal(t, prURL+"#issuecomment-4001", got["data"].(map[string]any)["review"].(map[string]any)["commentUrl"])
	got = asMap(t, mapAndMarshal(t, fixtureCases(prevents.GitHub)[4].event, "github-event"))
	assert.Equal(t, prURL+"#issuecomment-4002", got["data"].(map[string]any)["comment"].(map[string]any)["url"])
}

func TestMappingKeepsUnknownRefsNull(t *testing.T) {
	e := fixtureCases(prevents.GitHub)[0].event
	e.HeadSHA, e.BaseRef, e.Branch, e.Author = nil, nil, nil, nil
	e.Data = prevents.OpenedData{Title: "t"}
	raw := mapAndMarshal(t, e, "github-event")
	got := asMap(t, raw)
	data := got["data"].(map[string]any)
	for _, k := range []string{"headSha", "baseRef", "branch", "author", "url"} {
		v, present := data[k]
		assert.True(t, present, k)
		assert.Nil(t, v, k)
	}
	assert.NotContains(t, got, "correlation")
	require.NoError(t, checkOMB(t, raw, "github-event", "github"))
}

func TestMappingTurnsNilListsIntoEmptyArrays(t *testing.T) {
	cases := fixtureCases(prevents.GitHub)
	ci, review := cases[2].event, cases[3].event
	d := ci.Data.(prevents.CIFinishedData)
	d.FailedJobs = nil
	ci.Data = d
	r := review.Data.(prevents.ReviewFinishedData)
	r.Items = nil
	review.Data = r
	rawCI := mapAndMarshal(t, ci, "github-event")
	assert.Contains(t, string(rawCI), `"failedJobs":[]`)
	require.NoError(t, checkOMB(t, rawCI, "github-event", "github"))
	rawReview := mapAndMarshal(t, review, "github-event")
	assert.Contains(t, string(rawReview), `"items":[]`)
	require.NoError(t, checkOMB(t, rawReview, "github-event", "github"))
}

func TestMappingCoversEveryPREventKind(t *testing.T) {
	byKind := map[prevents.Kind]prevents.Event{}
	for _, c := range fixtureCases(prevents.GitHub) {
		byKind[c.event.Kind] = c.event
	}
	for _, k := range prevents.Kinds {
		t.Run(string(k), func(t *testing.T) {
			assert.NotEmpty(t, typeOf[k], "kind has no OMB type")
			e, ok := byKind[k]
			require.True(t, ok, "no sample for the kind")
			_, err := toOMB(e, "github-event")
			require.NoError(t, err)
		})
	}
}

func TestMappingRefusesTheOtherForge(t *testing.T) {
	e := fixtureCases(prevents.GitLab)[0].event
	_, err := toOMB(e, "github-event")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gitlab")
	assert.Contains(t, err.Error(), "github-event")
}

func TestMappingRefusesDataThatDoesNotFitTheKind(t *testing.T) {
	e := fixtureCases(prevents.GitHub)[0].event
	e.Data = prevents.MergedData{}
	_, err := toOMB(e, "github-event")
	require.Error(t, err)
}

func TestMappingWritesUTC(t *testing.T) {
	e := fixtureCases(prevents.GitHub)[0].event
	e.OccurredAt = time.Date(2026, 11, 22, 10, 0, 0, 0, time.FixedZone("ICT", 7*3600))
	got := asMap(t, mapAndMarshal(t, e, "github-event"))
	assert.Equal(t, "2026-11-22T03:00:00Z", got["occurredAt"])
}

// TestWriteMappedEventsForOMB writes the mapped events of both forges to the
// file named by OMB_EVENTS_OUT, so OMB's own validator can check them.
func TestWriteMappedEventsForOMB(t *testing.T) {
	out := os.Getenv("OMB_EVENTS_OUT")
	if out == "" {
		t.Skip("OMB_EVENTS_OUT is not set")
	}
	all := map[string][]json.RawMessage{}
	for forge, connector := range map[prevents.Provider]string{prevents.GitHub: "github-event", prevents.GitLab: "gitlab-event"} {
		for _, c := range fixtureCases(forge) {
			all[string(forge)] = append(all[string(forge)], mapAndMarshal(t, c.event, connector))
		}
	}
	raw, err := json.Marshal(all)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(out, raw, 0o600))
}

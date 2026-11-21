package normalize

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"git-event-hub/internal/testutil"
	"git-event-hub/pkg/prevents"
	"git-event-hub/schema"
)

type fakeIndex struct{ byNumber map[string]Entry }

func key(p prevents.Provider, repo string, n int) string {
	return string(p) + "|" + repo + "|" + itoa(n)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (f fakeIndex) Get(p prevents.Provider, repo string, n int) (Entry, bool) {
	e, ok := f.byNumber[key(p, repo, n)]
	return e, ok
}
func (f fakeIndex) ByBranch(p prevents.Provider, repo, branch string) (Entry, bool) {
	for k, e := range f.byNumber {
		if strings.HasPrefix(k, string(p)+"|"+repo+"|") && e.Branch == branch {
			return e, true
		}
	}
	return Entry{}, false
}
func (f fakeIndex) BySHA(p prevents.Provider, repo, sha string) (Entry, bool) {
	for k, e := range f.byNumber {
		if strings.HasPrefix(k, string(p)+"|"+repo+"|") && e.HeadSHA == sha {
			return e, true
		}
	}
	return Entry{}, false
}

var schemaOnce *jsonschema.Schema

func eventSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	if schemaOnce == nil {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema.PREventV1)))
		if err != nil {
			t.Fatal(err)
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("s.json", doc); err != nil {
			t.Fatal(err)
		}
		if schemaOnce, err = c.Compile("s.json"); err != nil {
			t.Fatal(err)
		}
	}
	return schemaOnce
}

// run normalizes a fixture and checks that every event follows the contract
// and its JSON Schema. It returns the events and the drop reasons.
func run(t *testing.T, idx Index, fn func(Context) ([]prevents.Event, error)) ([]prevents.Event, []string) {
	t.Helper()
	var drops []string
	events, err := fn(Context{
		DeliveryID: "d1", ReceivedAt: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), Index: idx,
		Drop: func(r string) { drops = append(drops, r) },
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	for i, e := range events {
		if err := e.Validate(); err != nil {
			t.Fatalf("event %d is invalid: %v", i, err)
		}
		raw, _ := json.Marshal(e)
		inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
		if err := eventSchema(t).Validate(inst); err != nil {
			t.Fatalf("event %d does not match the schema: %v\n%s", i, err, raw)
		}
		if want := "github:d1:" + itoa(i); e.Provider == prevents.GitHub && e.ID != want {
			t.Errorf("event id = %s, want %s", e.ID, want)
		}
	}
	return events, drops
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestGitHubPullRequest(t *testing.T) {
	const headSHA = "0bc93cc2e4122a993e4a73c4531beae821cddd67"
	tests := []struct {
		name, fixture string
		kinds         []prevents.Kind
		check         func(t *testing.T, e []prevents.Event)
	}{
		{"opened", "pull_request.opened.json", []prevents.Kind{prevents.KindOpened}, func(t *testing.T, e []prevents.Event) {
			if e[0].Number != 40 || e[0].Repo != "kevinle128/OpenMausBot" || str(e[0].HeadSHA) != headSHA {
				t.Errorf("event = %+v", e[0])
			}
			if str(e[0].BaseRef) != "main" || str(e[0].Branch) != "fix/desktop-update-with-remote-home" || str(e[0].Author) != "kevinle128" {
				t.Errorf("refs = %s %s %s", str(e[0].BaseRef), str(e[0].Branch), str(e[0].Author))
			}
			if d := e[0].Data.(prevents.OpenedData); d.Title == "" || d.Reopened {
				t.Errorf("data = %+v", d)
			}
		}},
		{"pushed", "pull_request.synchronize.json", []prevents.Kind{prevents.KindPushed}, func(t *testing.T, e []prevents.Event) {
			d := e[0].Data.(prevents.PushedData)
			if str(d.Before) != "ad6a6b62b4bdd5382d4ee1d57391275d0b30ccd9" || d.After != headSHA {
				t.Errorf("data = %+v", d)
			}
		}},
		{"push into a conflict gives two events", "pull_request.synchronize-conflict.json", []prevents.Kind{prevents.KindPushed, prevents.KindConflict}, func(t *testing.T, e []prevents.Event) {
			if e[1].Data.(prevents.ConflictData).State != "dirty" || e[1].ID != "github:d1:1" {
				t.Errorf("conflict = %+v", e[1])
			}
		}},
		{"merged", "pull_request.closed-merged.json", []prevents.Kind{prevents.KindMerged}, func(t *testing.T, e []prevents.Event) {
			d := e[0].Data.(prevents.MergedData)
			if str(d.MergedBy) != "kevinle128" || d.MergeCommitSHA == nil {
				t.Errorf("data = %+v", d)
			}
		}},
		{"closed without merge", "pull_request.closed-unmerged.json", []prevents.Kind{prevents.KindClosed}, func(t *testing.T, e []prevents.Event) {
			if str(e[0].Data.(prevents.ClosedData).ClosedBy) != "kevinle128" {
				t.Errorf("data = %+v", e[0].Data)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := testutil.Fixture(t, "github/"+tt.fixture)
			events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("pull_request", body, c) })
			if len(drops) != 0 {
				t.Errorf("drops = %v", drops)
			}
			if len(events) != len(tt.kinds) {
				t.Fatalf("got %d events, want %d", len(events), len(tt.kinds))
			}
			for i, k := range tt.kinds {
				if events[i].Kind != k {
					t.Errorf("event %d kind = %s, want %s", i, events[i].Kind, k)
				}
			}
			tt.check(t, events)
		})
	}
}

func TestGitHubPullRequestOtherActionIsDropped(t *testing.T) {
	body := testutil.Fixture(t, "github/pull_request.opened.json")
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["action"] = "labeled"
	body, _ = json.Marshal(m)
	events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("pull_request", body, c) })
	if len(events) != 0 || len(drops) != 1 || drops[0] != DropUnsupportedAction {
		t.Fatalf("events = %v drops = %v", events, drops)
	}
}

func TestGitHubChecks(t *testing.T) {
	t.Run("workflow run failed", func(t *testing.T) {
		body := testutil.Fixture(t, "github/workflow_run.completed-failure.json")
		events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("workflow_run", body, c) })
		if len(events) != 1 {
			t.Fatalf("events = %d", len(events))
		}
		e := events[0]
		d := e.Data.(prevents.CIFinishedData)
		if e.Number != 40 || d.Status != prevents.CIFailed || d.Scope != prevents.ScopeWorkflow || d.Source != "github-actions" || d.RunID == "" || len(d.FailedJobs) != 0 {
			t.Errorf("event = %+v data = %+v", e, d)
		}
	})
	t.Run("check run failed lists the job", func(t *testing.T) {
		body := testutil.Fixture(t, "github/check_run.completed-failure.json")
		events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("check_run", body, c) })
		if len(events) != 1 {
			t.Fatalf("events = %d", len(events))
		}
		d := events[0].Data.(prevents.CIFinishedData)
		if d.Scope != prevents.ScopeJob || len(d.FailedJobs) != 1 || d.FailedJobs[0].Name != "CI" || d.FailedJobs[0].ID == "" || !strings.HasPrefix(d.FailedJobs[0].URL, "https://github.com/") {
			t.Errorf("data = %+v", d)
		}
	})
	t.Run("check run passed is dropped", func(t *testing.T) {
		body := testutil.Fixture(t, "github/check_run.completed-success.json")
		events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("check_run", body, c) })
		if len(events) != 0 || len(drops) != 1 || drops[0] != DropCheckRunNotFailed {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
	t.Run("suite of github actions is covered by the workflow run", func(t *testing.T) {
		body := testutil.Fixture(t, "github/check_suite.completed-github-actions.json")
		events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("check_suite", body, c) })
		if len(events) != 0 || len(drops) != 1 || drops[0] != DropCoveredByWorkflow {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
	t.Run("suite of another app", func(t *testing.T) {
		body := testutil.Fixture(t, "github/check_suite.completed-third-party.json")
		events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("check_suite", body, c) })
		if len(events) != 1 {
			t.Fatalf("events = %d", len(events))
		}
		d := events[0].Data.(prevents.CIFinishedData)
		if d.Source != "circleci-checks" || d.Scope != prevents.ScopeSuite || d.Status != prevents.CIFailed {
			t.Errorf("data = %+v", d)
		}
	})
	t.Run("no pull request in the payload and none known", func(t *testing.T) {
		body := testutil.Fixture(t, "github/workflow_run.completed-failure.json")
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		m["workflow_run"].(map[string]any)["pull_requests"] = []any{}
		body, _ = json.Marshal(m)
		events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("workflow_run", body, c) })
		if len(events) != 0 || drops[0] != DropNoPullRequest {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
	t.Run("no pull request in the payload but the head is known", func(t *testing.T) {
		body := testutil.Fixture(t, "github/workflow_run.completed-failure.json")
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		m["workflow_run"].(map[string]any)["pull_requests"] = []any{}
		body, _ = json.Marshal(m)
		idx := fakeIndex{map[string]Entry{key(prevents.GitHub, "kevinle128/OpenMausBot", 40): {Number: 40, Author: "kevinle128", Branch: "b", BaseRef: "main", HeadSHA: "0bc93cc2e4122a993e4a73c4531beae821cddd67"}}}
		events, _ := run(t, idx, func(c Context) ([]prevents.Event, error) { return GitHub("workflow_run", body, c) })
		if len(events) != 1 || events[0].Number != 40 || str(events[0].Author) != "kevinle128" {
			t.Fatalf("events = %+v", events)
		}
	})
}

func TestGitHubStatus(t *testing.T) {
	idx := fakeIndex{map[string]Entry{key(prevents.GitHub, "kevinle128/OpenMausBot", 40): {Number: 40, Author: "kevinle128", HeadSHA: "0bc93cc2e4122a993e4a73c4531beae821cddd67"}}}
	tests := []struct {
		fixture  string
		wantDrop string
		status   string
	}{
		{"status.ocr-review-success.json", DropReviewStatus, ""},
		{"status.ci-pending.json", DropNotFinished, ""},
		{"status.ci-failure.json", "", prevents.CIFailed},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			body := testutil.Fixture(t, "github/"+tt.fixture)
			events, drops := run(t, idx, func(c Context) ([]prevents.Event, error) { return GitHub("status", body, c) })
			if tt.wantDrop != "" {
				if len(events) != 0 || len(drops) != 1 || drops[0] != tt.wantDrop {
					t.Fatalf("events = %d drops = %v", len(events), drops)
				}
				return
			}
			d := events[0].Data.(prevents.CIFinishedData)
			if d.Status != tt.status || d.Scope != prevents.ScopeStatus || len(d.FailedJobs) != 1 || events[0].Number != 40 {
				t.Errorf("data = %+v", d)
			}
		})
	}
	t.Run("unknown commit", func(t *testing.T) {
		body := testutil.Fixture(t, "github/status.ci-failure.json")
		events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("status", body, c) })
		if len(events) != 0 || drops[0] != DropNoPullRequest {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
}

func TestGitHubIssueComment(t *testing.T) {
	t.Run("review summary becomes review.finished with the reviewed sha from the machine block", func(t *testing.T) {
		body := testutil.Fixture(t, "github/issue_comment.ocr-summary.json")
		for _, event := range []string{"issue_comment"} {
			events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub(event, body, c) })
			if len(events) != 1 || events[0].Kind != prevents.KindReviewFinished {
				t.Fatalf("events = %+v", events)
			}
			d := events[0].Data.(prevents.ReviewFinishedData)
			if d.Status != prevents.ReviewSuccess || d.ReviewedSHA != "0bc93cc2e4122a993e4a73c4531beae821cddd67" || d.Rounds != 2 || d.MaxRounds != 3 || len(d.Items) != 0 {
				t.Errorf("data = %+v", d)
			}
			if d.Commenter != "kevinle128-ocr[bot]" || d.CommentID != "7064678333" || events[0].Number != 40 || str(events[0].Author) != "kevinle128" {
				t.Errorf("data = %+v event = %+v", d, events[0])
			}
		}
	})
	t.Run("edited summary also counts", func(t *testing.T) {
		body := testutil.Fixture(t, "github/issue_comment.ocr-summary-edited.json")
		events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("issue_comment", body, c) })
		if len(events) != 1 || events[0].Kind != prevents.KindReviewFinished {
			t.Fatalf("events = %+v", events)
		}
	})
	t.Run("round comment becomes comment without reading its HTML", func(t *testing.T) {
		body := testutil.Fixture(t, "github/issue_comment.ocr-round.json")
		events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("issue_comment", body, c) })
		if len(events) != 1 || events[0].Kind != prevents.KindComment {
			t.Fatalf("events = %+v", events)
		}
		d := events[0].Data.(prevents.CommentData)
		if d.Commenter != "kevinle128-ocr[bot]" || !strings.Contains(d.Body, "OCR review, round 1/3") || d.MarkerError != "" {
			t.Errorf("data = %+v", d)
		}
	})
	t.Run("comment on a plain issue is dropped", func(t *testing.T) {
		body := testutil.Fixture(t, "github/issue_comment.on-issue.json")
		events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("issue_comment", body, c) })
		if len(events) != 0 || drops[0] != DropNotPullRequest {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
	t.Run("a long body is cut at a character boundary", func(t *testing.T) {
		body := testutil.Fixture(t, "github/issue_comment.human.json")
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		m["comment"].(map[string]any)["body"] = strings.Repeat("é", prevents.MaxCommentBytes)
		body, _ = json.Marshal(m)
		events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("issue_comment", body, c) })
		d := events[0].Data.(prevents.CommentData)
		if !d.BodyTruncated || len(d.Body) > prevents.MaxCommentBytes || strings.ContainsRune(d.Body, '�') {
			t.Errorf("truncated = %v len = %d", d.BodyTruncated, len(d.Body))
		}
	})
	t.Run("a running chain gives nothing", func(t *testing.T) {
		body := testutil.Fixture(t, "github/issue_comment.ocr-summary.json")
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		running := `{"v":2,"status":"running","max":3,"rounds":[]}`
		m["comment"].(map[string]any)["body"] = "<!-- ocr-serve:v2 " + b64(running) + " -->"
		body, _ = json.Marshal(m)
		events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("issue_comment", body, c) })
		if len(events) != 0 || drops[0] != DropReviewRunning {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
	t.Run("a broken block keeps the comment and says why", func(t *testing.T) {
		body := testutil.Fixture(t, "github/issue_comment.ocr-summary.json")
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		m["comment"].(map[string]any)["body"] = "<!-- ocr-serve:v2 AAAA -->"
		body, _ = json.Marshal(m)
		events, _ := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("issue_comment", body, c) })
		if events[0].Kind != prevents.KindComment || events[0].Data.(prevents.CommentData).MarkerError == "" {
			t.Fatalf("event = %+v", events[0])
		}
	})
}

func TestGitHubUnsupportedAndBadPayload(t *testing.T) {
	events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("push", []byte(`{}`), c) })
	if len(events) != 0 || drops[0] != DropUnsupportedEvent {
		t.Fatalf("events = %d drops = %v", len(events), drops)
	}
	events, drops = run(t, nil, func(c Context) ([]prevents.Event, error) { return GitHub("ping", []byte(`{"zen":"x"}`), c) })
	if len(events) != 0 || drops[0] != DropPing {
		t.Fatalf("ping: events = %d drops = %v", len(events), drops)
	}
	_, err := GitHub("pull_request", []byte(`{"action":"opened"}`), Context{DeliveryID: "d"})
	if err == nil || !strings.Contains(err.Error(), ErrPayload.Error()) {
		t.Fatalf("err = %v", err)
	}
	if _, err := GitHub("pull_request", []byte(`not json`), Context{DeliveryID: "d"}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

package normalize

import (
	"strings"
	"testing"

	"git-event-hub/internal/testutil"
	"git-event-hub/pkg/prevents"
)

const (
	glRepo = "vtc/3q-chess/3q-chess-cms"
	glSHA  = "da1560886d4f094c3e6c9ef40349f7d38b5d27d7"
)

func glRun(t *testing.T, idx Index, fixture string) ([]prevents.Event, []string) {
	t.Helper()
	body := testutil.Fixture(t, "gitlab/"+fixture)
	events, drops := run(t, idx, func(c Context) ([]prevents.Event, error) { return GitLab(body, c) })
	for _, e := range events {
		if e.Provider != prevents.GitLab || e.Repo != glRepo || e.Number != 12 && e.Number != 13 {
			t.Errorf("unexpected event identity %+v", e)
		}
	}
	return events, drops
}

func knownMR() Index {
	return fakeIndex{map[string]Entry{key(prevents.GitLab, glRepo, 12): {Number: 12, Author: "kevin.le", Branch: "feat/cms-banner", BaseRef: "dev", HeadSHA: glSHA}}}
}

func TestGitLabMergeRequest(t *testing.T) {
	tests := []struct {
		fixture string
		idx     Index
		kinds   []prevents.Kind
		drop    string
		author  string
	}{
		{"merge_request.open.json", nil, []prevents.Kind{prevents.KindOpened}, "", "kevin.le"},
		{"merge_request.reopen.json", nil, []prevents.Kind{prevents.KindOpened}, "", "kevin.le"},
		{"merge_request.update-push.json", nil, []prevents.Kind{prevents.KindPushed}, "", "kevin.le"},
		{"merge_request.update-title.json", nil, nil, DropNoChange, "kevin.le"},
		// Another person pushes to my merge request. The webhook does not
		// name the author, so the hub knows it only from earlier events.
		{"merge_request.update-push-by-other-user.json", nil, []prevents.Kind{prevents.KindPushed}, "", "<nil>"},
		{"merge_request.update-push-by-other-user.json", knownMR(), []prevents.Kind{prevents.KindPushed}, "", "kevin.le"},
		{"merge_request.update-conflict.json", nil, []prevents.Kind{prevents.KindConflict}, "", "kevin.le"},
		{"merge_request.merge.json", nil, []prevents.Kind{prevents.KindMerged}, "", "kevin.le"},
		{"merge_request.close.json", nil, []prevents.Kind{prevents.KindClosed}, "", "kevin.le"},
		{"merge_request.approved.json", nil, nil, DropUnsupportedAction, ""},
		{"merge_request.open-by-other-author.json", nil, []prevents.Kind{prevents.KindOpened}, "", "minh.tran"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture+"/"+tt.author, func(t *testing.T) {
			events, drops := glRun(t, tt.idx, tt.fixture)
			if tt.drop != "" {
				if len(events) != 0 || len(drops) != 1 || drops[0] != tt.drop {
					t.Fatalf("events = %d drops = %v, want drop %s", len(events), drops, tt.drop)
				}
				return
			}
			if len(events) != len(tt.kinds) {
				t.Fatalf("got %d events, want %d", len(events), len(tt.kinds))
			}
			for i, k := range tt.kinds {
				if events[i].Kind != k {
					t.Errorf("event %d kind = %s, want %s", i, events[i].Kind, k)
				}
			}
			if got := str(events[0].Author); got != tt.author {
				t.Errorf("author = %s, want %s", got, tt.author)
			}
			if str(events[0].HeadSHA) != glSHA && tt.fixture != "merge_request.open-by-other-author.json" {
				t.Errorf("head = %s", str(events[0].HeadSHA))
			}
		})
	}
}

func TestGitLabMergeRequestFields(t *testing.T) {
	events, _ := glRun(t, nil, "merge_request.update-push.json")
	d := events[0].Data.(prevents.PushedData)
	if str(d.Before) != "91d2ac7c1bd5f6c6a2e0a1f3d84f3f5c2f6aa001" || d.After != glSHA {
		t.Errorf("data = %+v", d)
	}
	if str(events[0].Branch) != "feat/cms-banner" || str(events[0].BaseRef) != "dev" {
		t.Errorf("refs = %s %s", str(events[0].Branch), str(events[0].BaseRef))
	}
	if !events[0].OccurredAt.Equal(parseTime("2026-11-21 09:30:44 UTC")) {
		t.Errorf("occurredAt = %v", events[0].OccurredAt)
	}
	merged, _ := glRun(t, nil, "merge_request.merge.json")
	if str(merged[0].Data.(prevents.MergedData).MergedBy) != "kevin.le" {
		t.Errorf("merged = %+v", merged[0].Data)
	}
	if str(merged[0].Data.(prevents.MergedData).MergeCommitSHA) != "0b6d1c5e4a7f3e2d9c8b7a6f5e4d3c2b1a0f9e8d" {
		t.Errorf("mergeCommitSha = %v", merged[0].Data.(prevents.MergedData).MergeCommitSHA)
	}
}

func TestGitLabPipeline(t *testing.T) {
	t.Run("failed lists only jobs that may not fail", func(t *testing.T) {
		events, _ := glRun(t, knownMR(), "pipeline.mr-failed.json")
		if len(events) != 1 {
			t.Fatalf("events = %d", len(events))
		}
		d := events[0].Data.(prevents.CIFinishedData)
		if d.Status != prevents.CIFailed || d.Scope != prevents.ScopePipeline || d.Source != "gitlab-ci" || d.RunID != "77001" {
			t.Errorf("data = %+v", d)
		}
		if len(d.FailedJobs) != 1 || d.FailedJobs[0].Name != "unit-tests" || d.FailedJobs[0].ID != "5002" || d.FailedJobs[0].Stage != "test" ||
			d.FailedJobs[0].URL != "https://git.oceanlabs.dev/vtc/3q-chess/3q-chess-cms/-/jobs/5002" || d.FailedJobs[0].Conclusion != "script_failure" {
			t.Errorf("failed jobs = %+v", d.FailedJobs)
		}
		if d.URL != "https://git.oceanlabs.dev/vtc/3q-chess/3q-chess-cms/-/pipelines/77001" || str(events[0].Author) != "kevin.le" || events[0].Number != 12 {
			t.Errorf("event = %+v", events[0])
		}
	})
	t.Run("pipeline of a merge request that the hub never saw has no author", func(t *testing.T) {
		events, _ := glRun(t, nil, "pipeline.mr-failed.json")
		if str(events[0].Author) != "<nil>" {
			t.Errorf("author = %s", str(events[0].Author))
		}
	})
	t.Run("success", func(t *testing.T) {
		events, _ := glRun(t, nil, "pipeline.mr-success.json")
		d := events[0].Data.(prevents.CIFinishedData)
		if d.Status != prevents.CIPassed || d.FailedJobs == nil || len(d.FailedJobs) != 0 {
			t.Errorf("data = %+v", d)
		}
	})
	t.Run("running is dropped", func(t *testing.T) {
		events, drops := glRun(t, nil, "pipeline.mr-running.json")
		if len(events) != 0 || drops[0] != DropNotFinished {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
	t.Run("branch pipeline finds its merge request by branch", func(t *testing.T) {
		events, _ := glRun(t, knownMR(), "pipeline.branch-failed.json")
		if len(events) != 1 || events[0].Number != 12 || str(events[0].BaseRef) != "dev" {
			t.Fatalf("events = %+v", events)
		}
	})
	t.Run("tag pipeline is not matched to a branch", func(t *testing.T) {
		body := strings.Replace(string(testutil.Fixture(t, "gitlab/pipeline.branch-failed.json")), `"tag": false`, `"tag": true`, 1)
		events, drops := run(t, knownMR(), func(c Context) ([]prevents.Event, error) { return GitLab([]byte(body), c) })
		if len(events) != 0 || len(drops) != 1 || drops[0] != DropNoPullRequest {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
	t.Run("branch pipeline without a known merge request is dropped", func(t *testing.T) {
		events, drops := glRun(t, nil, "pipeline.branch-failed.json")
		if len(events) != 0 || drops[0] != DropNoPullRequest {
			t.Fatalf("events = %d drops = %v", len(events), drops)
		}
	})
}

func TestGitLabNote(t *testing.T) {
	t.Run("review summary", func(t *testing.T) {
		events, _ := glRun(t, knownMR(), "note.mr-ocr-summary.json")
		if len(events) != 1 || events[0].Kind != prevents.KindReviewFinished {
			t.Fatalf("events = %+v", events)
		}
		d := events[0].Data.(prevents.ReviewFinishedData)
		if d.Commenter != "ocr-review-bot" || d.ReviewedSHA != "0bc93cc2e4122a993e4a73c4531beae821cddd67" || d.Status != prevents.ReviewSuccess {
			t.Errorf("data = %+v", d)
		}
		if str(events[0].Author) != "kevin.le" || str(events[0].Branch) != "feat/cms-banner" {
			t.Errorf("event = %+v", events[0])
		}
	})
	t.Run("the commenter is not taken for the author", func(t *testing.T) {
		events, _ := glRun(t, nil, "note.mr-human.json")
		// The human comment is by kevin.le, who is also the author, but the
		// payload names the author only by id; without memory it stays unknown.
		if len(events) != 1 || str(events[0].Author) != "<nil>" || events[0].Data.(prevents.CommentData).Commenter != "kevin.le" {
			t.Fatalf("events = %+v", events)
		}
	})
	t.Run("round comment", func(t *testing.T) {
		events, _ := glRun(t, knownMR(), "note.mr-ocr-round.json")
		if len(events) != 1 || events[0].Kind != prevents.KindComment || !strings.Contains(events[0].Data.(prevents.CommentData).Body, "round 1/3") {
			t.Fatalf("events = %+v", events)
		}
	})
	t.Run("note on an issue and system note are dropped", func(t *testing.T) {
		_, drops := glRun(t, nil, "note.issue.json")
		_, drops2 := glRun(t, nil, "note.system.json")
		if drops[0] != DropNotPullRequest || drops2[0] != DropSystemNote {
			t.Fatalf("drops = %v %v", drops, drops2)
		}
	})
}

func TestGitLabUnsupportedAndBadPayload(t *testing.T) {
	events, drops := run(t, nil, func(c Context) ([]prevents.Event, error) {
		return GitLab([]byte(`{"object_kind":"push"}`), c)
	})
	if len(events) != 0 || drops[0] != DropUnsupportedEvent {
		t.Fatalf("events = %d drops = %v", len(events), drops)
	}
	if _, err := GitLab([]byte(`{"object_kind":"merge_request"}`), Context{DeliveryID: "d"}); err == nil {
		t.Fatal("an empty merge request payload was accepted")
	}
}

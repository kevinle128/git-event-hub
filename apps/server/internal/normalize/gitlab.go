package normalize

import (
	"encoding/json"
	"fmt"
	"strconv"

	"git-event-hub/pkg/prevents"
)

type glUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

type glProject struct {
	ID                int64  `json:"id"`
	WebURL            string `json:"web_url"`
	PathWithNamespace string `json:"path_with_namespace"`
}

// GitLab normalizes one webhook delivery. The payload's object_kind tells the
// event type, so the headers are not needed.
func GitLab(body []byte, c Context) ([]prevents.Event, error) {
	var head struct {
		ObjectKind string `json:"object_kind"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("%w: gitlab: %v", ErrPayload, err)
	}
	var (
		drafts []draft
		err    error
	)
	switch head.ObjectKind {
	case "merge_request":
		drafts, err = glMergeRequestDrafts(body, c)
	case "pipeline":
		drafts, err = glPipelineDrafts(body, c)
	case "note":
		drafts, err = glNoteDrafts(body, c)
	default:
		c.drop(DropUnsupportedEvent)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: gitlab %s: %v", ErrPayload, head.ObjectKind, err)
	}
	return c.finish(prevents.GitLab, drafts), nil
}

// glAuthor finds the username of the merge request author. The webhook gives
// only the numeric author id. The hub knows the name when the actor is the
// author, or from an earlier event of the same merge request.
func glAuthor(c Context, repo string, iid, authorID int, actor glUser) string {
	if e, ok := c.index().Get(prevents.GitLab, repo, iid); ok && e.Author != "" {
		return e.Author
	}
	if authorID != 0 && actor.ID == authorID {
		return actor.Username
	}
	return ""
}

func glMergeRequestDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		User    glUser    `json:"user"`
		Project glProject `json:"project"`
		Attrs   struct {
			IID                 int    `json:"iid"`
			Title               string `json:"title"`
			URL                 string `json:"url"`
			Action              string `json:"action"`
			SourceBranch        string `json:"source_branch"`
			TargetBranch        string `json:"target_branch"`
			AuthorID            int    `json:"author_id"`
			OldRev              string `json:"oldrev"`
			UpdatedAt           string `json:"updated_at"`
			Draft               bool   `json:"draft"`
			WorkInProgress      bool   `json:"work_in_progress"`
			MergeStatus         string `json:"merge_status"`
			DetailedMergeStatus string `json:"detailed_merge_status"`
			MergeCommitSHA      string `json:"merge_commit_sha"`
			LastCommit          struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	a := p.Attrs
	repo := p.Project.PathWithNamespace
	if repo == "" || a.IID < 1 {
		return nil, fmt.Errorf("missing project or iid")
	}
	// last_commit is null while the merge request has no commit of its own.
	head := a.LastCommit.ID
	if head == "" {
		if e, ok := c.index().Get(prevents.GitLab, repo, a.IID); ok {
			head = e.HeadSHA
		}
	}
	base := draft{
		repo: repo, number: a.IID, head: head, base: a.TargetBranch, branch: a.SourceBranch,
		author: glAuthor(c, repo, a.IID, a.AuthorID, p.User), at: parseTime(a.UpdatedAt),
	}
	conflict := a.DetailedMergeStatus == "conflict" || a.MergeStatus == "cannot_be_merged"
	var out []draft
	switch a.Action {
	case "open", "reopen":
		d := base
		d.kind = prevents.KindOpened
		d.data = prevents.OpenedData{Title: a.Title, URL: a.URL, Draft: a.Draft || a.WorkInProgress, Reopened: a.Action == "reopen"}
		out = append(out, d)
	case "update":
		if a.OldRev != "" && a.LastCommit.ID != "" {
			d := base
			d.kind = prevents.KindPushed
			d.data = prevents.PushedData{Before: nonEmpty(a.OldRev), After: a.LastCommit.ID, URL: a.URL}
			out = append(out, d)
		} else if !conflict {
			c.drop(DropNoChange)
		}
	case "merge":
		d := base
		d.kind = prevents.KindMerged
		d.data = prevents.MergedData{MergeCommitSHA: nonEmpty(a.MergeCommitSHA), MergedBy: nonEmpty(p.User.Username), URL: a.URL}
		return []draft{d}, nil
	case "close":
		d := base
		d.kind = prevents.KindClosed
		d.data = prevents.ClosedData{ClosedBy: nonEmpty(p.User.Username), URL: a.URL}
		return []draft{d}, nil
	default:
		c.drop(DropUnsupportedAction)
		return nil, nil
	}
	if conflict {
		d := base
		d.kind = prevents.KindConflict
		d.data = prevents.ConflictData{State: firstNonEmpty(a.DetailedMergeStatus, a.MergeStatus), URL: a.URL}
		out = append(out, d)
	}
	return out, nil
}

func glPipelineDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		User    glUser    `json:"user"`
		Project glProject `json:"project"`
		Attrs   struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Ref        string `json:"ref"`
			SHA        string `json:"sha"`
			Status     string `json:"status"`
			Tag        bool   `json:"tag"`
			FinishedAt string `json:"finished_at"`
			CreatedAt  string `json:"created_at"`
		} `json:"object_attributes"`
		MergeRequest *struct {
			IID             int    `json:"iid"`
			SourceBranch    string `json:"source_branch"`
			TargetBranch    string `json:"target_branch"`
			TargetProjectID int64  `json:"target_project_id"`
		} `json:"merge_request"`
		Builds []struct {
			ID            int64  `json:"id"`
			Stage         string `json:"stage"`
			Name          string `json:"name"`
			Status        string `json:"status"`
			AllowFailure  bool   `json:"allow_failure"`
			FailureReason string `json:"failure_reason"`
		} `json:"builds"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	a := p.Attrs
	repo := p.Project.PathWithNamespace
	if repo == "" || a.ID == 0 || a.SHA == "" {
		return nil, fmt.Errorf("missing project, pipeline id or sha")
	}
	var status string
	switch a.Status {
	case "success":
		status = prevents.CIPassed
	case "failed":
		status = prevents.CIFailed
	case "canceled":
		status = prevents.CICancelled
	case "skipped":
		status = prevents.CISkipped
	default:
		c.drop(DropNotFinished)
		return nil, nil
	}
	d := draft{repo: repo, head: a.SHA, branch: a.Ref, at: parseTime(a.FinishedAt, a.CreatedAt)}
	switch {
	case p.MergeRequest != nil && p.MergeRequest.IID > 0:
		// A fork pipeline runs in the fork, but its merge request number
		// belongs to the target project.
		if mr := p.MergeRequest; p.Project.ID != 0 && mr.TargetProjectID != 0 && mr.TargetProjectID != p.Project.ID {
			c.drop(DropNoPullRequest)
			return nil, nil
		}
		d.number = p.MergeRequest.IID
		d.base, d.branch = p.MergeRequest.TargetBranch, p.MergeRequest.SourceBranch
	case a.Tag:
		// A tag pipeline belongs to no merge request; its ref is a tag name,
		// which may equal a branch name.
		c.drop(DropNoPullRequest)
		return nil, nil
	default:
		e, ok := c.index().ByBranch(prevents.GitLab, repo, a.Ref)
		if !ok {
			c.drop(DropNoPullRequest)
			return nil, nil
		}
		d.number, d.base = e.Number, e.BaseRef
	}
	d.author = glAuthor(c, repo, d.number, 0, p.User)
	jobs := []prevents.FailedJob{}
	for _, b := range p.Builds {
		if b.Status == "failed" && !b.AllowFailure {
			jobs = append(jobs, prevents.FailedJob{
				ID: strconv.FormatInt(b.ID, 10), Name: b.Name, Stage: b.Stage,
				URL: fmt.Sprintf("%s/-/jobs/%d", p.Project.WebURL, b.ID), Conclusion: b.FailureReason,
			})
		}
	}
	d.kind = prevents.KindCIFinished
	d.data = prevents.CIFinishedData{
		Source: "gitlab-ci", Scope: prevents.ScopePipeline, Name: firstNonEmpty(a.Name, "pipeline"),
		Status: status, URL: fmt.Sprintf("%s/-/pipelines/%d", p.Project.WebURL, a.ID),
		RunID: strconv.FormatInt(a.ID, 10), FailedJobs: jobs,
	}
	return []draft{d}, nil
}

func glNoteDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		User    glUser    `json:"user"`
		Project glProject `json:"project"`
		Attrs   struct {
			ID           int64  `json:"id"`
			Note         string `json:"note"`
			NoteableType string `json:"noteable_type"`
			System       bool   `json:"system"`
			Action       string `json:"action"`
			URL          string `json:"url"`
			UpdatedAt    string `json:"updated_at"`
			CreatedAt    string `json:"created_at"`
		} `json:"object_attributes"`
		MergeRequest *struct {
			IID          int    `json:"iid"`
			SourceBranch string `json:"source_branch"`
			TargetBranch string `json:"target_branch"`
			LastCommit   struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"merge_request"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	a := p.Attrs
	repo := p.Project.PathWithNamespace
	if repo == "" || a.ID == 0 {
		return nil, fmt.Errorf("missing project or note id")
	}
	switch {
	case a.NoteableType != "MergeRequest" || p.MergeRequest == nil:
		c.drop(DropNotPullRequest)
		return nil, nil
	case a.System:
		c.drop(DropSystemNote)
		return nil, nil
	case a.Action != "" && a.Action != "create" && a.Action != "update":
		c.drop(DropUnsupportedAction)
		return nil, nil
	}
	mr := p.MergeRequest
	if mr.IID < 1 {
		return nil, fmt.Errorf("missing merge request iid")
	}
	base := draft{
		repo: repo, number: mr.IID, head: mr.LastCommit.ID, base: mr.TargetBranch, branch: mr.SourceBranch,
		// A note names the merge request author only by id, and the commenter
		// is not the author, so only the hub's memory can tell the name.
		author: glAuthor(c, repo, mr.IID, 0, glUser{}),
	}
	return commentDrafts(c, base, comment{
		ID: strconv.FormatInt(a.ID, 10), URL: a.URL, Body: a.Note, Login: p.User.Username,
		At: parseTime(a.UpdatedAt, a.CreatedAt), Edited: a.Action == "update",
	}), nil
}

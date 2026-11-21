package normalize

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"git-event-hub/pkg/prevents"
)

// reviewStatusContexts are the commit status contexts that the review bot
// writes. The bot's summary comment carries the same fact in more detail, so
// the statuses are dropped.
var reviewStatusContexts = map[string]bool{"ocr-review": true}

type ghRepo struct {
	FullName string `json:"full_name"`
}

type ghUser struct {
	Login string `json:"login"`
}

type ghPRRef struct {
	Number int `json:"number"`
	Head   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			URL string `json:"url"`
		} `json:"repo"`
	} `json:"base"`
}

// sameRepo reports whether a pull request reference has its base in the
// repository of the payload. GitHub can list pull requests of other
// repositories (for example a fork) that share the head branch; their
// numbers mean nothing in this repository. A reference without a URL is
// kept.
func sameRepo(apiURL, fullName string) bool {
	return apiURL == "" || strings.HasSuffix(strings.ToLower(apiURL), "/repos/"+strings.ToLower(fullName))
}

type ghPullRequest struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	HTMLURL        string  `json:"html_url"`
	Draft          bool    `json:"draft"`
	Merged         bool    `json:"merged"`
	MergedBy       *ghUser `json:"merged_by"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Mergeable      *bool   `json:"mergeable"`
	MergeableState string  `json:"mergeable_state"`
	UpdatedAt      string  `json:"updated_at"`
	ClosedAt       string  `json:"closed_at"`
	User           ghUser  `json:"user"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// GitHub normalizes one webhook delivery. event is the X-GitHub-Event header.
func GitHub(event string, body []byte, c Context) ([]prevents.Event, error) {
	var (
		drafts []draft
		err    error
	)
	switch event {
	case "ping":
		c.drop(DropPing)
	case "pull_request":
		drafts, err = ghPullRequestDrafts(body, c)
	case "check_suite":
		drafts, err = ghCheckSuiteDrafts(body, c)
	case "check_run":
		drafts, err = ghCheckRunDrafts(body, c)
	case "workflow_run":
		drafts, err = ghWorkflowRunDrafts(body, c)
	case "status":
		drafts, err = ghStatusDrafts(body, c)
	case "issue_comment":
		drafts, err = ghIssueCommentDrafts(body, c)
	default:
		c.drop(DropUnsupportedEvent)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: github %s: %v", ErrPayload, event, err)
	}
	return c.finish(prevents.GitHub, drafts), nil
}

func ghPullRequestDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		Action      string        `json:"action"`
		Before      string        `json:"before"`
		After       string        `json:"after"`
		PullRequest ghPullRequest `json:"pull_request"`
		Repository  ghRepo        `json:"repository"`
		Sender      ghUser        `json:"sender"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	pr := p.PullRequest
	if pr.Number < 1 || p.Repository.FullName == "" || pr.Head.SHA == "" {
		return nil, fmt.Errorf("missing number, repository or head")
	}
	base := draft{
		repo: p.Repository.FullName, number: pr.Number, head: pr.Head.SHA,
		base: pr.Base.Ref, branch: pr.Head.Ref, author: pr.User.Login,
		at: parseTime(pr.UpdatedAt),
	}
	var out []draft
	switch p.Action {
	case "opened", "reopened":
		d := base
		d.kind = prevents.KindOpened
		d.data = prevents.OpenedData{Title: pr.Title, URL: pr.HTMLURL, Draft: pr.Draft, Reopened: p.Action == "reopened"}
		out = append(out, d)
	case "synchronize":
		d := base
		d.kind = prevents.KindPushed
		after := firstNonEmpty(p.After, pr.Head.SHA)
		d.head = after
		d.data = prevents.PushedData{Before: nonEmpty(p.Before), After: after, URL: pr.HTMLURL}
		out = append(out, d)
	case "closed":
		d := base
		d.at = parseTime(pr.ClosedAt, pr.UpdatedAt)
		if pr.Merged {
			d.kind = prevents.KindMerged
			d.data = prevents.MergedData{MergeCommitSHA: nonEmpty(pr.MergeCommitSHA), MergedBy: userPtr(pr.MergedBy), URL: pr.HTMLURL}
		} else {
			d.kind = prevents.KindClosed
			d.data = prevents.ClosedData{ClosedBy: nonEmpty(p.Sender.Login), URL: pr.HTMLURL}
		}
		return []draft{d}, nil
	default:
		c.drop(DropUnsupportedAction)
		return nil, nil
	}
	if pr.MergeableState == "dirty" || (pr.Mergeable != nil && !*pr.Mergeable) {
		d := base
		d.kind = prevents.KindConflict
		d.data = prevents.ConflictData{State: firstNonEmpty(pr.MergeableState, "not-mergeable"), URL: pr.HTMLURL}
		out = append(out, d)
	}
	return out, nil
}

func userPtr(u *ghUser) *string {
	if u == nil {
		return nil
	}
	return nonEmpty(u.Login)
}

// ghPullRequests returns the pull requests that a check payload names. When
// it names none, the head commit may still belong to a pull request that the
// hub knows.
func ghPullRequests(c Context, repo, sha string, refs []ghPRRef) []draft {
	var out []draft
	for _, r := range refs {
		if !sameRepo(r.Base.Repo.URL, repo) {
			continue
		}
		d := draft{repo: repo, number: r.Number, head: firstNonEmpty(r.Head.SHA, sha), base: r.Base.Ref, branch: r.Head.Ref}
		if e, ok := c.index().Get(prevents.GitHub, repo, r.Number); ok {
			d.author = e.Author
		}
		out = append(out, d)
	}
	if len(out) == 0 && sha != "" {
		if e, ok := c.index().BySHA(prevents.GitHub, repo, sha); ok {
			out = append(out, draft{repo: repo, number: e.Number, head: sha, base: e.BaseRef, branch: e.Branch, author: e.Author})
		}
	}
	if len(out) == 0 {
		c.drop(DropNoPullRequest)
	}
	return out
}

func ghCheckSuiteDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		Action     string `json:"action"`
		Repository ghRepo `json:"repository"`
		CheckSuite struct {
			ID           int64     `json:"id"`
			Conclusion   string    `json:"conclusion"`
			HeadSHA      string    `json:"head_sha"`
			URL          string    `json:"url"`
			UpdatedAt    string    `json:"updated_at"`
			PullRequests []ghPRRef `json:"pull_requests"`
			App          struct {
				Slug string `json:"slug"`
				Name string `json:"name"`
			} `json:"app"`
		} `json:"check_suite"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	s := p.CheckSuite
	if p.Repository.FullName == "" || s.ID == 0 {
		return nil, fmt.Errorf("missing repository or check suite")
	}
	if p.Action != "completed" || s.Conclusion == "" {
		c.drop(DropNotFinished)
		return nil, nil
	}
	if s.App.Slug == "github-actions" {
		c.drop(DropCoveredByWorkflow)
		return nil, nil
	}
	drafts := ghPullRequests(c, p.Repository.FullName, s.HeadSHA, s.PullRequests)
	for i := range drafts {
		drafts[i].kind = prevents.KindCIFinished
		drafts[i].at = parseTime(s.UpdatedAt)
		drafts[i].data = prevents.CIFinishedData{
			Source: s.App.Slug, Scope: prevents.ScopeSuite, Name: firstNonEmpty(s.App.Name, s.App.Slug),
			Status: ciStatus(s.Conclusion), URL: s.URL, RunID: strconv.FormatInt(s.ID, 10),
			FailedJobs: []prevents.FailedJob{},
		}
	}
	return drafts, nil
}

func ghCheckRunDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		Action     string `json:"action"`
		Repository ghRepo `json:"repository"`
		CheckRun   struct {
			ID           int64     `json:"id"`
			Name         string    `json:"name"`
			Conclusion   string    `json:"conclusion"`
			HTMLURL      string    `json:"html_url"`
			HeadSHA      string    `json:"head_sha"`
			CompletedAt  string    `json:"completed_at"`
			PullRequests []ghPRRef `json:"pull_requests"`
			App          struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_run"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	r := p.CheckRun
	if p.Repository.FullName == "" || r.ID == 0 {
		return nil, fmt.Errorf("missing repository or check run")
	}
	if p.Action != "completed" {
		c.drop(DropNotFinished)
		return nil, nil
	}
	if ciStatus(r.Conclusion) != prevents.CIFailed {
		c.drop(DropCheckRunNotFailed)
		return nil, nil
	}
	drafts := ghPullRequests(c, p.Repository.FullName, r.HeadSHA, r.PullRequests)
	for i := range drafts {
		drafts[i].kind = prevents.KindCIFinished
		drafts[i].at = parseTime(r.CompletedAt)
		drafts[i].data = prevents.CIFinishedData{
			Source: firstNonEmpty(r.App.Slug, "github"), Scope: prevents.ScopeJob, Name: r.Name,
			Status: prevents.CIFailed, URL: r.HTMLURL, RunID: strconv.FormatInt(r.ID, 10),
			FailedJobs: []prevents.FailedJob{{ID: strconv.FormatInt(r.ID, 10), Name: r.Name, URL: r.HTMLURL, Conclusion: r.Conclusion}},
		}
	}
	return drafts, nil
}

func ghWorkflowRunDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		Action      string `json:"action"`
		Repository  ghRepo `json:"repository"`
		WorkflowRun struct {
			ID           int64     `json:"id"`
			Name         string    `json:"name"`
			Conclusion   string    `json:"conclusion"`
			HTMLURL      string    `json:"html_url"`
			HeadSHA      string    `json:"head_sha"`
			UpdatedAt    string    `json:"updated_at"`
			PullRequests []ghPRRef `json:"pull_requests"`
		} `json:"workflow_run"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	r := p.WorkflowRun
	if p.Repository.FullName == "" || r.ID == 0 {
		return nil, fmt.Errorf("missing repository or workflow run")
	}
	if p.Action != "completed" || r.Conclusion == "" {
		c.drop(DropNotFinished)
		return nil, nil
	}
	drafts := ghPullRequests(c, p.Repository.FullName, r.HeadSHA, r.PullRequests)
	for i := range drafts {
		drafts[i].kind = prevents.KindCIFinished
		drafts[i].at = parseTime(r.UpdatedAt)
		drafts[i].data = prevents.CIFinishedData{
			Source: "github-actions", Scope: prevents.ScopeWorkflow, Name: r.Name,
			Status: ciStatus(r.Conclusion), URL: r.HTMLURL, RunID: strconv.FormatInt(r.ID, 10),
			FailedJobs: []prevents.FailedJob{},
		}
	}
	return drafts, nil
}

func ghStatusDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		ID         int64  `json:"id"`
		SHA        string `json:"sha"`
		State      string `json:"state"`
		Context    string `json:"context"`
		TargetURL  string `json:"target_url"`
		UpdatedAt  string `json:"updated_at"`
		Repository ghRepo `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if p.Repository.FullName == "" || p.SHA == "" || p.Context == "" {
		return nil, fmt.Errorf("missing repository, sha or context")
	}
	switch {
	case reviewStatusContexts[p.Context]:
		c.drop(DropReviewStatus)
		return nil, nil
	case p.State == "pending":
		c.drop(DropNotFinished)
		return nil, nil
	}
	status := prevents.CIPassed
	if p.State != "success" {
		status = prevents.CIFailed
	}
	drafts := ghPullRequests(c, p.Repository.FullName, p.SHA, nil)
	id := strconv.FormatInt(p.ID, 10)
	for i := range drafts {
		drafts[i].kind = prevents.KindCIFinished
		drafts[i].at = parseTime(p.UpdatedAt)
		data := prevents.CIFinishedData{
			Source: "status:" + p.Context, Scope: prevents.ScopeStatus, Name: p.Context,
			Status: status, URL: p.TargetURL, RunID: id, FailedJobs: []prevents.FailedJob{},
		}
		if status == prevents.CIFailed {
			data.FailedJobs = []prevents.FailedJob{{ID: id, Name: p.Context, URL: p.TargetURL, Conclusion: p.State}}
		}
		drafts[i].data = data
	}
	return drafts, nil
}

func ghIssueCommentDrafts(body []byte, c Context) ([]draft, error) {
	var p struct {
		Action string `json:"action"`
		Issue  struct {
			Number      int             `json:"number"`
			User        ghUser          `json:"user"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		Comment struct {
			ID        int64  `json:"id"`
			HTMLURL   string `json:"html_url"`
			Body      string `json:"body"`
			User      ghUser `json:"user"`
			UpdatedAt string `json:"updated_at"`
		} `json:"comment"`
		Repository ghRepo `json:"repository"`
		Sender     ghUser `json:"sender"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if p.Repository.FullName == "" || p.Issue.Number < 1 || p.Comment.ID == 0 {
		return nil, fmt.Errorf("missing repository, issue or comment")
	}
	if p.Action != "created" && p.Action != "edited" {
		c.drop(DropUnsupportedAction)
		return nil, nil
	}
	if len(p.Issue.PullRequest) == 0 || string(p.Issue.PullRequest) == "null" {
		c.drop(DropNotPullRequest)
		return nil, nil
	}
	base := draft{repo: p.Repository.FullName, number: p.Issue.Number, author: p.Issue.User.Login}
	if e, ok := c.index().Get(prevents.GitHub, base.repo, base.number); ok {
		base.head, base.base, base.branch = e.HeadSHA, e.BaseRef, e.Branch
	}
	// An edit replaces the text, so the editor is the author of what the
	// comment now says. The account filter then checks the editor.
	login := p.Comment.User.Login
	if p.Action == "edited" && p.Sender.Login != "" && !strings.EqualFold(p.Sender.Login, login) {
		login = p.Sender.Login
	}
	return commentDrafts(c, base, comment{
		ID: strconv.FormatInt(p.Comment.ID, 10), URL: p.Comment.HTMLURL, Body: p.Comment.Body,
		Login: login, At: parseTime(p.Comment.UpdatedAt), Edited: p.Action == "edited",
	}), nil
}

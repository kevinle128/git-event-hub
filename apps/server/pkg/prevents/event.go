// Package prevents is the versioned contract for pull request and merge
// request events. The hub produces these events from forge webhooks, and
// any consumer can validate them against schema/pr-event.v1.json.
//
// Version 1 may gain optional fields. A consumer must ignore fields it does
// not know. A change that removes or renames a field needs a new version.
package prevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Version is the contract version that this package writes.
const Version = 1

// Provider names the forge that sent the event.
type Provider string

const (
	GitHub Provider = "github"
	GitLab Provider = "gitlab"
)

// Providers lists every provider value of version 1.
var Providers = []Provider{GitHub, GitLab}

// Kind names what happened to the pull request.
type Kind string

const (
	KindOpened         Kind = "opened"
	KindPushed         Kind = "pushed"
	KindCIFinished     Kind = "ci.finished"
	KindReviewFinished Kind = "review.finished"
	KindMerged         Kind = "merged"
	KindClosed         Kind = "closed"
	KindConflict       Kind = "conflict"
	KindComment        Kind = "comment"
)

// Kinds lists every kind of version 1.
var Kinds = []Kind{
	KindOpened, KindPushed, KindCIFinished, KindReviewFinished,
	KindMerged, KindClosed, KindConflict, KindComment,
}

// Event is one fact about one pull request (GitHub) or merge request
// (GitLab). Data holds one of the *Data types below; its type depends on
// Kind.
//
// HeadSHA, BaseRef, Branch and Author are null when the forge payload does
// not carry them and the hub has not seen them in an earlier event of
// the same pull request.
type Event struct {
	// V is the contract version. It is always 1.
	V int `json:"v"`
	// ID identifies the event: "<provider>:<forge delivery id>:<index>".
	// A repeated delivery of the same webhook gives the same ID.
	ID       string   `json:"id"`
	Provider Provider `json:"provider"`
	// Repo is "owner/name" on GitHub and the full project path on GitLab.
	Repo string `json:"repo"`
	// Number is the pull request number or the merge request iid.
	Number int  `json:"number"`
	Kind   Kind `json:"kind"`
	// HeadSHA is the head commit of the pull request that the event is about.
	HeadSHA *string `json:"headSha"`
	// BaseRef is the target branch name.
	BaseRef *string `json:"baseRef"`
	// Branch is the source branch name.
	Branch *string `json:"branch"`
	// Author is the login of the person who opened the pull request.
	Author *string `json:"author"`
	// OccurredAt is the time the forge gives for the event, in UTC.
	OccurredAt time.Time `json:"occurredAt"`
	Data       any       `json:"data"`
}

// OpenedData is the data of an "opened" event. It covers a reopened pull
// request too.
type OpenedData struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Draft    bool   `json:"draft"`
	Reopened bool   `json:"reopened"`
}

// PushedData is the data of a "pushed" event: new commits on the source
// branch. Before is null when the forge does not say.
type PushedData struct {
	Before *string `json:"before"`
	After  string  `json:"after"`
	URL    string  `json:"url"`
}

// CI statuses.
const (
	CIPassed    = "passed"
	CIFailed    = "failed"
	CICancelled = "cancelled"
	CISkipped   = "skipped"
)

// CI scopes: how much of the CI one event covers.
const (
	ScopeWorkflow = "workflow" // one GitHub Actions workflow run
	ScopeSuite    = "suite"    // one GitHub check suite of an app other than Actions
	ScopeJob      = "job"      // one failed GitHub check run
	ScopeStatus   = "status"   // one GitHub commit status
	ScopePipeline = "pipeline" // one GitLab pipeline
)

// FailedJob is one failed job, check run or commit status.
type FailedJob struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Stage      string `json:"stage,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

// CIFinishedData is the data of a "ci.finished" event.
type CIFinishedData struct {
	// Source tells who ran the check: "github-actions", "gitlab-ci", the app
	// slug of a check suite, or "status:<context>".
	Source string `json:"source"`
	Scope  string `json:"scope"`
	Name   string `json:"name"`
	Status string `json:"status"`
	URL    string `json:"url"`
	RunID  string `json:"runId,omitempty"`
	// FailedJobs is never null. A GitHub workflow or suite event lists no
	// jobs, because the webhook does not carry them; the consumer reads them
	// from the forge.
	FailedJobs []FailedJob `json:"failedJobs"`
}

// Review statuses.
const (
	ReviewSuccess     = "success"
	ReviewNeedsPerson = "needs-person"
	ReviewFailed      = "failed"
	ReviewSourceOCR   = "ocr"
	ReviewScopeFull   = "full"
	ReviewScopeCommit = "commit"
)

// ReviewItem is one round of the review chain that left findings which the
// bot judged real and did not fix.
type ReviewItem struct {
	Round      int    `json:"round"`
	Scope      string `json:"scope"`
	Real       int    `json:"real"`
	Fixed      int    `json:"fixed"`
	Unresolved int    `json:"unresolved"`
}

// ReviewFinishedData is the data of a "review.finished" event. It comes from
// the machine block of the bot's summary comment, never from its HTML.
type ReviewFinishedData struct {
	Source string `json:"source"`
	Status string `json:"status"`
	// ReviewedSHA is the last commit that the chain reviewed. It may differ
	// from the pull request head; a consumer must compare them.
	ReviewedSHA string       `json:"reviewedSha"`
	Rounds      int          `json:"rounds"`
	MaxRounds   int          `json:"maxRounds"`
	Items       []ReviewItem `json:"items"`
	Commenter   string       `json:"commenter"`
	CommentID   string       `json:"commentId"`
	CommentURL  string       `json:"commentUrl"`
}

// MergedData is the data of a "merged" event.
type MergedData struct {
	MergeCommitSHA *string `json:"mergeCommitSha"`
	MergedBy       *string `json:"mergedBy"`
	URL            string  `json:"url"`
}

// ClosedData is the data of a "closed" event (closed, not merged).
type ClosedData struct {
	ClosedBy *string `json:"closedBy"`
	URL      string  `json:"url"`
}

// ConflictData is the data of a "conflict" event: the forge says the pull
// request cannot be merged because of a conflict.
type ConflictData struct {
	// State is the raw forge value, for example "dirty" or "conflict".
	State string `json:"state"`
	URL   string `json:"url"`
}

// CommentData is the data of a "comment" event: a comment of a bot account
// that is not a review summary.
type CommentData struct {
	CommentID string `json:"commentId"`
	URL       string `json:"url"`
	Commenter string `json:"commenter"`
	Edited    bool   `json:"edited"`
	// Body is cut to MaxCommentBytes.
	Body          string `json:"body"`
	BodyTruncated bool   `json:"bodyTruncated"`
	// MarkerError is set when the body carries a review machine block that
	// cannot be read.
	MarkerError string `json:"markerError,omitempty"`
}

// MaxCommentBytes is the longest comment body that an event carries.
const MaxCommentBytes = 32 * 1024

// Batch is a group of events that a sink sends in one request.
type Batch struct {
	V      int     `json:"v"`
	Events []Event `json:"events"`
}

// UnmarshalJSON reads the data member as the type that Kind selects.
func (e *Event) UnmarshalJSON(b []byte) error {
	type plain Event
	aux := struct {
		*plain
		Data json.RawMessage `json:"data"`
	}{plain: (*plain)(e)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	var target any
	switch e.Kind {
	case KindOpened:
		target = &OpenedData{}
	case KindPushed:
		target = &PushedData{}
	case KindCIFinished:
		target = &CIFinishedData{}
	case KindReviewFinished:
		target = &ReviewFinishedData{}
	case KindMerged:
		target = &MergedData{}
	case KindClosed:
		target = &ClosedData{}
	case KindConflict:
		target = &ConflictData{}
	case KindComment:
		target = &CommentData{}
	default:
		return fmt.Errorf("unknown event kind %q", e.Kind)
	}
	if len(aux.Data) == 0 || string(aux.Data) == "null" {
		return errors.New("event has no data")
	}
	if err := json.Unmarshal(aux.Data, target); err != nil {
		return fmt.Errorf("data of kind %s: %w", e.Kind, err)
	}
	e.Data = derefData(target)
	return nil
}

func derefData(p any) any {
	switch v := p.(type) {
	case *OpenedData:
		return *v
	case *PushedData:
		return *v
	case *CIFinishedData:
		return *v
	case *ReviewFinishedData:
		return *v
	case *MergedData:
		return *v
	case *ClosedData:
		return *v
	case *ConflictData:
		return *v
	case *CommentData:
		return *v
	}
	return p
}

// Validate checks that the event follows version 1.
func (e Event) Validate() error {
	switch {
	case e.V != Version:
		return fmt.Errorf("v is %d, want %d", e.V, Version)
	case e.ID == "":
		return errors.New("id is empty")
	case !validProvider(e.Provider):
		return fmt.Errorf("provider %q is unknown", e.Provider)
	case e.Repo == "":
		return errors.New("repo is empty")
	case e.Number < 1:
		return errors.New("number is below 1")
	case e.OccurredAt.IsZero():
		return errors.New("occurredAt is empty")
	}
	ok := false
	switch e.Data.(type) {
	case OpenedData:
		ok = e.Kind == KindOpened
	case PushedData:
		ok = e.Kind == KindPushed
	case CIFinishedData:
		ok = e.Kind == KindCIFinished
	case ReviewFinishedData:
		ok = e.Kind == KindReviewFinished
	case MergedData:
		ok = e.Kind == KindMerged
	case ClosedData:
		ok = e.Kind == KindClosed
	case ConflictData:
		ok = e.Kind == KindConflict
	case CommentData:
		ok = e.Kind == KindComment
	}
	if !ok {
		return fmt.Errorf("data type %T does not fit kind %q", e.Data, e.Kind)
	}
	return nil
}

func validProvider(p Provider) bool {
	for _, v := range Providers {
		if v == p {
			return true
		}
	}
	return false
}

// ValidKind reports whether k is a kind of version 1.
func ValidKind(k Kind) bool {
	for _, v := range Kinds {
		if v == k {
			return true
		}
	}
	return false
}

// ValidProvider reports whether p is a provider of version 1.
func ValidProvider(p Provider) bool { return validProvider(p) }

// Commenter returns the login of the comment author for the kinds that come
// from a comment, and "" for the others.
func (e Event) Commenter() string {
	switch d := e.Data.(type) {
	case CommentData:
		return d.Commenter
	case ReviewFinishedData:
		return d.Commenter
	}
	return ""
}

// Package normalize turns raw forge webhook payloads into contract events. It
// is the anti-corruption layer: forge field names and shapes stop here, and
// only prevents.Event leaves.
package normalize

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"git-event-hub/pkg/prevents"
)

// Reasons that a normalizer gives when it drops a payload on purpose. They
// are metric labels, so the set stays small.
const (
	DropUnsupportedEvent  = "unsupported_event"
	DropUnsupportedAction = "unsupported_action"
	DropNoPullRequest     = "no_pull_request"
	DropNotFinished       = "not_finished"
	DropCoveredByWorkflow = "covered_by_workflow_run"
	DropCheckRunNotFailed = "check_run_not_failed"
	DropReviewStatus      = "review_status_context"
	DropReviewRunning     = "review_running"
	DropNotPullRequest    = "not_a_pull_request"
	DropSystemNote        = "system_note"
	DropNoChange          = "no_head_change"
	DropPing              = "ping"
)

// ErrPayload means that the payload does not have the shape that the forge
// documents. The webhook handler answers 400 so the problem shows up in the
// forge's delivery log.
var ErrPayload = errors.New("payload does not match the forge format")

// Entry is what the hub remembers about a pull request from earlier
// events. It fills the fields that a later payload lacks, for example the
// author of the pull request in a comment or pipeline payload.
type Entry struct {
	Number  int
	Author  string
	Branch  string
	BaseRef string
	HeadSHA string
}

// Index looks up pull requests that the hub has seen. A lookup that finds
// nothing returns false.
type Index interface {
	Get(provider prevents.Provider, repo string, number int) (Entry, bool)
	ByBranch(provider prevents.Provider, repo, branch string) (Entry, bool)
	BySHA(provider prevents.Provider, repo, sha string) (Entry, bool)
}

// NoIndex is an Index that knows nothing.
type NoIndex struct{}

func (NoIndex) Get(prevents.Provider, string, int) (Entry, bool)         { return Entry{}, false }
func (NoIndex) ByBranch(prevents.Provider, string, string) (Entry, bool) { return Entry{}, false }
func (NoIndex) BySHA(prevents.Provider, string, string) (Entry, bool)    { return Entry{}, false }

// Context carries what a normalizer needs besides the payload.
type Context struct {
	// DeliveryID is the forge delivery id; it makes the event ids.
	DeliveryID string
	// ReceivedAt is the fallback for events that carry no time.
	ReceivedAt time.Time
	Index      Index
	// Drop records why a payload gave no event. It may be nil.
	Drop func(reason string)
}

func (c Context) drop(reason string) {
	if c.Drop != nil {
		c.Drop(reason)
	}
}

func (c Context) index() Index {
	if c.Index == nil {
		return NoIndex{}
	}
	return c.Index
}

// draft is an event before it has its id.
type draft struct {
	repo   string
	number int
	kind   prevents.Kind
	head   string
	base   string
	branch string
	author string
	at     time.Time
	data   any
}

func (c Context) finish(p prevents.Provider, drafts []draft) []prevents.Event {
	if len(drafts) == 0 {
		return nil
	}
	out := make([]prevents.Event, 0, len(drafts))
	for i, d := range drafts {
		at := d.at
		if at.IsZero() {
			at = c.ReceivedAt
		}
		out = append(out, prevents.Event{
			V:          prevents.Version,
			ID:         fmt.Sprintf("%s:%s:%d", p, c.DeliveryID, i),
			Provider:   p,
			Repo:       d.repo,
			Number:     d.number,
			Kind:       d.kind,
			HeadSHA:    nonEmpty(d.head),
			BaseRef:    nonEmpty(d.base),
			Branch:     nonEmpty(d.branch),
			Author:     nonEmpty(d.author),
			OccurredAt: at.UTC(),
			Data:       d.data,
		})
	}
	return out
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var timeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"}

// parseTime reads the first value that parses, or returns the zero time.
func parseTime(values ...string) time.Time {
	for _, v := range values {
		if v == "" {
			continue
		}
		for _, layout := range timeLayouts {
			if t, err := time.Parse(layout, v); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// comment is a pull request comment in forge-neutral form.
type comment struct {
	ID     string
	URL    string
	Body   string
	Login  string
	At     time.Time
	Edited bool
}

// commentDrafts makes the draft for a comment: a review.finished event when
// the body carries a finished review block, a comment event otherwise. A
// block that still says "running" gives nothing.
func commentDrafts(c Context, base draft, cm comment) []draft {
	base.at = cm.At
	marker, err := prevents.ParseOCRMarker(cm.Body)
	if marker != nil {
		status := marker.ReviewStatus()
		if status == "" {
			c.drop(DropReviewRunning)
			return nil
		}
		base.kind = prevents.KindReviewFinished
		base.data = prevents.ReviewFinishedData{
			Source:      prevents.ReviewSourceOCR,
			Status:      status,
			ReviewedSHA: marker.ReviewedSHA(),
			Rounds:      len(marker.Rounds),
			MaxRounds:   marker.Max,
			Items:       marker.Items(),
			Commenter:   cm.Login,
			CommentID:   cm.ID,
			CommentURL:  cm.URL,
		}
		return []draft{base}
	}
	body, truncated := truncate(cm.Body, prevents.MaxCommentBytes)
	data := prevents.CommentData{
		CommentID: cm.ID, URL: cm.URL, Commenter: cm.Login, Edited: cm.Edited,
		Body: body, BodyTruncated: truncated,
	}
	if err != nil {
		data.MarkerError = err.Error()
	}
	base.kind = prevents.KindComment
	base.data = data
	return []draft{base}
}

// truncate cuts s to at most max bytes at a character boundary.
func truncate(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func ciStatus(conclusion string) string {
	switch strings.ToLower(conclusion) {
	case "success", "passed", "neutral":
		return prevents.CIPassed
	case "cancelled", "canceled", "stale":
		return prevents.CICancelled
	case "skipped":
		return prevents.CISkipped
	}
	return prevents.CIFailed
}

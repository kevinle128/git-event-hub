package omb

import (
	"fmt"

	"git-event-hub/pkg/prevents"
)

// Connector values: the OMB app whose intake a sink serves.
const (
	connectorGitHub = "github-event"
	connectorGitLab = "gitlab-event"
)

// forgeOf says which forge each connector takes. OMB refuses an event of
// another forge on a connector for good.
var forgeOf = map[string]prevents.Provider{
	connectorGitHub: prevents.GitHub,
	connectorGitLab: prevents.GitLab,
}

// typeOf maps the kind of a pr-event v1 event to the type of OMB event
// contract v1.
var typeOf = map[prevents.Kind]string{
	prevents.KindOpened:         "pr.opened",
	prevents.KindPushed:         "pr.pushed",
	prevents.KindCIFinished:     "pr.ci.finished",
	prevents.KindReviewFinished: "pr.review.finished",
	prevents.KindComment:        "pr.comment.created",
	prevents.KindConflict:       "pr.conflict.detected",
	prevents.KindMerged:         "pr.merged",
	prevents.KindClosed:         "pr.closed",
}

// toOMB maps one pr-event v1 event to OMB event contract v1. It fails when
// the event belongs to another forge than the connector, when its kind has
// no type, or when its data does not fit its kind.
//
// data.url is the pull request URL in OMB, and OMB writes it over the PR
// record. Only opened, pushed, conflict, merged and closed carry that URL in
// pr-event v1. For ci.finished, review.finished and comment the URL of the
// event is the check, the review comment or the comment, so data.url stays
// null and the URL goes to check.url, review.commentUrl or comment.url.
func toOMB(e prevents.Event, connector string) (ombEvent, error) {
	forge, ok := forgeOf[connector]
	if !ok {
		return ombEvent{}, fmt.Errorf("unknown connector %q", connector)
	}
	if e.Provider != forge {
		return ombEvent{}, fmt.Errorf("provider %s does not belong to connector %s", e.Provider, connector)
	}
	typ, ok := typeOf[e.Kind]
	if !ok {
		return ombEvent{}, fmt.Errorf("event kind %q has no OMB type", e.Kind)
	}
	common := prCommon{HeadSHA: e.HeadSHA, BaseRef: e.BaseRef, Branch: e.Branch, Author: e.Author}
	var data any
	var prURL string
	switch d := e.Data.(type) {
	case prevents.OpenedData:
		if e.Kind != prevents.KindOpened {
			return ombEvent{}, mismatch(e)
		}
		prURL = d.URL
		common.URL = nonEmpty(d.URL)
		data = openedData{prCommon: common, Title: d.Title, Draft: d.Draft, Reopened: d.Reopened}
	case prevents.PushedData:
		if e.Kind != prevents.KindPushed {
			return ombEvent{}, mismatch(e)
		}
		prURL = d.URL
		common.URL = nonEmpty(d.URL)
		data = pushedData{prCommon: common, Before: d.Before, After: d.After}
	case prevents.CIFinishedData:
		if e.Kind != prevents.KindCIFinished {
			return ombEvent{}, mismatch(e)
		}
		jobs := d.FailedJobs
		if jobs == nil {
			jobs = []prevents.FailedJob{}
		}
		data = ciFinishedData{prCommon: common, Check: ciCheck{
			Source: d.Source, Scope: d.Scope, Name: d.Name, Status: d.Status,
			URL: d.URL, RunID: d.RunID, FailedJobs: jobs,
		}}
	case prevents.ReviewFinishedData:
		if e.Kind != prevents.KindReviewFinished {
			return ombEvent{}, mismatch(e)
		}
		items := d.Items
		if items == nil {
			items = []prevents.ReviewItem{}
		}
		data = reviewFinishedData{prCommon: common, Review: reviewSummary{
			Source: d.Source, Status: d.Status, ReviewedSHA: d.ReviewedSHA,
			Rounds: d.Rounds, MaxRounds: d.MaxRounds, Items: items,
			Commenter: d.Commenter, CommentID: d.CommentID, CommentURL: d.CommentURL,
		}}
	case prevents.CommentData:
		if e.Kind != prevents.KindComment {
			return ombEvent{}, mismatch(e)
		}
		data = commentCreatedData{prCommon: common, Comment: commentPayload{
			ID: d.CommentID, URL: d.URL, Commenter: d.Commenter, Edited: d.Edited,
			Body: d.Body, BodyTruncated: d.BodyTruncated, MarkerError: d.MarkerError,
		}}
	case prevents.ConflictData:
		if e.Kind != prevents.KindConflict {
			return ombEvent{}, mismatch(e)
		}
		prURL = d.URL
		common.URL = nonEmpty(d.URL)
		data = conflictDetectedData{prCommon: common, Conflict: conflictInfo{State: d.State}}
	case prevents.MergedData:
		if e.Kind != prevents.KindMerged {
			return ombEvent{}, mismatch(e)
		}
		prURL = d.URL
		common.URL = nonEmpty(d.URL)
		data = mergedData{prCommon: common, Merge: mergeInfo{CommitSHA: d.MergeCommitSHA, MergedBy: d.MergedBy}}
	case prevents.ClosedData:
		if e.Kind != prevents.KindClosed {
			return ombEvent{}, mismatch(e)
		}
		prURL = d.URL
		common.URL = nonEmpty(d.URL)
		data = closedData{prCommon: common, ClosedBy: d.ClosedBy}
	default:
		return ombEvent{}, mismatch(e)
	}
	out := ombEvent{
		V:          1,
		ID:         e.ID,
		Source:     ombSource{Connector: connector},
		Type:       typ,
		OccurredAt: e.OccurredAt.UTC(),
		Subject:    prSubject{Kind: "pr", Forge: string(forge), Repo: e.Repo, Number: e.Number},
		Data:       data,
	}
	corr := map[string]string{}
	if prURL != "" {
		corr["prUrl"] = prURL
	}
	if e.HeadSHA != nil && *e.HeadSHA != "" {
		corr["headSha"] = *e.HeadSHA
	}
	if len(corr) > 0 {
		out.Correlation = corr
	}
	return out, nil
}

// nonEmpty returns nil for an empty URL, so the field is written as null.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mismatch(e prevents.Event) error {
	return fmt.Errorf("data type %T does not fit kind %q", e.Data, e.Kind)
}

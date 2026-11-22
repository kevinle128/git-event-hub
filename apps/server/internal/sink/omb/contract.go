package omb

import (
	"time"

	"git-event-hub/pkg/prevents"
)

// These types are the part of OMB event contract v1 that the hub writes: the
// envelope and the eight pr.* payloads. OMB owns the contract. A copy of its
// schema in testdata/omb-contract checks these types in the tests.

type ombEvent struct {
	V           int               `json:"v"`
	ID          string            `json:"id"`
	Source      ombSource         `json:"source"`
	Type        string            `json:"type"`
	OccurredAt  time.Time         `json:"occurredAt"`
	Subject     prSubject         `json:"subject"`
	Correlation map[string]string `json:"correlation,omitempty"`
	Data        any               `json:"data"`
}

type ombSource struct {
	Connector string `json:"connector"`
}

type prSubject struct {
	Kind   string `json:"kind"` // always "pr"
	Forge  string `json:"forge"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// prCommon holds the fields that every pr.* payload shares. A pointer that
// is nil is written as null.
type prCommon struct {
	HeadSHA *string `json:"headSha"`
	BaseRef *string `json:"baseRef"`
	Branch  *string `json:"branch"`
	Author  *string `json:"author"`
	URL     *string `json:"url"`
}

type openedData struct {
	prCommon
	Title    string `json:"title"`
	Draft    bool   `json:"draft"`
	Reopened bool   `json:"reopened"`
}

type pushedData struct {
	prCommon
	Before *string `json:"before"`
	After  string  `json:"after"`
}

type ciFinishedData struct {
	prCommon
	Check ciCheck `json:"check"`
}

type ciCheck struct {
	Source     string               `json:"source"`
	Scope      string               `json:"scope"`
	Name       string               `json:"name"`
	Status     string               `json:"status"`
	URL        string               `json:"url"`
	RunID      string               `json:"runId,omitempty"`
	FailedJobs []prevents.FailedJob `json:"failedJobs"`
}

type reviewFinishedData struct {
	prCommon
	Review reviewSummary `json:"review"`
}

type reviewSummary struct {
	Source      string                `json:"source"`
	Status      string                `json:"status"`
	ReviewedSHA string                `json:"reviewedSha"`
	Rounds      int                   `json:"rounds"`
	MaxRounds   int                   `json:"maxRounds"`
	Items       []prevents.ReviewItem `json:"items"`
	Commenter   string                `json:"commenter"`
	CommentID   string                `json:"commentId"`
	CommentURL  string                `json:"commentUrl"`
}

type commentCreatedData struct {
	prCommon
	Comment commentPayload `json:"comment"`
}

type commentPayload struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Commenter     string `json:"commenter"`
	Edited        bool   `json:"edited"`
	Body          string `json:"body"`
	BodyTruncated bool   `json:"bodyTruncated"`
	MarkerError   string `json:"markerError,omitempty"`
}

type conflictDetectedData struct {
	prCommon
	Conflict conflictInfo `json:"conflict"`
}

type conflictInfo struct {
	State string `json:"state"`
}

type mergedData struct {
	prCommon
	Merge mergeInfo `json:"merge"`
}

type mergeInfo struct {
	CommitSHA *string `json:"commitSha"`
	MergedBy  *string `json:"mergedBy"`
}

type closedData struct {
	prCommon
	ClosedBy *string `json:"closedBy"`
}

type ombBatch struct {
	V      int        `json:"v"`
	Events []ombEvent `json:"events"`
}

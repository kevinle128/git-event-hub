// TypeScript types for schema/pr-event.v1.json. Keep both files in step: a Go
// test checks that every kind and provider appears in both.
// A consumer must ignore fields it does not know.

export type Provider = "github" | "gitlab";

export type EventKind =
  | "opened"
  | "pushed"
  | "ci.finished"
  | "review.finished"
  | "merged"
  | "closed"
  | "conflict"
  | "comment";

export interface EventBase<K extends EventKind, D> {
  v: 1;
  /** "<provider>:<forge delivery id>:<index>". A repeated delivery gives the same id. */
  id: string;
  provider: Provider;
  /** "owner/name" on GitHub, the full project path on GitLab. */
  repo: string;
  /** Pull request number or merge request iid. */
  number: number;
  kind: K;
  headSha: string | null;
  baseRef: string | null;
  branch: string | null;
  author: string | null;
  /** RFC 3339 time that the forge gives for the event. */
  occurredAt: string;
  data: D;
}

export interface OpenedData { title: string; url: string; draft: boolean; reopened: boolean }
export interface PushedData { before: string | null; after: string; url: string }
export interface FailedJob { id: string; name: string; url: string; stage?: string; conclusion?: string }
export interface CIFinishedData {
  source: string;
  scope: "workflow" | "suite" | "job" | "status" | "pipeline";
  name: string;
  status: "passed" | "failed" | "cancelled" | "skipped";
  url: string;
  runId?: string;
  failedJobs: FailedJob[];
}
export interface ReviewItem { round: number; scope: "full" | "commit"; real: number; fixed: number; unresolved: number }
export interface ReviewFinishedData {
  source: "ocr";
  status: "success" | "needs-person" | "failed";
  reviewedSha: string;
  rounds: number;
  maxRounds: number;
  items: ReviewItem[];
  commenter: string;
  commentId: string;
  commentUrl: string;
}
export interface MergedData { mergeCommitSha: string | null; mergedBy: string | null; url: string }
export interface ClosedData { closedBy: string | null; url: string }
export interface ConflictData { state: string; url: string }
export interface CommentData {
  commentId: string;
  url: string;
  commenter: string;
  edited: boolean;
  body: string;
  bodyTruncated: boolean;
  markerError?: string;
}

export type PrEvent =
  | EventBase<"opened", OpenedData>
  | EventBase<"pushed", PushedData>
  | EventBase<"ci.finished", CIFinishedData>
  | EventBase<"review.finished", ReviewFinishedData>
  | EventBase<"merged", MergedData>
  | EventBase<"closed", ClosedData>
  | EventBase<"conflict", ConflictData>
  | EventBase<"comment", CommentData>;

/** The body that a batch sink sends. */
export interface Batch { v: 1; events: PrEvent[] }

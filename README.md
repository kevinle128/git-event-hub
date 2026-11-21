# git-event-hub

An event gateway for pull requests and merge requests. It receives webhooks from GitHub (a GitHub App) and GitLab, checks and filters them, turns them into one versioned event format, and delivers them to one or more consumers. OpenMausBot (OMB) is the first consumer; the gateway does not depend on it.

```
GitHub App ──┐                                         ┌─► OMB sink
             ├─► sources ─► normalize ─► filter ─► outbox ─┤
GitLab hook ─┘   (verify)   (contract)  (scope)   (SQLite) └─► webhook sink (any HTTP endpoint)
```

## What it does

- **Sources** (`WebhookSource`): verify the request signature and parse the payload. GitHub App (`X-Hub-Signature-256`) and GitLab (`X-Gitlab-Token`) now; a new forge is one adapter.
- **Normalize**: each source maps its payloads to the event contract, so raw forge payloads never leave the gateway.
- **Filter**: config decides what passes: providers, repo globs, event kinds, and for GitLab only merge requests authored by the configured people.
- **Outbox**: every accepted event is stored in SQLite before the forge gets its answer, then delivered with retries; a repeated delivery is dropped by its id.
- **Sinks** (`EventSink`): deliver events to consumers, each with its own filter, cursor and retries, so a slow or down consumer blocks no other. OMB and a generic webhook now; a new consumer is one adapter.

The gateway holds only webhook secrets and sink secrets. It never calls the GitHub or GitLab API, so the internet-facing service cannot merge or change anything.

## Event contract

Every event has the same shape, versioned by `v`:

```json
{
  "v": 1,
  "id": "github:delivery-id",
  "provider": "github",
  "repo": "owner/name",
  "number": 40,
  "kind": "ci.finished",
  "headSha": "…",
  "baseRef": "main",
  "branch": "fix/…",
  "author": "login",
  "occurredAt": "2026-11-21T16:00:00Z",
  "data": {}
}
```

Kinds: `opened`, `pushed`, `ci.finished`, `review.finished`, `comment`, `conflict`, `merged`, `closed`. The JSON Schema (`schema/pr-event.v1.json`) lets consumers in other languages validate events.

## Status

Under construction.

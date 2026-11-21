# git-event-hub

One team-wide hub for pull request events.

The hub receives GitHub App webhooks and GitLab group webhooks. It verifies each webhook and turns it into an event of the **pr-event v1** contract. It sends each event to the person who opened the pull request (PR) or merge request (MR). Later events of the same PR follow the same person, because the hub remembers the PR. A CI event that names only a branch or a commit finds only an open PR, and finds none when two open PRs match, because a guess would send the event to the wrong person.

The hub stores each webhook in Postgres in one transaction. The transaction writes the receipt, the events and one outbox row for each sink. A dispatcher for each sink then delivers the events and retries them.

- The hub holds only webhook secrets and sink secrets.
- The hub never calls the GitHub API or the GitLab API.
- [OpenMausBot](https://github.com/kevinle128/OpenMausBot) (OMB) is one sink. Any HTTP endpoint is another sink.

## Contents

1. [Architecture](#architecture)
2. [Event contract](#event-contract)
3. [Routing and isolation](#routing-and-isolation)
4. [Configuration](#configuration)
5. [Delivery](#delivery)
6. [HTTP API](#http-api)
7. [Add a source or a sink](#add-a-source-or-a-sink)
8. [Run locally and test](#run-locally-and-test)
9. [Deploy](#deploy)
10. [Human setup](#human-setup)

## Architecture

```text
 GitHub App webhook ──┐                                         ┌─► sink  kevin/omb      ─► OMB
                      ├─► POST /hooks/{source}                  │
 GitLab group hook  ──┘        │                                ├─► sink  lan/hermes     ─► any HTTP endpoint
                               ▼                                │
                   ┌──────────────────────┐   ┌───────────┐   ┌─┴────────────────────────────┐
                   │ source: verify +     │──►│ normalize │──►│ route: owner, filters        │
                   │ delivery id          │   │ (pr-event)│   └─┬────────────────────────────┘
                   └──────────────────────┘   └─────▲─────┘     │ targets (subscriber/sink)
                                                    │           ▼
                                          PR memory │   ┌────────────────────────────────────┐
                                          (pr_index)└───│ store: ONE transaction              │
                                                        │  receipt + PR memory + events +     │
                                                        │  one outbox row per target sink     │
                                                        └──────────────┬─────────────────────┘
                                                                       ▼
                                                        dispatcher (one per sink) ─► sink.Deliver
                                                        claim → deliver → settle + delivery log
```

The hub answers the forge after the transaction commits. A crash or a down consumer loses no event.

| Package (`apps/server/internal/…`) | Role |
|---|---|
| `source`, `source/githubapp`, `source/gitlab` | Inbound port and adapters. They check the signature or token, read the delivery id and parse the payload. |
| `normalize` | Anti-corruption layer. It turns forge payloads into `prevents.Event`. It reads the PR memory to find the author of check, pipeline and comment events. |
| `route` | Decides which sinks receive an event, or why none does. |
| `store` | Postgres state through GORM: receipts, events, outbox, delivery log, PR memory. It also serves the read queries. |
| `dispatch` | One dispatcher for each sink: claim, deliver, settle, backoff, expiry. |
| `sink`, `sink/omb`, `sink/webhook` | Outbound port and adapters. |
| `hub` | The core. `Receive` handles one webhook. It also reloads the configuration and runs the dispatchers and the pruning loop. It knows no HTTP framework. |
| `httpapi` | Echo routes generated from `api/openapi.yaml`, the bearer check and the request log. |
| `config` | YAML configuration (Viper, validator) and the reloading store. |
| `app` | fx wiring: config, database, store, hub, HTTP server. The blank imports of the adapters are here. |
| `database`, `metrics`, `glob` | Postgres and migrations, Prometheus counters, glob matching. |

Other directories of `apps/server`:

- `pkg/prevents`: the public event contract (Go types and the review-block parser).
- `schema`: the JSON Schema and the TypeScript types of the contract.
- `migrations/{postgres,sqlite}`: embedded golang-migrate migrations.
- `testdata`: GitHub and GitLab webhook fixtures.
- `config/example.yaml`: the example configuration.

`apps/web` holds only the generated React shell. It has no hub screen yet.

## Event contract

Every event has the same envelope. The `data` member depends on `kind`.

| Field | Meaning |
|---|---|
| `v` | Contract version. Always `1`. |
| `id` | `<provider>:<forge delivery id>:<index>`. A repeated delivery of the same webhook gives the same id. |
| `provider` | `github` or `gitlab`. |
| `repo` | `owner/name` on GitHub. The full project path on GitLab. |
| `number` | PR number or MR iid. |
| `kind` | See the table below. |
| `headSha`, `baseRef`, `branch`, `author` | Strings, or `null` when the payload does not carry them and the PR memory does not know them. `author` is the login of the person who opened the PR or MR. |
| `occurredAt` | Time that the forge gives for the event, in UTC. |
| `data` | Kind-specific object. |

| Kind | When | Main `data` fields |
|---|---|---|
| `opened` | PR or MR opened or reopened | `title`, `url`, `draft`, `reopened` |
| `pushed` | New commits on the source branch | `before`, `after`, `url` |
| `ci.finished` | A CI run ended | `source`, `scope` (`workflow`, `suite`, `job`, `status`, `pipeline`), `name`, `status` (`passed`, `failed`, `cancelled`, `skipped`), `url`, `runId`, `failedJobs` |
| `review.finished` | The review bot ended its chain | `source` (`ocr`), `status` (`success`, `needs-person`, `failed`), `reviewedSha`, `rounds`, `maxRounds`, `items`, `commenter`, `commentId`, `commentUrl` |
| `comment` | A comment of a bot account that is not a review summary | `commentId`, `url`, `commenter`, `edited`, `body` (cut to 32 KiB), `bodyTruncated`, `markerError` |
| `conflict` | The forge says the PR cannot be merged | `state`, `url` |
| `merged` | PR or MR merged | `mergeCommitSha`, `mergedBy`, `url` |
| `closed` | PR or MR closed without a merge | `closedBy`, `url` |

Details:

- `ci.finished.failedJobs` is never `null`. GitHub workflow and suite events list no jobs, because the webhook does not carry them. The consumer reads them from the forge.
- GitHub `check_run` events become events only when the run failed. A finished `check_suite` of GitHub Actions is dropped, because the `workflow_run` event covers it.
- The commit status context `ocr-review` is dropped. The summary comment of the review bot carries the same fact.
- The hub makes a `conflict` event in addition to `opened` or `pushed` when the forge reports a conflict.

### Review machine block

The review bot (OCR) writes this block into its summary comment:

```text
<!-- ocr-serve:v2 BASE64JSON -->
```

The hub reads only this block. It never reads the HTML around it.

- A finished chain becomes a `review.finished` event. A chain that is still `running` gives no event (drop reason `review_running`).
- `status` is `failed` when the chain failed. It is `needs-person` when a round has more real findings than fixed findings. Otherwise it is `success`. `items` lists the rounds that leave unresolved findings.
- A comment with more than one block, or a block that cannot be read, becomes a `comment` event with `markerError` set.
- Only comment authors that match `defaults.commentAuthors` count, and a review block counts only from an account that matches `defaults.reviewAuthors` (see [Routing](#routing-and-isolation)). A person cannot fake a review block.
- An edited GitHub comment counts as written by the account that edited it (`sender`), because an edit replaces the text. The edit of a person is dropped, even when the comment was first written by the review bot.

### Schema and versioning

- JSON Schema: `apps/server/schema/pr-event.v1.json`.
- TypeScript types: `apps/server/schema/pr-event.v1.d.ts`.
- Go types: `apps/server/pkg/prevents`.
- A Go test checks that the schema and the types list the same kinds and providers.

Version 1 may gain optional fields. A consumer must ignore the fields that it does not know. A change that removes or renames a field needs a new version.

## Routing and isolation

`route.Router` applies these rules in this order.

1. **Kind scope.** If `defaults.kinds` is not empty and the kind is not in it, the event is dropped (`kind_out_of_scope`).
2. **Comment authors.** A `comment` or `review.finished` event passes only when its commenter matches a glob of `defaults.commentAuthors`. Default: `*[bot]`. A person cannot register a login with `[`, but a login that ends in `-bot` is free to take, so the default does not include it. A GitLab bot account has no `[bot]` suffix and must be listed by its exact name. A pattern made only of `*` is refused. A comment without an author never counts. Otherwise the event is dropped (`comment_author_out_of_scope`). A `review.finished` event must also match `defaults.reviewAuthors`; there is no default, so with an empty list every `review.finished` event is dropped (`review_author_out_of_scope`).
3. **Owner.** The event `author` is compared with the `identities` of every enabled subscriber for the provider. The comparison ignores case. The subscriber that matches owns the event.
4. **Subscriptions (GitHub only).** A subscriber also receives a GitHub event when the repository matches a glob in one of its `subscriptions`, and the kind is in that subscription's `kinds` (or the list is empty). GitLab MRs go only to their author. This is a decision of the person.
5. **No subscriber found.** The event is dropped. The reason is `no_owner` when the event has no author. It is `no_subscriber` when the author is unknown to every subscriber.
6. **Filters.** For each selected subscriber, the subscriber `filter` must match. Then each enabled sink `filter` must match. A filter has `providers`, `repos.include`, `repos.exclude` and `kinds`. An empty field matches everything. If no sink is left, the event is dropped (`filtered`).

The result is a list of targets (`subscriber/sink`). The hub stores one outbox row for each target.

Isolation:

- A subscriber receives only the events that it owns, or that its `subscriptions` select. No other subscriber receives them.
- `GET /api/events?subscriber=<id>` returns only the sinks of that subscriber in `targets`.
- Each sink has its own queue and its own backoff. A down sink delays no other sink.
- Logs and counters carry the subscriber and sink names.

Glob rules (`repos`, `commentAuthors`, `reviewAuthors`): `*` matches any run of characters except `/`. `**` matches any run, `/` included. Matching ignores case. `[` and `]` stand for themselves, so `*[bot]` matches a login that ends with the text `[bot]`.

### Drop reasons

The hub counts each drop in `hub_dropped_total{source,reason}`. It still answers `202`.

| Reason | Meaning |
|---|---|
| `no_owner` | The event has no author. |
| `no_subscriber` | The author is not an identity of any enabled subscriber. |
| `filtered` | A subscriber filter or every sink filter rejected the event. |
| `kind_out_of_scope` | `defaults.kinds` excludes the kind. |
| `comment_author_out_of_scope` | The commenter does not match `defaults.commentAuthors`, or the comment has no author. |
| `review_author_out_of_scope` | A review block whose commenter does not match `defaults.reviewAuthors`, or has no author. |
| `repo_out_of_source_scope` | The event is of a repository that the `repos` globs of the source do not cover. The hub does not route it and does not remember its pull request. |
| `unsupported_event`, `unsupported_action` | The webhook type or action is not used. |
| `no_pull_request` | A check or status event names no known PR. A pull request listed with a base in another repository does not count. |
| `not_finished` | A check, workflow, pipeline or status is still running. |
| `covered_by_workflow_run` | A GitHub Actions check suite. The workflow event covers it. |
| `check_run_not_failed` | A GitHub check run that did not fail. |
| `review_status_context` | The `ocr-review` commit status. |
| `review_running` | A review block with status `running`. |
| `not_a_pull_request` | A comment on an issue, not on a PR or MR. |
| `system_note` | A GitLab system note. |
| `no_head_change` | A GitLab MR update without a new head commit. |
| `ping` | A GitHub `ping` event. |
| `no_events` | The payload gave no event and no reason. |
| `source_disabled` | The source has `enabled: false`. |

### Known limits

- **Events of a PR that the hub has never seen.** The hub learns the author of a PR from `opened`, `pushed`, `merged`, `closed` and `conflict` events. A GitHub check, workflow or status event, and every GitLab pipeline or note event, take the author from the PR memory only. If the hub never saw the PR (for example the PR was open before the hub started), these events have no author and are dropped as `no_owner`. A GitHub `issue_comment` event carries the PR author in its payload. It does not have this limit.
- A GitLab MR event names the author only by numeric id. The hub knows the author name when the person who acts is the author, or when it has seen the MR before. An MR event by another person on an MR that the hub has not seen is dropped as `no_owner`.
- GitLab MRs are never selected by `subscriptions`.
- The PR memory lives for `retention.indexDays` (60 days) after the last event of the PR.

## Configuration

The hub reads one YAML file. The default path is `/etc/git-event-hub/config.yaml`. Set another path with `-config <file>` or with the variable `HUB_CONFIG`. The example file is `apps/server/config/example.yaml`.

Rules:

- **Secrets are never in the file.** Each secret is named by the environment variable that holds it (`secretEnv`, `urlEnv`, `tokenEnv`).
- **Unknown keys are errors.** A typo stops the load.
- **Live reload.** The hub checks the file every 2 seconds (modification time and size). A valid file replaces the old configuration at once. A file that fails to load or to validate leaves the old configuration in force. The hub then logs an error and counts `hub_config_reload_failed_total`.
- **Restart needed** for `server`, `database` and `log`. Changes of environment variables also need a restart.
- **Mount the directory, not the file.** A file bind mount keeps the old inode, so the hub does not see an edit. Docker Compose mounts `apps/server/config` for this reason.
- **A sink that did not change keeps running** (and keeps its queue position) when the file reloads. A changed sink restarts.
- **Check before you deploy:** `server -check -config <file>` validates the file and reads every secret variable that the file names. It does not need the database. It prints `configuration is valid` or the first error.

A zero value takes the default. A default cannot be switched off with `0`.

### `server` (restart)

| Key | Type | Default | Meaning |
|---|---|---|---|
| `host` | string | `0.0.0.0` | Listen address. |
| `port` | int, 1–65535 | `8080` | Listen port. |
| `maxBodyBytes` | int | `26214400` (25 MiB) | Largest webhook body. A larger body gets `413`. |

### `database` (restart)

| Key | Type | Default | Meaning |
|---|---|---|---|
| `urlEnv` | string | `DATABASE_URL` | Variable that holds the Postgres URL. |
| `maxOpenConns` | int ≥ 1 | `10` | Size of the connection pool. |

### `log` (restart)

| Key | Type | Default | Meaning |
|---|---|---|---|
| `level` | `debug`, `info`, `warn`, `error` | `info` | Log level. |
| `format` | `json`, `console` | `json` | Log format. |

### `api`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `tokenEnv` | string | `HUB_API_TOKEN` | Variable that holds the bearer token of `/api/*` and `/metrics`. If the variable is empty, these endpoints refuse every request. |

### `sources`

A list. Each entry is one forge. The hub accepts `POST /hooks/<name>`.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string, required | none | URL segment. Lower case: `a-z`, `0-9`, `-`, `_`. Unique. |
| `type` | `githubapp`, `gitlab` | `githubapp` for the name `github`, otherwise the name | Adapter. |
| `enabled` | bool | `true` | A disabled source answers `202` with `ignored` and checks nothing. |
| `secretEnv` | string, required | none | Variable with the GitHub webhook secret or the GitLab secret token. For GitLab, a comma-separated list gives several tokens (one for each webhook). |
| `repos` | list of globs | all repositories | Repositories that the source may speak for. Use one source for each GitLab group webhook, with its own token and `repos`. |

An enabled source whose variable is empty stops the load.

### `defaults`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `kinds` | list of event kinds | all kinds | Kinds that the hub accepts at all. |
| `commentAuthors` | list of globs | `["*[bot]"]` | Accounts whose comments count. A pattern made only of `*` is refused. |
| `reviewAuthors` | list of globs | none | Accounts that write review blocks. Only their comments become `review.finished` events. With an empty list every `review.finished` event is dropped. A pattern made only of `*` is refused. |
| `delivery` | object | see below | Default retry policy of every sink. |

`delivery` (in `defaults` and in each sink; a zero field of a sink takes the default):

| Key | Type | Default | Meaning |
|---|---|---|---|
| `batchSize` | int, 0–500 | `20` | Largest number of events in one call. |
| `timeout` | duration | `10s` | Limit of one call. |
| `backoff.initial` | duration | `2s` | First wait after a failure. |
| `backoff.max` | duration | `5m` | The wait doubles up to this value. Must not be below `initial`. |
| `maxAttempts` | int | `20` | After this number of failed calls, the event becomes `failed`. |
| `maxAge` | duration | `72h` | An event that waits longer becomes `expired`. |

### `retention`

| Key | Type | Default | Meaning |
|---|---|---|---|
| `receiptsDays` | int | `14` | Webhook receipts (the deduplication window). |
| `eventsDays` | int | `30` | Events, finished outbox rows and the delivery log. |
| `indexDays` | int | `60` | PR memory, counted from the last event of the PR. |

The hub prunes once at start and then every hour. Rows of sinks that are no longer configured go after `eventsDays` too. A sink or subscriber that is only disabled keeps its pending rows.

### `subscribers`

A list. Each entry is one team member, or one system.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `id` | string, required | none | Name (same character rules as a source name). Unique. |
| `enabled` | bool | `true` | A disabled subscriber receives nothing and owns no identity. |
| `identities.github` | list of strings | none | GitHub logins. Case is ignored. |
| `identities.gitlab` | list of strings | none | GitLab usernames. Include any bot or token account that agents use to open MRs. |
| `filter` | filter | none | Narrows what the subscriber receives. |
| `subscriptions` | list | none | GitHub repositories to follow whoever opens the PR. Each has `repos` (list of globs, required) and `kinds` (optional). |
| `sinks` | list, at least one | none | Delivery targets. |

One identity cannot belong to two enabled subscribers. The load fails if it does.

`filter` (subscriber and sink):

| Key | Meaning |
|---|---|
| `providers` | `github`, `gitlab`. |
| `repos.include` | Globs. Empty means all repositories. |
| `repos.exclude` | Globs. |
| `kinds` | Event kinds. |

Sink entry:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string, required | none | Unique for the subscriber. The queue name is `<subscriber>/<sink>`. |
| `type` | string, required | none | Adapter: `omb` or `webhook`. |
| `enabled` | bool | `true` | |
| `filter` | filter | none | |
| `delivery` | delivery | from `defaults.delivery` | |
| any other key | | | Adapter settings. See [Delivery](#delivery). An unknown key stops the load. |

## Delivery

### Semantics

- **At least once.** The hub can send an event again (a crash after the call, a lost answer). The consumer must dedupe by event `id`.
- **Order per sink.** A dispatcher claims the oldest pending events of its sink, at most `batchSize`. It keeps the order in which the hub stored them.
- **Head-of-line backoff.** After a failure, the failed event waits `backoff.initial`. The wait doubles at each failure up to `backoff.max`. Later events of the same sink wait behind it. Other sinks are not affected.
- **`maxAttempts`.** After this number of failed calls, the event gets the state `failed`. The sink then goes on with the next event.
- **`maxAge`.** A pending event that is older than this (counted from storage) gets the state `expired`.
- **`rejected`.** The consumer refused the event for good. The hub stops sending it.
- **Outbox states:** `pending`, `done`, `rejected`, `failed`, `expired`.
- **Delivery log.** Each attempt writes one entry. Outcomes: `acked`, `retry`, `rejected`, `failed`, `expired`. Read it with `GET /api/deliveries`.
- A sink that gives no answer for any event of a batch counts every event as `retry`. When the sink answers for some events of the batch, an event without an answer was not sent: it stays pending, with no attempt counted and no log entry. A panic in a sink counts as a failed call.
- The hub saves the result of a call even when the hub stops during the call.
- A sink is "healthy" when its last call worked. A new sink is healthy until its first failure.

### `omb` sink

Settings: `url` (absolute http or https URL) and `secretEnv`.

Request:

```text
POST <url>
Content-Type: application/json
X-Omb-Timestamp: <unix seconds>
X-Omb-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">

{"v":1,"events":[ ...pr-event v1... ]}
```

The key of the HMAC is the secret. The signed text is the timestamp, a dot and the raw body.

Answer: status `200` with

```json
{"acked": ["<event id>"], "rejected": ["<event id>"]}
```

An id in neither list is sent again later. Any other status, or an answer that is not this JSON, sends the whole batch again.

### `webhook` sink

Settings:

| Key | Values | Default | Meaning |
|---|---|---|---|
| `url` | absolute http or https URL | none | Endpoint. |
| `mode` | `single`, `batch` | `single` | `single` sends one request for each event, with the event as the body. `batch` sends `{"v":1,"events":[…]}`. |
| `auth.type` | `none`, `hmac`, `bearer` | `none` | |
| `auth.header` | header name | `X-Signature-256` (hmac), `Authorization` (bearer) | |
| `auth.secretEnv` | variable name | none | Required for `hmac` and `bearer`. |

With `hmac`, the header value is `sha256=` and the hex HMAC-SHA256 of the raw body. With `bearer`, it is `Bearer <secret>`. Any `2xx` answer means the event was taken. Any other answer means retry. In `single` mode, the hub stops at the first failed event, so the order stays the same.

All sink requests carry `Content-Type: application/json` and `User-Agent: git-event-hub`.

## HTTP API

The contract is `apps/server/api/openapi.yaml`.

### `POST /hooks/{source}`

The body is the forge payload as sent. The signature (GitHub `X-Hub-Signature-256`) or the token (GitLab `X-Gitlab-Token`) covers the raw bytes. The hub answers with `{"accepted":n,"dropped":n,...}`.

| Status | Meaning |
|---|---|
| `202` | The hub stored the delivery. Events that no subscriber wants are counted and dropped, and the answer is still `202`. Also `202` with `ignored` for a disabled source. |
| `200` | The hub saw this delivery before (`duplicate: true`). |
| `400` | The payload does not match the forge format, or the delivery has no id. |
| `401` | The signature or token is missing or wrong. The answer never says which. |
| `404` | Unknown source name. The hub checks the name before it reads the body. |
| `413` | The body is larger than `server.maxBodyBytes`. |
| `500` | Storage error. The forge retries, and the retry is deduplicated. |

The delivery id is `X-GitHub-Delivery` for GitHub. For GitLab it is `Idempotency-Key`, then `X-Gitlab-Event-UUID`, then a hash of the body.

### `GET /healthz`

Public. It carries no repository data. It answers `200` with `status`, `version`, `contract`, `uptimeSeconds` and `sinks` (`total`, `healthy`, `pending`). It answers `503` with `status: "database unavailable"` when the database does not answer. A failing sink does not make the hub unhealthy, because a restart does not help it.

### Protected endpoints

`GET /metrics`, `GET /api/events` and `GET /api/deliveries` need the header `Authorization: Bearer <value of the variable api.tokenEnv>`. They answer `401` when the header is wrong. They answer `401` to every request when the variable is empty.

`GET /metrics` returns counters and gauges in the Prometheus text format. Counters start from zero at each start. Main series: `hub_received_total`, `hub_refused_total`, `hub_deduped_total`, `hub_dropped_total`, `hub_queued_total`, `hub_delivered_total`, `hub_retried_total`, `hub_rejected_total`, `hub_failed_total`, `hub_expired_total`, `hub_config_reloaded_total`, `hub_config_reload_failed_total`, and for each sink `hub_queue_pending`, `hub_queue_oldest_seconds`, `hub_sink_healthy`.

`GET /api/events` returns stored events, newest first, with the state of each sink.

| Parameter | Meaning |
|---|---|
| `subscriber` | Only events for this subscriber. `targets` then lists only its sinks. |
| `provider` | `github` or `gitlab`. |
| `repo`, `number`, `kind` | Exact match. An invalid `kind` gives `400`. |
| `before` | Return events with a smaller `seq`. |
| `limit` | 1–200. Default 50. |

`GET /api/deliveries` returns the delivery log, newest first. Parameters: `subscriber`, `sink`, `eventId`, `outcome` (`acked`, `retry`, `rejected`, `failed`, `expired`), `before` (smaller `id`), `limit`.

Paging: a page that is full carries `nextBefore`. Send it as `before` to read the next page. The last page has no `nextBefore`.

```sh
curl -H "Authorization: Bearer $HUB_API_TOKEN" \
  "http://localhost:8080/api/events?subscriber=kevin&limit=20"
```

### Web UI

The web UI comes later. Its login will be VTC SSO, with the same protocol as `plans/261120-0010-vtc-sso-login` in operator-master. Until then, use the API.

## Add a source or a sink

A source and a sink are adapters. Each one lives in its own package and registers itself in `init`.

### New source (a new forge)

1. Create `internal/source/<name>/<name>.go`.
2. Implement `source.WebhookSource` (`internal/source/source.go`): `ID`, `Provider`, `Verify`, `DeliveryID`, `Parse`.
   - `Verify` checks the signature on the raw body. It must not read the body as JSON.
   - `DeliveryID` returns an id that stays the same for a retry.
   - `Parse` returns events. Call `c.Drop(reason)` when a payload gives no event. Wrap `normalize.ErrPayload` for a payload with the wrong shape.
3. Add a normalizer in `internal/normalize` if the forge needs one. Only `prevents.Event` may leave this layer.
4. Call `source.Register("<type>", New)` from `init`.
5. Add one blank import to `internal/app/app.go`:
   `_ "git-event-hub/internal/source/<name>"`.
6. Add tests with webhook fixtures in `apps/server/testdata/<forge>/`. Use `testutil.Fixture`.
7. If the forge needs a new provider value, update `pkg/prevents`, the JSON Schema and the `.d.ts` file together.

### New sink

1. Create `internal/sink/<name>/<name>.go`.
2. Implement `sink.EventSink` (`internal/sink/sink.go`): `Deliver(ctx, batch)` and `Health(ctx)`. Embed `sink.Tracker` for `Health`.
   - `Deliver` returns one `sink.Outcome` (`Acked`, `Retry` or `Rejected`) for each event. A non-nil error means the whole batch failed.
   - `Deliver` must be safe to call again with events that the consumer already has.
3. Read the settings in the factory with `sink.DecodeSettings`. It rejects unknown keys. Read secrets only through `spec.Env`. Return an error when a secret variable is empty.
4. Call `sink.Register("<type>", New)` from `init`.
5. Add one blank import to `internal/app/app.go`.
6. Add tests. `internal/sink/sinktest` has helpers.

## Run locally and test

Requirements: Go 1.26 (the version in `apps/server/go.mod`). A C compiler for the SQLite tests (cgo). Docker for the Compose run.

### With Docker Compose

```sh
cp .env.example .env
cp apps/server/.env.example apps/server/.env   # then fill in the values
docker compose up --build
```

- `apps/server/.env` must hold a value for every secret variable that the config names. With the default `example.yaml`, these are `GITHUB_WEBHOOK_SECRET`, `GITLAB_WEBHOOK_TOKEN`, `OMB_KEVIN_SECRET`, `HERMES_LAN_SECRET` and `HUB_API_TOKEN`. The hub does not start when a variable that the config needs is empty. Any value works for a local run.
- Compose starts the hub and Postgres 16. It sets `DATABASE_URL` for the bundled database.
- It mounts the directory `apps/server/config` read-only. `HUB_CONFIG_FILE` (default `example.yaml`) in `.env` picks the file. An edit of the file reaches the container and the hub reloads it.
- The hub listens on `127.0.0.1:${HUB_PORT:-8080}`.
- Check it: `curl http://127.0.0.1:8080/healthz`.

### Without Docker

You need a Postgres database and the same secret variables.

```sh
cd apps/server
export DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/git-event-hub?sslmode=disable'
export GITHUB_WEBHOOK_SECRET=... GITLAB_WEBHOOK_TOKEN=... HUB_API_TOKEN=...
export OMB_KEVIN_SECRET=... HERMES_LAN_SECRET=...
go run ./cmd/server -config config/example.yaml
```

The hub applies the migrations at start.

Check the configuration without a database:

```sh
go run ./cmd/server -check -config config/example.yaml
```

Other flags: `-version`, and `-healthcheck <url>` (exit code 0 when the URL answers 200; the Docker health check uses it).

### Tests and checks

Run these in `apps/server`.

| Command | What it does |
|---|---|
| `go test -race ./...` | All tests on SQLite. |
| `TEST_DATABASE_URL=postgres://USER:PASSWORD@HOST:PORT/DB?sslmode=disable go test -race -count=1 ./...` | The same tests on Postgres. Each test gets its own schema. |
| `go vet ./...` | Vet. |
| `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint run ./...` | Lint. |
| `go generate ./internal/api` | Regenerate `internal/api/openapi.gen.go` after you edit `api/openapi.yaml`. |

CI (`.github/workflows/ci.yml`) runs: the generated code is up to date, vet, lint, tests on SQLite and on Postgres, `-check` on the example configuration, and the Docker build. The web job runs `bun run check-types` and `bun run build`.

The script `bun run dev:server` runs `go run cmd/server/main.go` without `-config`. Set `HUB_CONFIG` first, or use the commands above.

## Deploy

The hub is designed for one Komodo stack on the VTC dev server (`103.82.28.49`). The stack is reachable from the internet through Tailscale Funnel on port 8443.

- Deploy only when the person asks. The runbook is the skill `deploy-vtc-komodo` of operator-master. Do not run Compose by hand on the server.
- **Run one instance only.** The dispatchers do not support several replicas. Two instances would send events twice and break the order.
- **Back up Postgres before a schema change** (a new migration). A code-only deploy needs no backup.
- Migrations run at start. Keep `migrations/postgres` and `migrations/sqlite` in step.
- Put secrets in the stack environment, never in the config file.
- Edit the config file to change subscribers or sinks. The hub reloads it. A restart is needed only for `server`, `database`, `log` and environment variables.
- The image is built from `apps/server/Dockerfile`. It has no cgo and no SQLite. It runs as the user `hub`. The health check calls `/healthz`.

## Human setup

Do these steps once, in this order. Replace `<funnel host>` with the public host name of the VTC dev server. It is `vtc.taildae6a9.ts.net`, the host that OCR uses (see `docs/ocr-review.md` in operator-master).

Funnel serves each service on its own path of port 8443 (OCR uses `/ocr`, and n8n uses `/webhook`). Funnel removes the path prefix before it forwards the request. Add a Funnel path for the hub that forwards to the hub port. Then use that path as `<hub path>` below. If you give the hub the whole port, leave `<hub path>` out.

### 1. GitHub App

Create the App in the GitHub settings of the account or the organization.

| Setting | Value |
|---|---|
| Name | For example `vtc-git-event-hub`. |
| Homepage URL | The repository URL, `https://github.com/kevinle128/git-event-hub`. |
| Webhook: Active | On. |
| Webhook URL | `https://<funnel host>:8443/<hub path>/hooks/github` |
| Webhook secret | A long random value. Put the same value in the variable `GITHUB_WEBHOOK_SECRET` of the hub. |
| SSL verification | Enabled. |

Repository permissions. The hub itself needs only the webhooks. OMB later uses the private key of the same App, so set all of these now:

- Contents: Read and write
- Pull requests: Read and write
- Checks: Read-only
- Commit statuses: Read-only
- Actions: Read-only
- Issues: Read-only
- Metadata: Read-only

Subscribe to these events: **Pull request**, **Check run**, **Check suite**, **Workflow run**, **Status**, **Issue comment**.

Then:

1. Install the App on each team account or organization, with access to all repositories.
2. Add the GitHub login of each member to `identities.github` of that member in the config file.
3. Keep the private key of the App for OMB. The hub never needs it.

### 2. GitLab group webhook

In the GitLab group: **Settings → Webhooks → Add new webhook**.

| Setting | Value |
|---|---|
| URL | `https://<funnel host>:8443/<hub path>/hooks/gitlab` |
| Secret token | A long random value. Put it in the variable `GITLAB_WEBHOOK_TOKEN` of the hub. For several webhooks, use one token each and separate them with commas in the variable. |
| Triggers | **Merge request events**, **Pipeline events**, **Comments** |
| Enable SSL verification | On |

Group webhooks need GitLab Premium. Without Premium, add the same webhook to each project.

Then add the GitLab username of each member to `identities.gitlab`. Add every bot or token account that agents use to open MRs. An MR that is opened by an account that is not listed is dropped as `no_subscriber`.

### 3. Secrets

Set these variables in the environment of the hub. Do not write them in the config file or in Git.

| Variable | Used for |
|---|---|
| `DATABASE_URL` | Postgres URL (`database.urlEnv`). |
| `GITHUB_WEBHOOK_SECRET` | GitHub App webhook secret. |
| `GITLAB_WEBHOOK_TOKEN` | GitLab secret token (several tokens: comma-separated). |
| `HUB_API_TOKEN` | Bearer token of `/api/*` and `/metrics`. If it is empty, these endpoints refuse every request. |
| One variable for each sink | The `secretEnv` of the sink, for example `OMB_KEVIN_SECRET`. The same value must be configured in the consumer so that it can verify the signature. |

### 4. Check

1. `server -check -config <file>` with the real environment.
2. Send a test delivery from the GitHub App page (**Advanced → Recent deliveries → Redeliver**) or from the GitLab webhook page (**Test**). A `202` answer means the hub stored it.
3. Read the result: `GET /api/events` and `GET /api/deliveries` with the bearer token.

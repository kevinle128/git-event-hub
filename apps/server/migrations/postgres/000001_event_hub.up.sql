-- Webhook deliveries that the hub accepted. The primary key drops a repeated
-- delivery of the same webhook.
CREATE TABLE IF NOT EXISTS webhook_receipts (
	source      TEXT    NOT NULL,
	delivery_id TEXT    NOT NULL,
	event_type  TEXT    NOT NULL DEFAULT '',
	received_at BIGINT  NOT NULL,
	events      INTEGER NOT NULL DEFAULT 0,
	queued      INTEGER NOT NULL DEFAULT 0,
	dropped     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (source, delivery_id)
);
CREATE INDEX IF NOT EXISTS webhook_receipts_received ON webhook_receipts (received_at);

-- Contract events that at least one sink must receive.
CREATE TABLE IF NOT EXISTS events (
	seq        BIGSERIAL PRIMARY KEY,
	id         TEXT    NOT NULL UNIQUE,
	provider   TEXT    NOT NULL,
	repo       TEXT    NOT NULL,
	number     INTEGER NOT NULL,
	kind       TEXT    NOT NULL,
	author     TEXT    NOT NULL DEFAULT '',
	payload    TEXT    NOT NULL,
	created_at BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS events_created ON events (created_at);
CREATE INDEX IF NOT EXISTS events_pr ON events (provider, repo, number);

-- One row per event and sink: the per-sink queue with its own retry state.
CREATE TABLE IF NOT EXISTS outbox (
	sink_key   TEXT    NOT NULL,
	seq        BIGINT  NOT NULL REFERENCES events (seq) ON DELETE CASCADE,
	subscriber TEXT    NOT NULL,
	sink       TEXT    NOT NULL,
	state      TEXT    NOT NULL DEFAULT 'pending',
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_at    BIGINT  NOT NULL,
	last_error TEXT    NOT NULL DEFAULT '',
	updated_at BIGINT  NOT NULL,
	PRIMARY KEY (sink_key, seq)
);
CREATE INDEX IF NOT EXISTS outbox_pending ON outbox (sink_key, state, seq);
CREATE INDEX IF NOT EXISTS outbox_seq ON outbox (seq);
CREATE INDEX IF NOT EXISTS outbox_subscriber ON outbox (subscriber, seq);

-- Every delivery attempt and its outcome.
CREATE TABLE IF NOT EXISTS delivery_log (
	id          BIGSERIAL PRIMARY KEY,
	sink_key    TEXT    NOT NULL,
	subscriber  TEXT    NOT NULL,
	sink        TEXT    NOT NULL,
	seq         BIGINT  NOT NULL,
	event_id    TEXT    NOT NULL,
	attempt     INTEGER NOT NULL,
	outcome     TEXT    NOT NULL,
	error       TEXT    NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	at          BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS delivery_log_at ON delivery_log (at);
CREATE INDEX IF NOT EXISTS delivery_log_subscriber ON delivery_log (subscriber, id);
CREATE INDEX IF NOT EXISTS delivery_log_event ON delivery_log (event_id);

-- What the hub knows about each pull request, so a later check, pipeline or
-- comment event finds its author, branch and head.
CREATE TABLE IF NOT EXISTS pr_index (
	provider   TEXT    NOT NULL,
	repo       TEXT    NOT NULL,
	number     INTEGER NOT NULL,
	author     TEXT    NOT NULL DEFAULT '',
	branch     TEXT    NOT NULL DEFAULT '',
	base_ref   TEXT    NOT NULL DEFAULT '',
	head_sha   TEXT    NOT NULL DEFAULT '',
	updated_at BIGINT  NOT NULL,
	PRIMARY KEY (provider, repo, number)
);
CREATE INDEX IF NOT EXISTS pr_index_branch ON pr_index (provider, repo, branch, updated_at);
CREATE INDEX IF NOT EXISTS pr_index_sha ON pr_index (provider, repo, head_sha, updated_at);

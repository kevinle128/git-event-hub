// Package migrations holds the database schema. Production runs on Postgres;
// the sqlite copy exists only for fast tests and must stay equal in meaning
// (the tests check that both directories have the same files).
package migrations

import "embed"

// Postgres is the production schema.
//
//go:embed postgres/*.sql
var Postgres embed.FS

// SQLite is the test schema.
//
//go:embed sqlite/*.sql
var SQLite embed.FS

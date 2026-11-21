// Package storetest opens a fresh database for tests.
//
// With TEST_DATABASE_URL set (a Postgres URL), each call creates its own
// Postgres schema, so tests run against the production dialect. Without it,
// each call uses a SQLite file in a temporary directory. Only tests import
// this package, so the SQLite driver never reaches the server binary.
package storetest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	sqlitemigrate "github.com/golang-migrate/migrate/v4/database/sqlite3"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"git-event-hub/internal/database"
	"git-event-hub/internal/store"
	"git-event-hub/migrations"
)

// DB is a test database that can be opened again, as a restarted process
// would.
type DB struct {
	t      testing.TB
	open   func() *gorm.DB
	handle *gorm.DB
	// Dialect is "postgres" or "sqlite".
	Dialect string
}

// New creates an empty database with the hub schema.
func New(t testing.TB) *DB {
	t.Helper()
	if base := os.Getenv("TEST_DATABASE_URL"); base != "" {
		return newPostgres(t, base)
	}
	return newSQLite(t)
}

// Gorm returns the open handle.
func (d *DB) Gorm() *gorm.DB { return d.handle }

// Store returns a store on the open handle.
func (d *DB) Store() *store.Store { return store.New(d.handle) }

// Reopen closes the handle and opens the same database again.
func (d *DB) Reopen() *store.Store {
	d.t.Helper()
	if err := database.Close(d.handle); err != nil {
		d.t.Fatal(err)
	}
	d.handle = d.open()
	return store.New(d.handle)
}

func newSQLite(t testing.TB) *DB {
	path := filepath.Join(t.TempDir(), "hub.db")
	d := &DB{t: t, Dialect: "sqlite"}
	d.open = func() *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+path+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		// One connection keeps SQLite free of lock errors.
		sqlDB.SetMaxOpenConns(1)
		drv, err := sqlitemigrate.WithInstance(sqlDB, &sqlitemigrate.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := database.Migrate(drv, "sqlite3", migrations.SQLite, "sqlite"); err != nil {
			t.Fatal(err)
		}
		return db
	}
	d.handle = d.open()
	t.Cleanup(func() { _ = database.Close(d.handle) })
	return d
}

func newPostgres(t testing.TB, base string) *DB {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	admin, err := gorm.Open(postgres.Open(base), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	qs := u.Query()
	qs.Set("search_path", schema)
	u.RawQuery = qs.Encode()
	d := &DB{t: t, Dialect: "postgres"}
	d.open = func() *gorm.DB {
		db, err := database.OpenPostgres(u.String(), 5)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	d.handle = d.open()
	t.Cleanup(func() {
		_ = database.Close(d.handle)
		if err := admin.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE", schema)).Error; err != nil {
			t.Logf("drop test schema: %v", err)
		}
		_ = database.Close(admin)
	})
	return d
}

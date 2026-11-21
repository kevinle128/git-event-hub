// Package database opens the Postgres database and brings its schema up to
// date with the embedded migrations.
package database

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"git-event-hub/migrations"
)

// OpenPostgres connects to url, runs the migrations and returns the handle.
func OpenPostgres(url string, maxOpenConns int) (*gorm.DB, error) {
	if url == "" {
		return nil, errors.New("database: the database URL is empty")
	}
	db, err := gorm.Open(postgres.Open(url), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	if err := migratePostgres(url); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// migratePostgres runs the migrations on a pool of its own. The driver pins
// one connection of the pool for its advisory lock and releases it only on
// Close, which also closes the pool; so it must not use the pool of the app.
func migratePostgres(url string) error {
	mdb, err := gorm.Open(postgres.Open(url), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	msql, err := mdb.DB()
	if err != nil {
		return err
	}
	drv, err := pgxmigrate.WithInstance(msql, &pgxmigrate.Config{})
	if err != nil {
		_ = msql.Close()
		return fmt.Errorf("database: migrate driver: %w", err)
	}
	err = Migrate(drv, "pgx5", migrations.Postgres, "postgres")
	_ = drv.Close()
	return err
}

// Migrate applies every migration in dir of fsys through drv. It does not
// close drv; the caller does.
func Migrate(drv database.Driver, name string, fsys fs.FS, dir string) error {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("database: migrations: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, name, drv)
	if err != nil {
		return fmt.Errorf("database: migrations: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database: migrations: %w", err)
	}
	return nil
}

// Close closes the connection pool of db.
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

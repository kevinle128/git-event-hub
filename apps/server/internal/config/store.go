package config

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Store holds the configuration in force and swaps it when the file changes.
// A file that fails to load or to check leaves the old configuration in place.
type Store struct {
	path  string
	check func(*Config) error
	cur   atomic.Pointer[Config]
	log   *zap.Logger
	// OnReloadError is called for each failed reload. It may be nil.
	OnReloadError func(error)
}

// NewStore loads the file once. check runs after the built-in validation and
// may reject a configuration, for example when an adapter does not accept its
// settings or a secret is missing. check may be nil.
func NewStore(path string, check func(*Config) error, log *zap.Logger) (*Store, error) {
	s := &Store{path: path, check: check, log: log}
	c, err := s.read()
	if err != nil {
		return nil, err
	}
	s.cur.Store(c)
	return s, nil
}

// Path returns the file that the store reads.
func (s *Store) Path() string { return s.path }

func (s *Store) read() (*Config, error) {
	c, err := Load(s.path)
	if err != nil {
		return nil, err
	}
	if s.check != nil {
		if err := s.check(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Current returns the configuration in force.
func (s *Store) Current() *Config { return s.cur.Load() }

// Reload reads the file now. On success the new configuration is in force and
// is returned; on failure the old one stays and the error is returned.
func (s *Store) Reload() (*Config, error) {
	c, err := s.read()
	if err != nil {
		s.log.Error("config reload failed; keeping the old configuration", zap.String("path", s.path), zap.Error(err))
		if s.OnReloadError != nil {
			s.OnReloadError(err)
		}
		return nil, err
	}
	s.cur.Store(c)
	s.log.Info("config reloaded", zap.String("path", s.path))
	return c, nil
}

// Watch polls the file every interval and calls onChange with each new valid
// configuration, until ctx ends. A poll compares modification time and size,
// so it also sees a file that an editor or a bind mount replaces. (Mount the
// config directory, not the file: a file bind mount keeps the old inode.)
func (s *Store) Watch(ctx context.Context, interval time.Duration, onChange func(*Config)) {
	last := stat(s.path)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := stat(s.path)
		if now == last {
			continue
		}
		last = now
		if c, err := s.Reload(); err == nil {
			onChange(c)
		}
	}
}

type fileStamp struct {
	mod  time.Time
	size int64
}

func stat(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{fi.ModTime(), fi.Size()}
}

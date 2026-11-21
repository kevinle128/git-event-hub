// Package app wires the hub together with fx: configuration, database,
// store, hub (dispatchers and config watcher) and the HTTP server, each with
// its start and stop hooks.
package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/labstack/echo/v4"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"git-event-hub/internal/config"
	"git-event-hub/internal/database"
	"git-event-hub/internal/httpapi"
	"git-event-hub/internal/hub"
	"git-event-hub/internal/metrics"
	"git-event-hub/internal/store"

	// Adapters register themselves. A new source or sink needs one import here.
	_ "git-event-hub/internal/sink/omb"
	_ "git-event-hub/internal/sink/webhook"
	_ "git-event-hub/internal/source/githubapp"
	_ "git-event-hub/internal/source/gitlab"
)

// Options builds the fx options of the server. configPath is the YAML file.
func Options(configPath, version string, log *zap.Logger) fx.Option {
	return fx.Options(
		fx.Supply(log),
		fx.WithLogger(func() fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx").WithOptions(zap.IncreaseLevel(zap.WarnLevel))}
		}),
		fx.Provide(
			func() (*config.Store, error) { return config.NewStore(configPath, hub.Check(os.Getenv), log) },
			openDatabase,
			store.New,
			metrics.New,
			func(st *store.Store, cs *config.Store, m *metrics.Registry) (*hub.Hub, error) {
				return hub.New(st, cs, m, log, os.Getenv)
			},
			func(h *hub.Hub, cs *config.Store) *echo.Echo { return httpapi.New(h, cs, os.Getenv, log, version) },
		),
		fx.Invoke(startHub, startHTTP),
	)
}

func openDatabase(cs *config.Store, lc fx.Lifecycle) (*gorm.DB, error) {
	c := cs.Current().Database
	db, err := database.OpenPostgres(os.Getenv(c.URLEnv), c.MaxOpenConns)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.StopHook(func() error { return database.Close(db) }))
	return db, nil
}

func startHub(h *hub.Hub, lc fx.Lifecycle) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { return h.Start() },
		OnStop:  func(context.Context) error { h.Stop(); return nil },
	})
}

func startHTTP(e *echo.Echo, cs *config.Store, log *zap.Logger, lc fx.Lifecycle) {
	addr := cs.Current().Server.Addr()
	srv := &http.Server{
		Addr: addr, Handler: e,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("http server stopped", zap.Error(err))
				}
			}()
			log.Info("listening", zap.String("addr", addr), zap.String("config", cs.Path()))
			return nil
		},
		OnStop: func(ctx context.Context) error { return srv.Shutdown(ctx) },
	})
}

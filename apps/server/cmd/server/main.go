// Command server runs git-event-hub: it receives pull request and merge
// request webhooks from GitHub and GitLab, turns them into pr-event v1
// events, stores them, and delivers them to the sinks of each subscriber.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"git-event-hub/internal/app"
	"git-event-hub/internal/config"
	"git-event-hub/internal/hub"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "git-event-hub:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", envOr("HUB_CONFIG", "/etc/git-event-hub/config.yaml"), "path of the YAML configuration")
	check := flag.Bool("check", false, "check the configuration and the secrets it names, then exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.String("healthcheck", "", "call this URL and exit 0 when it answers 200 (for the container health check)")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println(version)
		return nil
	case *healthcheck != "":
		return probe(*healthcheck)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if *check {
		if err := hub.Check(os.Getenv)(cfg); err != nil {
			return err
		}
		fmt.Println("configuration is valid")
		return nil
	}
	log, err := newLogger(cfg.Log)
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()
	log = log.With(zap.String("service", "git-event-hub"), zap.String("version", version))
	fx.New(app.Options(*path, version, log)).Run()
	return nil
}

func newLogger(c config.Log) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(c.Level)
	if err != nil {
		return nil, err
	}
	zc := zap.NewProductionConfig()
	if c.Format == "console" {
		zc = zap.NewDevelopmentConfig()
	}
	zc.Level = zap.NewAtomicLevelAt(lvl)
	return zc.Build()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func probe(url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check answered %d", resp.StatusCode)
	}
	return nil
}

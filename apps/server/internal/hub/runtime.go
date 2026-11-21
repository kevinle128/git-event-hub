package hub

import (
	"fmt"

	"git-event-hub/internal/config"
	"git-event-hub/internal/glob"
	"git-event-hub/internal/route"
	"git-event-hub/internal/sink"
	"git-event-hub/internal/source"
)

// runtime is everything that is built from one configuration: the sources
// that accept webhooks and the router. A reload builds a new runtime and
// swaps it in whole.
type runtime struct {
	cfg      *config.Config
	sources  map[string]source.WebhookSource
	disabled map[string]bool
	// scopes holds the repository globs of each source that sets repos. A
	// source without an entry answers for every repository.
	scopes map[string]glob.Set
	router *route.Router
}

func buildRuntime(cfg *config.Config, getenv func(string) string) (*runtime, error) {
	rt := &runtime{cfg: cfg, sources: map[string]source.WebhookSource{}, disabled: map[string]bool{}, scopes: map[string]glob.Set{}}
	for _, sc := range cfg.Sources {
		if !sc.On() {
			rt.disabled[sc.Name] = true
			continue
		}
		src, err := source.New(sc.AdapterType(), source.Settings{ID: sc.Name, Secret: getenv(sc.SecretEnv)})
		if err != nil {
			return nil, fmt.Errorf("config: source %s: %w (variable %s)", sc.Name, err, sc.SecretEnv)
		}
		rt.sources[sc.Name] = src
		if len(sc.Repos) > 0 {
			scope, err := glob.CompileSet(sc.Repos)
			if err != nil {
				return nil, fmt.Errorf("config: source %s: %w", sc.Name, err)
			}
			rt.scopes[sc.Name] = scope
		}
	}
	r, err := route.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	rt.router = r
	return rt, nil
}

// sinkSpec is an enabled sink with its subscriber.
type sinkSpec struct {
	key string
	cfg config.Sink
}

func enabledSinks(cfg *config.Config) []sinkSpec {
	var out []sinkSpec
	for _, s := range cfg.Subscribers {
		if !s.On() {
			continue
		}
		for _, k := range s.Sinks {
			if k.On() {
				out = append(out, sinkSpec{key: config.Key(s.ID, k.Name), cfg: k})
			}
		}
	}
	return out
}

func newSink(sp sinkSpec, getenv func(string) string) (sink.EventSink, error) {
	s, err := sink.New(sp.cfg.Type, sink.Spec{Name: sp.key, Settings: sp.cfg.Settings, Env: getenv})
	if err != nil {
		return nil, fmt.Errorf("config: sink %s: %w", sp.key, err)
	}
	return s, nil
}

// Check returns the function that config.NewStore runs on each
// configuration. It builds every source, the router and every enabled sink
// once, so a missing secret, an unknown adapter type or a typo in a sink
// setting stops the load; on a reload the old configuration stays in force.
func Check(getenv func(string) string) func(*config.Config) error {
	return func(cfg *config.Config) error {
		if _, err := buildRuntime(cfg, getenv); err != nil {
			return err
		}
		for _, sp := range enabledSinks(cfg) {
			if _, err := newSink(sp, getenv); err != nil {
				return err
			}
		}
		return nil
	}
}

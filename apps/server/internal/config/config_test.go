package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const minimal = `
sources:
  - {name: github, secretEnv: GH_SECRET}
subscribers:
  - id: kevin
    identities: {github: [kevinle128]}
    sinks:
      - {name: omb, type: omb, url: "http://omb.test/hook", secretEnv: OMB_SECRET}
`

func TestExampleConfigIsValid(t *testing.T) {
	c, err := Load("../../config/example.yaml")
	require.NoError(t, err)
	require.Len(t, c.Subscribers, 2)
	lan := c.Subscribers[1]
	assert.Equal(t, "lan", lan.ID)
	assert.Equal(t, []string{"kevinle128/git-event-hub"}, lan.Subscriptions[0].Repos)
	hermes := lan.Sinks[0]
	assert.Equal(t, 10, hermes.Delivery.MaxAttempts, "a sink value wins")
	assert.Equal(t, 20, hermes.Delivery.BatchSize, "a missing sink value comes from defaults")
	assert.Equal(t, "batch", hermes.Settings["mode"], "adapter keys go to Settings")
	_, leaked := hermes.Settings["name"]
	assert.False(t, leaked, "known keys must not reach Settings")
	assert.Equal(t, "githubapp", c.Sources[0].AdapterType())
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0:8080", c.Server.Addr())
	assert.Equal(t, "[::1]:8080", Server{Host: "::1", Port: 8080}.Addr())
	assert.Equal(t, "DATABASE_URL", c.Database.URLEnv)
	assert.Equal(t, "HUB_API_TOKEN", c.API.TokenEnv)
	assert.Equal(t, []string{"*[bot]"}, c.Defaults.CommentAuthors)
	assert.Empty(t, c.Defaults.ReviewAuthors, "review authors have no default")
	d := c.Subscribers[0].Sinks[0].Delivery
	assert.Equal(t, Delivery{BatchSize: 20, Timeout: 10 * time.Second, Backoff: Backoff{2 * time.Second, 5 * time.Minute}, MaxAttempts: 20, MaxAge: 72 * time.Hour}, d)
	assert.Equal(t, "gitlab", Source{Name: "gitlab"}.AdapterType())
	assert.True(t, c.Subscribers[0].On())
}

func TestParseRejects(t *testing.T) {
	tests := map[string]struct{ yaml, want string }{
		"unknown top key":          {minimal + "extra: 1\n", "extra"},
		"unknown subscriber key":   {strings.Replace(minimal, "    identities:", "    nickname: k\n    identities:", 1), "nickname"},
		"gitlab subscription":      {strings.Replace(minimal, "    sinks:", "    subscriptions:\n      - {repos: [\"g/**\"], providers: [gitlab]}\n    sinks:", 1), "providers"},
		"bad kind":                 {strings.Replace(minimal, "sources:", "defaults: {kinds: [opened, exploded]}\nsources:", 1), "exploded"},
		"bad provider":             {strings.Replace(minimal, "    sinks:", "    filter: {providers: [bitbucket]}\n    sinks:", 1), "bitbucket"},
		"bad name":                 {strings.Replace(minimal, "id: kevin", "id: Kevin Le", 1), "lower-case name"},
		"no sinks":                 {"subscribers:\n  - id: kevin\n", "Sinks is required"},
		"sink without type":        {strings.Replace(minimal, "type: omb, ", "", 1), "Type is required"},
		"duplicate subscriber":     {minimal + "  - id: kevin\n    sinks: [{name: x, type: webhook}]\n", "listed twice"},
		"shared identity":          {minimal + "  - id: lan\n    identities: {github: [KevinLe128]}\n    sinks: [{name: x, type: webhook}]\n", "belongs to both"},
		"duplicate source":         {strings.Replace(minimal, "sources:\n", "sources:\n  - {name: github, secretEnv: X}\n", 1), "listed twice"},
		"backoff order":            {strings.Replace(minimal, "sources:", "defaults: {delivery: {backoff: {initial: 1m, max: 1s}}}\nsources:", 1), "backoff.max"},
		"bad port":                 {"server: {port: 70000}\n" + minimal, "Port"},
		"bad duration":             {strings.Replace(minimal, "sources:", "defaults: {delivery: {timeout: soon}}\nsources:", 1), "timeout"},
		"empty file":               {"", "Sources is required"},
		"no subscribers":           {"sources:\n  - {name: github, secretEnv: GH_SECRET}\nsubscribers: []\n", "Subscribers"},
		"match-all comment author": {strings.Replace(minimal, "sources:", "defaults: {commentAuthors: [\"*\"]}\nsources:", 1), "matches every account"},
		"match-all comment author with two stars": {strings.Replace(minimal, "sources:", "defaults: {commentAuthors: [\"**\"]}\nsources:", 1), "matches every account"},
		"match-all review author":                 {strings.Replace(minimal, "sources:", "defaults: {reviewAuthors: [\"*\"]}\nsources:", 1), "matches every account"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestSourceReposAreParsed(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(minimal, "{name: github, secretEnv: GH_SECRET}", "{name: github, secretEnv: GH_SECRET, repos: [\"vtc/**\"]}", 1)))
	require.NoError(t, err)
	assert.Equal(t, []string{"vtc/**"}, c.Sources[0].Repos)
}

func TestReviewAuthorsAreParsed(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(minimal, "sources:", "defaults: {reviewAuthors: [\"ocr[bot]\"]}\nsources:", 1)))
	require.NoError(t, err)
	assert.Equal(t, []string{"ocr[bot]"}, c.Defaults.ReviewAuthors)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func TestStoreHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, minimal)
	var refuse atomic.Bool
	check := func(c *Config) error {
		if refuse.Load() {
			return errors.New("adapter refuses")
		}
		return nil
	}
	s, err := NewStore(path, check, zap.NewNop())
	require.NoError(t, err)
	var failures atomic.Int32
	s.OnReloadError = func(error) { failures.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan *Config, 4)
	go s.Watch(ctx, 10*time.Millisecond, func(c *Config) { changes <- c })

	// A valid change is applied.
	time.Sleep(20 * time.Millisecond)
	writeFile(t, path, strings.Replace(minimal, "kevinle128", "kevinle128, kevin-bot-account", 1))
	select {
	case c := <-changes:
		assert.Equal(t, []string{"kevinle128", "kevin-bot-account"}, c.Subscribers[0].Identities.GitHub)
	case <-time.After(5 * time.Second):
		t.Fatal("no reload")
	}

	// A broken file keeps the old configuration.
	writeFile(t, path, "subscribers: [")
	require.Eventually(t, func() bool { return failures.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Len(t, s.Current().Subscribers[0].Identities.GitHub, 2)

	// So does a file that the check refuses.
	refuse.Store(true)
	writeFile(t, path, minimal+"\n")
	require.Eventually(t, func() bool { return failures.Load() == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Len(t, s.Current().Subscribers[0].Identities.GitHub, 2)
}

func TestNewStoreFailsOnBadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, "subscribers: 3")
	_, err := NewStore(path, nil, zap.NewNop())
	assert.Error(t, err)
	_, err = NewStore(filepath.Join(t.TempDir(), "missing.yaml"), nil, zap.NewNop())
	assert.Error(t, err)
}

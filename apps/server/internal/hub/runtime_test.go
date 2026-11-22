package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/config"
	_ "git-event-hub/internal/sink/omb"
	_ "git-event-hub/internal/sink/webhook"
	_ "git-event-hub/internal/source/githubapp"
	_ "git-event-hub/internal/source/gitlab"
)

func TestConfiguredKeysIncludeDisabledSinks(t *testing.T) {
	cfg, err := config.Parse([]byte(`
sources:
  - {name: github, secretEnv: T_GH}
subscribers:
  - id: kevin
    identities: {github: [kevinle128]}
    sinks:
      - {name: omb, type: webhook, url: "http://omb.test/hook"}
      - {name: paused, type: webhook, url: "http://omb.test/other", enabled: false}
  - id: lan
    enabled: false
    identities: {github: [lan]}
    sinks:
      - {name: hermes, type: webhook, url: "http://hermes.test/hook"}
`))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"kevin/omb", "kevin/paused", "lan/hermes"}, configuredKeys(cfg))
}

func TestCheckRefusesAnOMBSinkWithoutItsProviderFilter(t *testing.T) {
	cfg, err := config.Parse([]byte(`
sources:
  - {name: github, secretEnv: T_GH}
subscribers:
  - id: kevin
    identities: {github: [kevinle128]}
    sinks:
      - {name: omb-github, type: omb, connector: github-event, url: "https://omb.test/events/github-event", secretEnv: T_OMB}
`))
	require.NoError(t, err)
	getenv := func(string) string { return "set" }
	err = Check(getenv)(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "filter.providers: [github]")
}

func TestCheckAcceptsTheExampleConfig(t *testing.T) {
	cfg, err := config.Load("../../config/example.yaml")
	require.NoError(t, err)
	require.NoError(t, Check(func(string) string { return "ci" })(cfg))
}

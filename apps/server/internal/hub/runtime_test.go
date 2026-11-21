package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/config"
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

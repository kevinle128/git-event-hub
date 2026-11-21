package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopedConfig is a hub with one GitLab source that is limited by repos.
const scopedConfig = `
sources:
  - {name: gitlab, secretEnv: T_GL, repos: [%REPOS%]}
api: {tokenEnv: T_API}
server: {maxBodyBytes: 2000000}
defaults:
  commentAuthors: ["*[bot]", "ocr-review-bot"]
  reviewAuthors: ["kevinle128-ocr[bot]", "ocr-review-bot"]
subscribers:
  - id: kevin
    identities: {gitlab: [kevin.le]}
    sinks:
      - {name: omb, type: test}
`

func eventCount(t *testing.T, h *testHub) int {
	t.Helper()
	var page struct{ Items []struct{} }
	require.Equal(t, http.StatusOK, h.get("/api/events", apiToken, &page))
	return len(page.Items)
}

func TestEventsOutsideTheSourceReposAreDropped(t *testing.T) {
	h := newHub(t, strings.Replace(scopedConfig, "%REPOS%", `"other/**"`, 1))

	opened := send(h, "gitlab/merge_request.open.json")
	assert.Equal(t, http.StatusAccepted, opened.Status)
	assert.Equal(t, 0, opened.Accepted)
	assert.Equal(t, 1, opened.Dropped)
	assert.Zero(t, eventCount(t, h), "an event outside the scope must not reach the outbox")
	assert.Equal(t, uint64(1), h.hub.Metrics().Value("hub_dropped_total", "source", "gitlab", "reason", "repo_out_of_source_scope"))

	// The forged merge request must not have planted an author: a later
	// note of the same project finds no pull request in the memory.
	note := send(h, "gitlab/note.mr-ocr-summary.json")
	assert.Equal(t, http.StatusAccepted, note.Status)
	assert.Equal(t, 0, note.Accepted)
	assert.Zero(t, eventCount(t, h))
}

func TestEventsInsideTheSourceReposAreQueued(t *testing.T) {
	h := newHub(t, strings.Replace(scopedConfig, "%REPOS%", `"vtc/**"`, 1))

	opened := send(h, "gitlab/merge_request.open.json")
	assert.Equal(t, http.StatusAccepted, opened.Status)
	assert.Equal(t, 1, opened.Accepted)
	assert.Equal(t, 1, eventCount(t, h))

	note := send(h, "gitlab/note.mr-ocr-summary.json")
	assert.Equal(t, 1, note.Accepted, "the memory of the in-scope merge request finds its author")
}

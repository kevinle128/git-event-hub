package httpapi_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnknownSourceIsRefusedBeforeTheBody(t *testing.T) {
	h := newHub(t, teamConfig)
	// 2,000,000 is maxBodyBytes in teamConfig, so this body is over the limit.
	body := bytes.Repeat([]byte("x"), 2_000_001)
	r := h.post("/hooks/nope", http.Header{}, body)
	assert.Equal(t, http.StatusNotFound, r.Status, "an unknown source must be refused before the body is read")
	assert.Equal(t, "unknown source", r.Error)

	var metrics string
	require.Equal(t, http.StatusOK, h.get("/metrics", apiToken, &metrics))
	assert.NotContains(t, metrics, `source="nope"`)

	// A configured source still answers 413 for the same body.
	assert.Equal(t, http.StatusRequestEntityTooLarge, h.post("/hooks/gitlab", http.Header{}, body).Status)
}

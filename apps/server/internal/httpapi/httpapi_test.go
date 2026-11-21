package httpapi_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git-event-hub/internal/testutil"
)

func TestSignatures(t *testing.T) {
	h := newHub(t, teamConfig)
	body := testutil.Fixture(t, "github/pull_request.opened.json")

	hd := http.Header{}
	hd.Set("X-GitHub-Event", "pull_request")
	hd.Set("X-GitHub-Delivery", "d1")
	assert.Equal(t, http.StatusUnauthorized, h.post("/hooks/github", hd, body).Status, "no signature")
	hd.Set("X-Hub-Signature-256", "sha256=00")
	assert.Equal(t, http.StatusUnauthorized, h.post("/hooks/github", hd, body).Status, "wrong signature")
	hd.Set("X-Hub-Signature-256", signGitHub(append([]byte(" "), body...)))
	assert.Equal(t, http.StatusUnauthorized, h.post("/hooks/github", hd, body).Status, "signature of other bytes")

	gl := http.Header{}
	gl.Set("X-Gitlab-Token", "wrong")
	assert.Equal(t, http.StatusUnauthorized, h.post("/hooks/gitlab", gl, testutil.Fixture(t, "gitlab/merge_request.open.json")).Status)

	assert.Equal(t, uint64(1), h.hub.Metrics().Value("hub_refused_total", "source", "github", "reason", "no_signature"))
	assert.Equal(t, uint64(2), h.hub.Metrics().Value("hub_refused_total", "source", "github", "reason", "signature"))
	assert.Equal(t, http.StatusAccepted, h.github("pull_request", "d1", body).Status)
}

func TestDedupeByDeliveryID(t *testing.T) {
	h := newHub(t, teamConfig)
	body := testutil.Fixture(t, "github/pull_request.opened.json")
	first := h.github("pull_request", "same-id", body)
	assert.Equal(t, http.StatusAccepted, first.Status)
	assert.Equal(t, 1, first.Accepted)
	again := h.github("pull_request", "same-id", body)
	assert.Equal(t, http.StatusOK, again.Status)
	assert.True(t, again.Duplicate)
	assert.Equal(t, uint64(1), h.hub.Metrics().Value("hub_deduped_total", "source", "github"))

	f := fake(t, "kevin/omb")
	require.Eventually(t, func() bool { return len(f.Acked()) == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Len(t, f.Received(), 1, "a repeated delivery must reach the sink once")
}

func TestUnknownEventsAreAcknowledgedAndCounted(t *testing.T) {
	h := newHub(t, teamConfig)
	r := h.github("star", "s1", []byte(`{"action":"created"}`))
	assert.Equal(t, http.StatusAccepted, r.Status)
	assert.Equal(t, 1, r.Dropped)
	assert.Equal(t, uint64(1), h.hub.Metrics().Value("hub_dropped_total", "source", "github", "reason", "unsupported_event"))

	r = h.gitlab("g1", []byte(`{"object_kind":"push"}`))
	assert.Equal(t, http.StatusAccepted, r.Status)
	assert.Equal(t, uint64(1), h.hub.Metrics().Value("hub_dropped_total", "source", "gitlab", "reason", "unsupported_event"))

	r = h.github("ping", "p1", []byte(`{"zen":"hi"}`))
	assert.Equal(t, http.StatusAccepted, r.Status)
}

func TestBadRequests(t *testing.T) {
	h := newHub(t, teamConfig)
	assert.Equal(t, http.StatusBadRequest, h.github("pull_request", "b1", []byte(`{"action":"opened","pull_request":{}}`)).Status, "wrong shape")
	assert.Equal(t, http.StatusBadRequest, h.github("pull_request", "b2", []byte(`not json`)).Status)
	assert.Equal(t, http.StatusNotFound, h.post("/hooks/bitbucket", http.Header{}, []byte(`{}`)).Status)

	disabled := h.post("/hooks/old", http.Header{}, []byte(`{}`))
	assert.Equal(t, http.StatusAccepted, disabled.Status)
	assert.Equal(t, "source is disabled", disabled.Ignored)

	big := []byte(`{"pad":"` + strings.Repeat("x", 2_100_000) + `"}`)
	assert.Equal(t, http.StatusRequestEntityTooLarge, h.github("pull_request", "big", big).Status)

	hd := http.Header{}
	hd.Set("X-GitHub-Event", "pull_request")
	body := testutil.Fixture(t, "github/pull_request.opened.json")
	hd.Set("X-Hub-Signature-256", signGitHub(body))
	assert.Equal(t, http.StatusBadRequest, h.post("/hooks/github", hd, body).Status, "no delivery id")
}

func TestFanOutAndIsolationEndToEnd(t *testing.T) {
	h := newHub(t, teamConfig)
	omb, flaky, minh := fake(t, "kevin/omb"), fake(t, "kevin/flaky"), fake(t, "minh/hook")
	flaky.SetDown(true)

	require.Equal(t, http.StatusAccepted, send(h, "gitlab/merge_request.open.json").Status)
	require.Equal(t, http.StatusAccepted, send(h, "gitlab/merge_request.open-by-other-author.json").Status)
	require.Equal(t, http.StatusAccepted, send(h, "gitlab/pipeline.mr-failed.json").Status)

	require.Eventually(t, func() bool { return len(omb.Acked()) == 2 && len(minh.Acked()) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.True(t, minh.Acked()["gitlab:gitlab/merge_request.open-by-other-author.json:0"])
	for id := range omb.Acked() {
		assert.NotContains(t, id, "other-author", "kevin must not receive minh's merge request")
	}
	assert.Empty(t, flaky.Acked(), "the failing sink holds its events")

	var page struct {
		Items []struct {
			Event   struct{ ID string }
			Targets []struct{ Sink, State string }
		}
	}
	require.Equal(t, http.StatusOK, h.get("/api/events?subscriber=kevin", apiToken, &page))
	require.Len(t, page.Items, 2)
	for _, it := range page.Items {
		states := map[string]string{}
		for _, tg := range it.Targets {
			states[tg.Sink] = tg.State
		}
		assert.Equal(t, map[string]string{"omb": "done", "flaky": "pending"}, states, it.Event.ID)
	}

	var deliveries struct {
		Items []struct {
			Sink, Outcome, Error string
		}
		NextBefore *int64
	}
	require.Equal(t, http.StatusOK, h.get("/api/deliveries?subscriber=kevin&sink=flaky", apiToken, &deliveries))
	require.NotEmpty(t, deliveries.Items)
	assert.Equal(t, "retry", deliveries.Items[0].Outcome)
	assert.Contains(t, deliveries.Items[0].Error, "consumer is down")
	require.Equal(t, http.StatusOK, h.get("/api/deliveries?limit=1", apiToken, &deliveries))
	assert.Len(t, deliveries.Items, 1)
	assert.NotNil(t, deliveries.NextBefore)
	assert.Equal(t, http.StatusBadRequest, h.get("/api/deliveries?outcome=lost", apiToken, nil))
	for _, p := range []string{
		"/api/deliveries?limit=0", "/api/deliveries?limit=201", "/api/deliveries?before=0",
		"/api/events?number=0", "/api/events?number=-1", "/api/events?before=-1", "/api/events?limit=0",
	} {
		assert.Equal(t, http.StatusBadRequest, h.get(p, apiToken, nil), p)
	}
}

func TestReadEndpointsNeedTheToken(t *testing.T) {
	h := newHub(t, teamConfig)
	for _, p := range []string{"/api/events", "/api/deliveries", "/metrics"} {
		assert.Equal(t, http.StatusUnauthorized, h.get(p, "", nil), p)
		assert.Equal(t, http.StatusUnauthorized, h.get(p, "wrong", nil), p)
		assert.Equal(t, http.StatusOK, h.get(p, apiToken, nil), p)
	}
	var metrics string
	h.get("/metrics", apiToken, &metrics)
	assert.Contains(t, metrics, `hub_sink_healthy{sink="kevin/omb"} 1`)

	// An unset token closes the endpoints instead of opening them.
	h.env.unset("T_API")
	assert.Equal(t, http.StatusUnauthorized, h.get("/api/events", "", nil))
	assert.Equal(t, http.StatusUnauthorized, h.get("/api/events", apiToken, nil))
}

func TestHealthIsPublicAndCarriesNoDetail(t *testing.T) {
	h := newHub(t, teamConfig)
	var body string
	require.Equal(t, http.StatusOK, h.get("/healthz", "", &body))
	assert.Contains(t, body, `"status":"ok"`)
	assert.Contains(t, body, `"total":3`)
	assert.NotContains(t, body, "kevin")
}

func TestHotReloadAddsASubscriber(t *testing.T) {
	h := newHub(t, teamConfig)
	body := testutil.Fixture(t, "github/pull_request.opened.json")
	other := strings.ReplaceAll(string(body), `"login": "kevinle128"`, `"login": "newcomer"`)
	r := h.github("pull_request", "before", []byte(other))
	require.Equal(t, http.StatusAccepted, r.Status)
	assert.Equal(t, 0, r.Accepted, "nobody owns the PR yet")

	cfg := teamConfig + `  - id: newcomer
    identities: {github: [newcomer]}
    sinks: [{name: omb, type: test}]
`
	require.NoError(t, os.WriteFile(h.path, []byte(cfg), 0o600))
	require.Eventually(t, func() bool { return len(h.cfg.Current().Subscribers) == 3 }, 5*time.Second, 10*time.Millisecond)

	r = h.github("pull_request", "after", []byte(other))
	assert.Equal(t, 1, r.Accepted)
	f := fake(t, "newcomer/omb")
	require.Eventually(t, func() bool { return len(f.Acked()) == 1 }, 5*time.Second, 10*time.Millisecond)

	// A broken file keeps the running configuration.
	require.NoError(t, os.WriteFile(h.path, []byte("subscribers: ["), 0o600))
	require.Eventually(t, func() bool { return h.hub.Metrics().Value("hub_config_reload_failed_total") >= 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, h.github("pull_request", "broken", []byte(other)).Accepted)
}

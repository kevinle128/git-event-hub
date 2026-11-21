package httpapi_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"git-event-hub/internal/config"
	"git-event-hub/internal/httpapi"
	"git-event-hub/internal/hub"
	"git-event-hub/internal/metrics"
	"git-event-hub/internal/sink"
	"git-event-hub/internal/sink/sinktest"
	"git-event-hub/internal/store/storetest"

	_ "git-event-hub/internal/source/githubapp"
	_ "git-event-hub/internal/source/gitlab"
)

const (
	ghSecret = "gh-secret"
	glToken  = "gl-token"
	apiToken = "api-token"
)

// fakes holds one fake sink per queue name. The "test" sink type returns
// them, so a test can look at what each subscriber received.
var fakes = struct {
	sync.Mutex
	m map[string]*sinktest.Fake
}{m: map[string]*sinktest.Fake{}}

func init() {
	sink.Register("test", func(s sink.Spec) (sink.EventSink, error) {
		fakes.Lock()
		defer fakes.Unlock()
		f, ok := fakes.m[s.Name]
		if !ok {
			f = sinktest.NewFake()
			fakes.m[s.Name] = f
		}
		return f, nil
	})
}

const teamConfig = `
sources:
  - {name: github, secretEnv: T_GH}
  - {name: gitlab, secretEnv: T_GL}
  - {name: old, type: gitlab, secretEnv: T_GL, enabled: false}
api: {tokenEnv: T_API}
server: {maxBodyBytes: 2000000}
subscribers:
  - id: kevin
    identities: {github: [kevinle128], gitlab: [kevin.le]}
    sinks:
      - {name: omb, type: test}
      - {name: flaky, type: test, delivery: {backoff: {initial: 1h, max: 1h}}}
  - id: minh
    identities: {gitlab: [minh.tran]}
    sinks:
      - {name: hook, type: test}
`

// env is a fake environment that a test may change while the hub runs.
type env struct {
	mu sync.Mutex
	m  map[string]string
}

func (e *env) get(k string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.m[k]
}

func (e *env) unset(k string) {
	e.mu.Lock()
	delete(e.m, k)
	e.mu.Unlock()
}

type testHub struct {
	t    *testing.T
	hub  *hub.Hub
	srv  *httptest.Server
	cfg  *config.Store
	path string
	env  *env
}

func newHub(t *testing.T, yaml string) *testHub {
	t.Helper()
	resetFakes()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	e := &env{m: map[string]string{"T_GH": ghSecret, "T_GL": glToken, "T_API": apiToken}}
	cs, err := config.NewStore(path, hub.Check(e.get), zap.NewNop())
	require.NoError(t, err)
	h, err := hub.New(storetest.New(t).Store(), cs, metrics.New(), zap.NewNop(), e.get)
	require.NoError(t, err)
	h.SetPollInterval(10 * time.Millisecond)
	require.NoError(t, h.Start())
	t.Cleanup(h.Stop)
	srv := httptest.NewServer(httpapi.New(h, cs, e.get, zap.NewNop(), "test"))
	t.Cleanup(srv.Close)
	return &testHub{t: t, hub: h, srv: srv, cfg: cs, path: path, env: e}
}

// fake returns the fake sink of a queue name, waiting until the hub has
// built it.
func fake(t *testing.T, key string) *sinktest.Fake {
	t.Helper()
	var f *sinktest.Fake
	require.Eventually(t, func() bool {
		fakes.Lock()
		defer fakes.Unlock()
		f = fakes.m[key]
		return f != nil
	}, 5*time.Second, 5*time.Millisecond)
	return f
}

func resetFakes() {
	fakes.Lock()
	fakes.m = map[string]*sinktest.Fake{}
	fakes.Unlock()
}

func signGitHub(body []byte) string {
	m := hmac.New(sha256.New, []byte(ghSecret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type hookResult struct {
	Status    int
	Accepted  int    `json:"accepted"`
	Dropped   int    `json:"dropped"`
	Duplicate bool   `json:"duplicate"`
	Ignored   string `json:"ignored"`
	Error     string `json:"error"`
}

func (h *testHub) post(path string, header http.Header, body []byte) hookResult {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+path, bytes.NewReader(body))
	require.NoError(h.t, err)
	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer func() { _ = resp.Body.Close() }()
	var r hookResult
	_ = json.NewDecoder(resp.Body).Decode(&r)
	r.Status = resp.StatusCode
	return r
}

func (h *testHub) github(event, delivery string, body []byte) hookResult {
	hd := http.Header{}
	hd.Set("X-GitHub-Event", event)
	hd.Set("X-GitHub-Delivery", delivery)
	hd.Set("X-Hub-Signature-256", signGitHub(body))
	return h.post("/hooks/github", hd, body)
}

func (h *testHub) gitlab(delivery string, body []byte) hookResult {
	hd := http.Header{}
	hd.Set("X-Gitlab-Token", glToken)
	hd.Set("X-Gitlab-Event-UUID", delivery)
	return h.post("/hooks/gitlab", hd, body)
}

func (h *testHub) get(path, token string, out any) int {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	require.NoError(h.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if s, ok := out.(*string); ok {
			var b bytes.Buffer
			_, _ = b.ReadFrom(resp.Body)
			*s = b.String()
		} else {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
	}
	return resp.StatusCode
}

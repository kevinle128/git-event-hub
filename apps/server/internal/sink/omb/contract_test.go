package omb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	contractDir = "testdata/omb-contract"
	schemaID    = "https://openmausbot/schema/omb-event.v1.json"
)

var prTypes = []string{
	"pr.opened", "pr.pushed", "pr.ci.finished", "pr.review.finished",
	"pr.comment.created", "pr.conflict.detected", "pr.merged", "pr.closed",
}

func newCompiler(t testing.TB) *jsonschema.Compiler {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "omb-event.v1.json"))
	require.NoError(t, err)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	require.NoError(t, c.AddResource(schemaID, doc))
	return c
}

// checkOMB does what the OMB validator does for one connector: the envelope
// must pass the schema, the data must pass the schema of its type, and the
// type, connector, subject kind and forge must be the ones that the
// connector takes.
func checkOMB(t testing.TB, raw []byte, connector, forge string) error {
	t.Helper()
	c := newCompiler(t)
	root, err := c.Compile(schemaID)
	require.NoError(t, err)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if err := root.Validate(inst); err != nil {
		return fmt.Errorf("envelope: %w", err)
	}
	obj, ok := inst.(map[string]any)
	if !ok {
		return errors.New("event is not an object")
	}
	typ, _ := obj["type"].(string)
	if !slices.Contains(prTypes, typ) {
		return fmt.Errorf("type %q is not a pr type", typ)
	}
	payload, err := c.Compile(schemaID + "#/$defs/payloads/" + typ)
	if err != nil {
		return fmt.Errorf("no payload schema for %s: %w", typ, err)
	}
	if err := payload.Validate(obj["data"]); err != nil {
		return fmt.Errorf("data: %w", err)
	}
	src, _ := obj["source"].(map[string]any)
	if src["connector"] != connector {
		return fmt.Errorf("source.connector is %v, want %s", src["connector"], connector)
	}
	subj, _ := obj["subject"].(map[string]any)
	if subj["kind"] != "pr" || subj["forge"] != forge {
		return fmt.Errorf("subject is %v, want a %s pr", subj, forge)
	}
	return nil
}

func fixtureFiles(t testing.TB, kind string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(contractDir, "fixtures", kind, "*.json"))
	require.NoError(t, err)
	return files
}

func TestContractCheckerAcceptsEveryValidFixture(t *testing.T) {
	files := fixtureFiles(t, "valid")
	require.Len(t, files, 8)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			require.NoError(t, err)
			require.NoError(t, checkOMB(t, raw, "github-event", "github"))
		})
	}
}

func TestContractCheckerRefusesEveryInvalidFixture(t *testing.T) {
	files := fixtureFiles(t, "invalid")
	require.Len(t, files, 9)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			require.NoError(t, err)
			assert.Error(t, checkOMB(t, raw, "github-event", "github"))
		})
	}
}

// vendoredFiles lists the JSON files of the copy, relative to its folder.
func vendoredFiles(t testing.TB, dir string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return err
	}))
	slices.Sort(out)
	return out
}

func TestVendoredContractIsUnchanged(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(contractDir, "SOURCE.txt"))
	require.NoError(t, err)
	want := map[string]string{}
	for _, line := range strings.Split(string(src), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && len(f[0]) == 64 {
			want[strings.TrimPrefix(f[1], "./")] = f[0]
		}
	}
	files := vendoredFiles(t, contractDir)
	got := map[string]string{}
	for _, rel := range files {
		raw, err := os.ReadFile(filepath.Join(contractDir, rel))
		require.NoError(t, err)
		sum := sha256.Sum256(raw)
		got[rel] = hex.EncodeToString(sum[:])
	}
	assert.Equal(t, want, got, "the copy and SOURCE.txt differ: copy the contract again and update SOURCE.txt")
}

// TestVendoredContractMatchesOMBCheckout compares the copy with a checkout of
// OMB. CI cannot see that repository, so the test runs only when
// OMB_CONTRACT_DIR points to server/pr-orchestrator/contract of OMB.
func TestVendoredContractMatchesOMBCheckout(t *testing.T) {
	dir := os.Getenv("OMB_CONTRACT_DIR")
	if dir == "" {
		t.Skip("OMB_CONTRACT_DIR is not set")
	}
	theirs := map[string]string{"omb-event.v1.json": filepath.Join(dir, "omb-event.v1.json")}
	for _, kind := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(dir, "conformance", "fixtures", kind, "*.json"))
		require.NoError(t, err)
		for _, f := range files {
			theirs["fixtures/"+kind+"/"+filepath.Base(f)] = f
		}
	}
	ours := vendoredFiles(t, contractDir)
	for rel, path := range theirs {
		want, err := os.ReadFile(path)
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(contractDir, rel))
		if !assert.NoError(t, err, "OMB has %s and the hub copy lacks it", rel) {
			continue
		}
		assert.True(t, bytes.Equal(want, got), "%s differs from OMB", rel)
	}
	for _, rel := range ours {
		_, ok := theirs[rel]
		assert.True(t, ok, "the hub copy has %s and OMB does not", rel)
	}
}

package prevents

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realSummaryBody is the summary comment of a real pull request; it is the
// body that the review bot wrote, with its HTML and its machine block.
func realSummaryBody(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "github", "issue_comment.ocr-summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p.Comment.Body
}

func block(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	return "<!-- ocr-serve:v2 " + base64.StdEncoding.EncodeToString(b) + " -->"
}

const (
	sha1 = "ad6a6b62b4bdd5382d4ee1d57391275d0b30ccd9"
	sha2 = "cc69331d2561848178e7590936314434e9e3a6b0"
)

func TestParseOCRMarkerRealComment(t *testing.T) {
	m, err := ParseOCRMarker(realSummaryBody(t))
	if err != nil || m == nil {
		t.Fatalf("marker = %v, err = %v", m, err)
	}
	if m.Status != OCRSuccess || m.Max != 3 || len(m.Rounds) != 2 {
		t.Fatalf("unexpected marker %+v", m)
	}
	if got := m.ReviewedSHA(); got != "0bc93cc2e4122a993e4a73c4531beae821cddd67" {
		t.Errorf("reviewed sha = %s", got)
	}
	if m.ReviewStatus() != ReviewSuccess || len(m.Items()) != 0 {
		t.Errorf("status = %q items = %v", m.ReviewStatus(), m.Items())
	}
}

func TestParseOCRMarkerBlockOnlyNotHTML(t *testing.T) {
	// Text that looks like a status in the HTML must not change the result.
	body := "Status: the review chain ended; merge is allowed.\n⚠️ Needs you: 3\n" +
		block(t, map[string]any{"v": 2, "status": "failure", "max": 3, "rounds": []any{}})
	m, err := ParseOCRMarker(body)
	if err != nil || m.ReviewStatus() != ReviewFailed {
		t.Fatalf("status = %v err = %v", m, err)
	}
}

func TestParseOCRMarkerCases(t *testing.T) {
	round := func(real, fixed int) map[string]any {
		return map[string]any{"n": 1, "scope": "full", "from": sha1, "to": sha2, "real": real, "fixed": fixed, "fix": ""}
	}
	tests := []struct {
		name       string
		body       string
		wantNil    bool
		wantErr    string
		wantStatus string
	}{
		{name: "no block", body: "just a comment", wantNil: true},
		{name: "version 1 block holds no chain", body: "<!-- ocr-serve:v1 e30= -->", wantNil: true},
		{name: "unknown version", body: "<!-- ocr-serve:v9 e30= -->", wantErr: "unknown"},
		{name: "two blocks", body: block(t, map[string]any{"v": 2, "status": "success", "max": 1, "rounds": []any{}}) + block(t, map[string]any{"v": 2, "status": "success", "max": 1, "rounds": []any{}}), wantErr: "found 2"},
		{name: "mangled block", body: "<!-- ocr-serve:v2 !!!! -->", wantErr: "malformed"},
		{name: "not json", body: "<!-- ocr-serve:v2 " + base64.StdEncoding.EncodeToString([]byte("nope")) + " -->", wantErr: "not JSON"},
		{name: "bad status", body: block(t, map[string]any{"v": 2, "status": "maybe", "max": 1, "rounds": []any{}}), wantErr: "status"},
		{name: "bad sha", body: block(t, map[string]any{"v": 2, "status": "success", "max": 1, "rounds": []any{map[string]any{"n": 1, "scope": "full", "from": "", "to": "xyz", "real": 0, "fixed": 0, "fix": ""}}}), wantErr: "40-hex"},
		{name: "running", body: block(t, map[string]any{"v": 2, "status": "running", "max": 3, "rounds": []any{round(1, 0)}}), wantStatus: ""},
		{name: "success all fixed", body: block(t, map[string]any{"v": 2, "status": "success", "max": 3, "rounds": []any{round(2, 2)}}), wantStatus: ReviewSuccess},
		{name: "success with unfixed findings needs a person", body: block(t, map[string]any{"v": 2, "status": "success", "max": 3, "rounds": []any{round(2, 1)}}), wantStatus: ReviewNeedsPerson},
		{name: "failure", body: block(t, map[string]any{"v": 2, "status": "failure", "max": 3, "rounds": []any{round(0, 0)}}), wantStatus: ReviewFailed},
		{name: "more fixed than real", body: block(t, map[string]any{"v": 2, "status": "success", "max": 3, "rounds": []any{round(1, 2)}}), wantErr: "rounds[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseOCRMarker(tt.body)
			switch {
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
			case tt.wantNil:
				if m != nil || err != nil {
					t.Fatalf("got %v, %v; want nil, nil", m, err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if got := m.ReviewStatus(); got != tt.wantStatus {
					t.Errorf("status = %q, want %q", got, tt.wantStatus)
				}
			}
		})
	}
}

func TestItemsListUnresolvedRounds(t *testing.T) {
	m := OCRMarker{Status: OCRSuccess, Rounds: []OCRRound{{N: 1, Scope: "full", Real: 3, Fixed: 1}, {N: 2, Scope: "commit", Real: 1, Fixed: 1}}}
	items := m.Items()
	if len(items) != 1 || items[0].Round != 1 || items[0].Unresolved != 2 {
		t.Fatalf("items = %+v", items)
	}
}

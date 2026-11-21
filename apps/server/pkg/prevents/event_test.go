package prevents

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"git-event-hub/schema"
)

func sample(kind Kind, data any) Event {
	s := "abc"
	return Event{V: 1, ID: "github:d1:0", Provider: GitHub, Repo: "o/r", Number: 1, Kind: kind,
		HeadSHA: &s, OccurredAt: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC), Data: data}
}

func allSamples() []Event {
	return []Event{
		sample(KindOpened, OpenedData{Title: "t", URL: "u"}),
		sample(KindPushed, PushedData{After: "a"}),
		sample(KindCIFinished, CIFinishedData{Source: "github-actions", Scope: ScopeWorkflow, Name: "CI", Status: CIFailed, FailedJobs: []FailedJob{{ID: "1", Name: "j", URL: "u"}}}),
		sample(KindReviewFinished, ReviewFinishedData{Source: "ocr", Status: ReviewNeedsPerson, ReviewedSHA: "a", Items: []ReviewItem{{Round: 1, Scope: "full", Real: 2, Fixed: 1, Unresolved: 1}}}),
		sample(KindMerged, MergedData{}),
		sample(KindClosed, ClosedData{}),
		sample(KindConflict, ConflictData{State: "dirty"}),
		sample(KindComment, CommentData{CommentID: "1", Commenter: "bot[bot]", Body: "x"}),
	}
}

func compileSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema.PREventV1)))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("pr-event.v1.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("pr-event.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEventsRoundTripAndMatchSchema(t *testing.T) {
	sc := compileSchema(t)
	for _, e := range allSamples() {
		t.Run(string(e.Kind), func(t *testing.T) {
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
			if err := sc.Validate(inst); err != nil {
				t.Fatalf("schema: %v\n%s", err, raw)
			}
			var back Event
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			if err := back.Validate(); err != nil {
				t.Fatal(err)
			}
			raw2, _ := json.Marshal(back)
			if string(raw) != string(raw2) {
				t.Errorf("round trip changed the event:\n%s\n%s", raw, raw2)
			}
		})
	}
}

func TestSchemaRejectsWrongData(t *testing.T) {
	sc := compileSchema(t)
	e := sample(KindMerged, ClosedData{}) // data of another kind
	raw, _ := json.Marshal(e)
	inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	// merged needs mergeCommitSha; closed data lacks it.
	if err := sc.Validate(inst); err == nil {
		t.Fatal("schema accepted data of the wrong kind")
	}
	if err := e.Validate(); err == nil {
		t.Fatal("Validate accepted data of the wrong kind")
	}
}

func TestValidateRejects(t *testing.T) {
	good := sample(KindOpened, OpenedData{})
	tests := map[string]func(*Event){
		"version":  func(e *Event) { e.V = 2 },
		"id":       func(e *Event) { e.ID = "" },
		"provider": func(e *Event) { e.Provider = "svn" },
		"repo":     func(e *Event) { e.Repo = "" },
		"number":   func(e *Event) { e.Number = 0 },
		"time":     func(e *Event) { e.OccurredAt = time.Time{} },
	}
	for name, mutate := range tests {
		e := good
		mutate(&e)
		if e.Validate() == nil {
			t.Errorf("%s: Validate accepted the event", name)
		}
	}
}

func TestUnmarshalRejectsUnknownKindAndMissingData(t *testing.T) {
	var e Event
	if err := json.Unmarshal([]byte(`{"v":1,"kind":"exploded","data":{}}`), &e); err == nil {
		t.Error("unknown kind accepted")
	}
	if err := json.Unmarshal([]byte(`{"v":1,"kind":"opened"}`), &e); err == nil {
		t.Error("missing data accepted")
	}
}

func TestTypesFileListsEveryKindAndProvider(t *testing.T) {
	for _, k := range Kinds {
		if !strings.Contains(string(schema.PREventV1Types), `"`+string(k)+`"`) {
			t.Errorf("pr-event.v1.d.ts lacks kind %s", k)
		}
		if !strings.Contains(string(schema.PREventV1), `"`+string(k)+`"`) {
			t.Errorf("pr-event.v1.json lacks kind %s", k)
		}
	}
	for _, p := range Providers {
		if !strings.Contains(string(schema.PREventV1Types), `"`+string(p)+`"`) || !strings.Contains(string(schema.PREventV1), `"`+string(p)+`"`) {
			t.Errorf("a schema file lacks provider %s", p)
		}
	}
	// The embedded schema is the file on disk.
	disk, err := os.ReadFile("../../schema/pr-event.v1.json")
	if err != nil || string(disk) != string(schema.PREventV1) {
		t.Error("embedded schema differs from schema/pr-event.v1.json")
	}
}

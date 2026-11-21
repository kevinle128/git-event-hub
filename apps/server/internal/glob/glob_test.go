package glob

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"kevinle128/*", "kevinle128/git-event-hub", true},
		{"kevinle128/*", "Kevinle128/OMB-Gateway", true},
		{"kevinle128/*", "other/git-event-hub", false},
		{"kevinle128/*", "kevinle128/a/b", false},
		{"vtc/**", "vtc/commons/game-db-mcp", true},
		{"vtc/**", "vtc", false},
		{"*[bot]", "kevinle128-ocr[bot]", true},
		{"*[bot]", "kevinle128-ocr", false},
		{"a.b", "axb", false},
		{"*", "anything", true},
		{"*", "a/b", false},
		{"**", "a/b", true},
	}
	for _, tt := range tests {
		p, err := Compile(tt.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Match(tt.name); got != tt.want {
			t.Errorf("%q vs %q: got %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestCompileRejectsEmpty(t *testing.T) {
	if _, err := Compile(""); err == nil {
		t.Fatal("empty pattern must fail")
	}
}

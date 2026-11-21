package prevents

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// OCRMarker is the machine block that the review bot writes into its sticky
// summary comment: <!-- ocr-serve:v2 BASE64JSON -->. It holds the state of
// the review chain.
type OCRMarker struct {
	V      int        `json:"v"`
	Status string     `json:"status"` // running, success or failure
	Max    int        `json:"max"`
	Rounds []OCRRound `json:"rounds"`
}

// OCRRound is one round of the chain. From is empty for a commit round. Real
// counts the findings that the fix run judged real, and Fixed the ones it
// fixed. Fix is the fix commit that the round pushed, or empty.
type OCRRound struct {
	N     int    `json:"n"`
	Scope string `json:"scope"`
	From  string `json:"from"`
	To    string `json:"to"`
	Real  int    `json:"real"`
	Fixed int    `json:"fixed"`
	Fix   string `json:"fix"`
}

// Chain states of the marker.
const (
	OCRRunning = "running"
	OCRSuccess = "success"
	OCRFailure = "failure"
)

var (
	ocrMarkerRE = regexp.MustCompile(`^<!-- ocr-serve:v([0-9]+) ([A-Za-z0-9+/]+={0,2}) -->$`)
	ocrPrefixRE = regexp.MustCompile(`<!-- ocr-serve:v`)
	ocrFenceRE  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	ocrSHARE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// markerLines returns the lines of body that start with the block prefix,
// outside fenced code blocks. The bot writes its block on a line of its own;
// a block inside a line or a code block is quoted text, for example a diff
// of the pull request, and must not count.
func markerLines(body string) []string {
	var (
		lines []string
		fence string
	)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if m := ocrFenceRE.FindStringSubmatch(line); m != nil {
			rest := line[len(m[0]):]
			switch {
			// A backtick fence may not hold a backtick in its info string;
			// such a line is inline code, not a fence (CommonMark 4.5).
			case fence == "" && (m[1][0] != '`' || !strings.ContainsRune(rest, '`')):
				fence = m[1]
				continue
			case fence != "" && m[1][0] == fence[0] && len(m[1]) >= len(fence) && strings.TrimSpace(rest) == "":
				fence = ""
				continue
			}
		}
		if fence == "" && strings.HasPrefix(line, "<!-- ocr-serve:v") {
			lines = append(lines, line)
		}
	}
	return lines
}

// ParseOCRMarker reads the machine block of body. It returns nil and no
// error when the body has no block, or only a version 1 block that holds no
// chain. It returns an error when the body has more than one block, or the
// block is not valid. It never reads the HTML around the block. Only a block
// on a line of its own, outside code blocks, counts.
func ParseOCRMarker(body string) (*OCRMarker, error) {
	lines := markerLines(body)
	n := 0
	for _, line := range lines {
		n += len(ocrPrefixRE.FindAllStringIndex(line, -1))
	}
	switch {
	case n == 0:
		return nil, nil
	case n > 1:
		return nil, fmt.Errorf("found %d review blocks, want one", n)
	}
	m := ocrMarkerRE.FindStringSubmatch(lines[0])
	if m == nil {
		return nil, errors.New("review block is malformed")
	}
	switch m[1] {
	case "1":
		return nil, nil
	case "2":
	default:
		return nil, fmt.Errorf("review block version %s is unknown", m[1])
	}
	raw, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil {
		return nil, fmt.Errorf("review block is not base64: %w", err)
	}
	var out OCRMarker
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("review block is not JSON: %w", err)
	}
	if err := out.validate(); err != nil {
		return nil, fmt.Errorf("review block is invalid: %w", err)
	}
	return &out, nil
}

func (m OCRMarker) validate() error {
	if m.V != 2 {
		return fmt.Errorf("version %d", m.V)
	}
	switch m.Status {
	case OCRRunning, OCRSuccess, OCRFailure:
	default:
		return fmt.Errorf("status %q is unknown", m.Status)
	}
	if m.Max < 0 {
		return errors.New("negative round limit")
	}
	for i, r := range m.Rounds {
		if r.N < 1 || r.Real < 0 || r.Fixed < 0 {
			return fmt.Errorf("rounds[%d] has a negative or zero count", i)
		}
		if r.Scope != ReviewScopeFull && r.Scope != ReviewScopeCommit {
			return fmt.Errorf("rounds[%d].scope %q is unknown", i, r.Scope)
		}
		if r.From != "" && !ocrSHARE.MatchString(r.From) {
			return fmt.Errorf("rounds[%d].from is not a 40-hex SHA", i)
		}
		if r.Fix != "" && !ocrSHARE.MatchString(r.Fix) {
			return fmt.Errorf("rounds[%d].fix is not a 40-hex SHA", i)
		}
		if !ocrSHARE.MatchString(r.To) {
			return fmt.Errorf("rounds[%d].to is not a 40-hex SHA", i)
		}
	}
	return nil
}

// Finished reports whether the chain has ended.
func (m OCRMarker) Finished() bool { return m.Status != OCRRunning }

// ReviewedSHA is the last commit that the chain looked at: the target of the
// last round. It is empty when the chain has no round.
func (m OCRMarker) ReviewedSHA() string {
	if len(m.Rounds) == 0 {
		return ""
	}
	return m.Rounds[len(m.Rounds)-1].To
}

// Items lists the rounds that left real findings unfixed. The bot cannot
// fix these, so a person must decide.
func (m OCRMarker) Items() []ReviewItem {
	items := []ReviewItem{}
	for _, r := range m.Rounds {
		if r.Real > r.Fixed {
			items = append(items, ReviewItem{Round: r.N, Scope: r.Scope, Real: r.Real, Fixed: r.Fixed, Unresolved: r.Real - r.Fixed})
		}
	}
	return items
}

// ReviewStatus maps the chain state to the contract status: a failed chain
// is "failed", a finished chain with unresolved findings is "needs-person",
// and any other finished chain is "success". It returns "" while the chain
// runs.
func (m OCRMarker) ReviewStatus() string {
	switch {
	case !m.Finished():
		return ""
	case m.Status == OCRFailure:
		return ReviewFailed
	case len(m.Items()) > 0:
		return ReviewNeedsPerson
	}
	return ReviewSuccess
}

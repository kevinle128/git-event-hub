// Package glob matches names against simple patterns. "*" matches any run of
// characters except "/", "**" matches any run including "/". Matching ignores
// case. Every other character, "[" and "]" too, stands for itself.
package glob

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern is a compiled glob.
type Pattern struct {
	src string
	re  *regexp.Regexp
}

// Compile turns a glob into a Pattern.
func Compile(p string) (Pattern, error) {
	if p == "" {
		return Pattern{}, fmt.Errorf("empty pattern")
	}
	var b strings.Builder
	b.WriteString("(?is)^")
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case p[i] == '*':
			b.WriteString("[^/]*")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return Pattern{}, err
	}
	return Pattern{src: p, re: re}, nil
}

// String returns the source pattern.
func (p Pattern) String() string { return p.src }

// Match reports whether name matches the pattern.
func (p Pattern) Match(name string) bool { return p.re != nil && p.re.MatchString(name) }

// Set is a list of patterns.
type Set []Pattern

// CompileSet compiles every pattern, or fails on the first bad one.
func CompileSet(patterns []string) (Set, error) {
	out := make(Set, 0, len(patterns))
	for _, s := range patterns {
		p, err := Compile(s)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// Any reports whether name matches at least one pattern.
func (s Set) Any(name string) bool {
	for _, p := range s {
		if p.Match(name) {
			return true
		}
	}
	return false
}

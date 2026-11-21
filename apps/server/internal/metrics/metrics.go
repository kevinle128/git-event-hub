// Package metrics holds the counters of the hub and writes them in the
// Prometheus text format. Counters live in memory and start from zero when the
// process starts.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Registry is a set of labelled counters.
type Registry struct {
	mu       sync.Mutex
	counters map[string]map[string]uint64 // name -> label string -> value
}

// New returns an empty registry.
func New() *Registry { return &Registry{counters: map[string]map[string]uint64{}} }

// Inc adds one to the counter name with the given label pairs
// (key, value, key, value, ...).
func (r *Registry) Inc(name string, labels ...string) { r.Add(name, 1, labels...) }

// Add adds n to the counter name.
func (r *Registry) Add(name string, n uint64, labels ...string) {
	key := labelString(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.counters[name]
	if m == nil {
		m = map[string]uint64{}
		r.counters[name] = m
	}
	m[key] += n
}

// Value returns the current value of a counter.
func (r *Registry) Value(name string, labels ...string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[name][labelString(labels)]
}

// Gauge is a value that the caller computes when the metrics are read.
type Gauge struct {
	Name   string
	Labels []string
	Value  float64
}

// Write prints the counters and the gauges in the Prometheus text format.
func (r *Registry) Write(out io.Writer, gauges []Gauge) error {
	w := bufio.NewWriter(out)
	r.mu.Lock()
	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", n)
		keys := make([]string, 0, len(r.counters[n]))
		for k := range r.counters[n] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			_, _ = fmt.Fprintf(w, "%s%s %d\n", n, k, r.counters[n][k])
		}
	}
	r.mu.Unlock()
	sort.Slice(gauges, func(i, j int) bool {
		return gauges[i].Name+labelString(gauges[i].Labels) < gauges[j].Name+labelString(gauges[j].Labels)
	})
	last := ""
	for _, g := range gauges {
		if g.Name != last {
			_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", g.Name)
			last = g.Name
		}
		_, _ = fmt.Fprintf(w, "%s%s %g\n", g.Name, labelString(g.Labels), g.Value)
	}
	return w.Flush()
}

// labelEscaper escapes a label value as the text format 0.0.4 asks: only
// backslash, double quote and newline.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func labelString(labels []string) string {
	if len(labels) < 2 {
		return ""
	}
	parts := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, labels[i], labelEscaper.Replace(labels[i+1])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

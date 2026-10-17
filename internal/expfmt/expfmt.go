// Package expfmt writes metrics in the Prometheus text exposition format
// (version 0.0.4). It supports exactly what the exporter needs: gauges and
// counters with labels.
package expfmt

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the media type of the text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Metric types.
const (
	Gauge   = "gauge"
	Counter = "counter"
)

type label struct{ name, value string }

type sample struct {
	labels []label
	key    string // rendered labels, used for stable ordering and duplicate detection
	value  float64
}

type family struct {
	name, help, typ string
	samples         []sample
	index           map[string]int // sample key -> position in samples
}

// Set is an ordered collection of metric families.
type Set struct {
	fams  map[string]*family
	order []string
}

// New returns an empty Set.
func New() *Set { return &Set{fams: map[string]*family{}} }

// Add records one sample. labels alternate name, value. Adding a sample whose
// name and labels already exist replaces its value, so a family never holds
// duplicate series. Add panics on an odd label list or a family redeclared
// with a different type: both are programming errors.
func (s *Set) Add(name, help, typ string, value float64, labels ...string) {
	if len(labels)%2 != 0 {
		panic("expfmt: odd number of label arguments for " + name)
	}
	f, ok := s.fams[name]
	if !ok {
		f = &family{name: name, help: help, typ: typ, index: map[string]int{}}
		s.fams[name] = f
		s.order = append(s.order, name)
	} else if f.typ != typ {
		panic("expfmt: family " + name + " redeclared with a different type")
	}
	ls := make([]label, 0, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		ls = append(ls, label{labels[i], labels[i+1]})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].name < ls[j].name })
	key := renderLabels(ls)
	if i, dup := f.index[key]; dup {
		f.samples[i].value = value
		return
	}
	f.index[key] = len(f.samples)
	f.samples = append(f.samples, sample{labels: ls, key: key, value: value})
}

// Merge copies every sample of other into s.
func (s *Set) Merge(other *Set) {
	for _, name := range other.order {
		f := other.fams[name]
		for _, sm := range f.samples {
			flat := make([]string, 0, 2*len(sm.labels))
			for _, l := range sm.labels {
				flat = append(flat, l.name, l.value)
			}
			s.Add(f.name, f.help, f.typ, sm.value, flat...)
		}
	}
}

// WriteTo renders the set. Families are sorted by name and samples by label
// set, so the output is deterministic.
func (s *Set) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: bufio.NewWriter(w)}
	names := append([]string(nil), s.order...)
	sort.Strings(names)
	for _, name := range names {
		f := s.fams[name]
		fmt.Fprintf(cw, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.typ)
		samples := append([]sample(nil), f.samples...)
		sort.SliceStable(samples, func(i, j int) bool { return samples[i].key < samples[j].key })
		for _, sm := range samples {
			fmt.Fprintf(cw, "%s%s %s\n", f.name, sm.key, formatValue(sm.value))
		}
	}
	err := cw.w.Flush()
	if cw.err != nil {
		err = cw.err
	}
	return cw.n, err
}

type countingWriter struct {
	w   *bufio.Writer
	n   int64
	err error
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if err != nil && c.err == nil {
		c.err = err
	}
	return n, err
}

func renderLabels(ls []label) string {
	if len(ls) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteByte('{')
	for i, l := range ls {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(l.name)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(l.value))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

func escapeLabelValue(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func escapeHelp(h string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(h)
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

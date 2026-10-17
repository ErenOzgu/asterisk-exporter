package expfmt

import (
	"bytes"
	"math"
	"regexp"
	"strings"
	"testing"
)

func render(t *testing.T, s *Set) string {
	t.Helper()
	var b bytes.Buffer
	n, err := s.WriteTo(&b)
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != b.Len() {
		t.Fatalf("WriteTo reported %d bytes, wrote %d", n, b.Len())
	}
	return b.String()
}

func TestHelpAndTypeOncePerFamilyAndSortedSamples(t *testing.T) {
	s := New()
	s.Add("x_total", "Things.", Counter, 3, "b", "2", "a", "1")
	s.Add("x_total", "Things.", Counter, 4, "a", "1", "b", "1")
	s.Add("y", "Other.", Gauge, 1)
	got := render(t, s)
	want := `# HELP x_total Things.
# TYPE x_total counter
x_total{a="1",b="1"} 4
x_total{a="1",b="2"} 3
# HELP y Other.
# TYPE y gauge
y 1
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDuplicateSeriesReplaceValue(t *testing.T) {
	s := New()
	s.Add("g", "h", Gauge, 1, "k", "v")
	s.Add("g", "h", Gauge, 2, "k", "v")
	if got := render(t, s); strings.Count(got, "g{k=") != 1 || !strings.Contains(got, `g{k="v"} 2`) {
		t.Fatalf("duplicate series not collapsed:\n%s", got)
	}
}

func TestEscaping(t *testing.T) {
	s := New()
	s.Add("e", "line1\nline2 \\ end", Gauge, 1, "path", "a\\b\"c\nd")
	got := render(t, s)
	if !strings.Contains(got, `# HELP e line1\nline2 \\ end`) {
		t.Errorf("HELP not escaped:\n%s", got)
	}
	if !strings.Contains(got, `e{path="a\\b\"c\nd"} 1`) {
		t.Errorf("label value not escaped:\n%s", got)
	}
}

func TestSpecialValues(t *testing.T) {
	s := New()
	s.Add("n", "h", Gauge, math.NaN(), "k", "nan")
	s.Add("n", "h", Gauge, math.Inf(1), "k", "pinf")
	s.Add("n", "h", Gauge, math.Inf(-1), "k", "ninf")
	s.Add("n", "h", Gauge, 1.5e9, "k", "big")
	got := render(t, s)
	for _, want := range []string{`n{k="nan"} NaN`, `n{k="pinf"} +Inf`, `n{k="ninf"} -Inf`, `n{k="big"} 1.5e+09`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestMerge(t *testing.T) {
	a, b := New(), New()
	a.Add("m", "h", Gauge, 1, "k", "a")
	b.Add("m", "h", Gauge, 2, "k", "b")
	b.Add("only_b", "h", Gauge, 7)
	a.Merge(b)
	got := render(t, a)
	for _, want := range []string{`m{k="a"} 1`, `m{k="b"} 2`, `only_b 7`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestProgrammingErrorsPanic(t *testing.T) {
	for name, fn := range map[string]func(){
		"odd labels":   func() { New().Add("m", "h", Gauge, 1, "lonely") },
		"type changes": func() { s := New(); s.Add("m", "h", Gauge, 1); s.Add("m", "h", Counter, 1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			fn()
		}()
	}
}

// Every line of the output must be a comment or a well-formed sample.
func TestOutputIsWellFormed(t *testing.T) {
	s := New()
	s.Add("a_b", "h", Gauge, 1, "x", `we"ird`)
	s.Add("c", "h", Counter, 2)
	sample := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*",?)*\})? (NaN|[+-]Inf|-?[0-9.]+(e[+-]?[0-9]+)?)$`)
	for _, line := range strings.Split(strings.TrimSuffix(render(t, s), "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		if !sample.MatchString(line) {
			t.Errorf("malformed line: %q", line)
		}
	}
}

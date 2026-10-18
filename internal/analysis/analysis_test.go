package analysis

import (
	"strings"
	"testing"

	"github.com/vfarcic/inference-vote/internal/engines"
)

func race() Race {
	hit, acc := 0.7, 0.3
	return Race{
		Elapsed:   252,
		Optimized: Side{Config: "BBA", Serving: true, TokensPerSec: 260, P50: 46, P95: 85, Requests: 700, Correct: 77, Answered: 78, Evidence: &engines.Evidence{Engines: 2, MaxConcurrency: 17.03, PrefixHitRate: &hit}},
		Audience:  Side{Config: "ABB", Serving: true, TokensPerSec: 80, P50: 51, P95: 178, Requests: 260, Failures: 59, Correct: 96, Answered: 100, Evidence: &engines.Evidence{Engines: 2, MaxConcurrency: 2.79, Preemptions: 41, SpecAcceptance: &acc}},
	}
}

func TestRows(t *testing.T) {
	got := map[string]string{}
	for _, r := range Rows(race()) {
		got[r.Measure] = r.Winner
	}
	want := map[string]string{
		"Tokens per second":       "optimized",
		"Answer time, median":     "tie", // 46 vs 51 is under 10%
		"Answer time, slowest 5%": "optimized",
		"Failed requests":         "optimized",
		"Math accuracy":           "tie", // 99% vs 96%
	}
	for m, w := range want {
		if got[m] != w {
			t.Errorf("%s: winner %q, want %q", m, got[m], w)
		}
	}
	r := race()
	r.Audience.Serving = false
	if Rows(r) != nil {
		t.Error("rows for a deployment that never served")
	}
}

func TestPrompt(t *testing.T) {
	r := race()
	p := Prompt(r, Rows(r))
	for _, want := range []string{
		"4:12 (still running)",
		"Question 1 (quantization): optimized B",
		"your pick A",
		"(DIFFERENT)",
		"Question 3 (speculative decoding)",
		"Tokens per second: 260 vs 80 (optimized wins)",
		"Answer time, median: 46.0s vs 51.0s (tie)",
		"17.0 vs 2.8",
		"preemptions): 0 vs 41",
		"Guessed tokens accepted: n/a vs 30%",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if !strings.Contains(Prompt(Race{Optimized: Side{Config: "BBA"}, Audience: Side{Config: "AAA"}}, nil), "not available") {
		t.Error("missing evidence isn't said")
	}
}

func TestFallback(t *testing.T) {
	r := race()
	f := Fallback(r, Rows(r))
	if !strings.HasPrefix(f, "After 4:12: the optimized configuration won on tokens per second (260 vs 80)") || !strings.Contains(f, "Too close to call: answer time, median, math accuracy.") {
		t.Errorf("got %q", f)
	}
	if !strings.Contains(Fallback(r, nil), "never served") {
		t.Error("no rows should say the audience's pick never served")
	}
}

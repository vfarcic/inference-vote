package analysis

import (
	"strings"
	"testing"

	"github.com/vfarcic/inference-vote/internal/engines"
)

// A race where the pick (BBA) beats its opposite (AAB) and ties the optimized
// config (BAA), all three separate deployments.
func race() Race {
	hit, acc := 0.7, 0.3
	return Race{
		Elapsed: 252,
		Sides: []Side{
			{Name: "audience", Roles: []string{"audience"}, Config: "BBA", Serving: true, TokensPerSec: 260, P50: 46, P95: 85, Requests: 700, Seconds: 240, Correct: 77, Answered: 100, Evidence: &engines.Evidence{Engines: 2, MaxConcurrency: 17.03, PrefixHitRate: &hit}},
			{Name: "opposite", Roles: []string{"opposite"}, Config: "AAB", Serving: true, TokensPerSec: 80, P50: 51, P95: 178, Requests: 260, Failures: 59, Seconds: 240, Correct: 80, Answered: 100, Evidence: &engines.Evidence{Engines: 1, MaxConcurrency: 14.28, Preemptions: 41, SpecAcceptance: &acc}},
			{Name: "optimized", Roles: []string{"optimized"}, Config: "BAA", Serving: true, TokensPerSec: 270, P50: 44, P95: 80, Requests: 720, Seconds: 252, Correct: 78, Answered: 100, Evidence: &engines.Evidence{Engines: 1, MaxConcurrency: 23.93}},
		},
	}
}

func TestRows(t *testing.T) {
	got := map[string]string{}
	for _, r := range Rows(race()) {
		got[r.Measure] = r.Winner
	}
	want := map[string]string{
		"Requests completed per minute": "tie", // 175 vs 171 is under 10%
		"Tokens per second":             "tie", // 270 vs 260
		"Answer time, median":           "tie",
		"Answer time, slowest 5%":       "tie",
		"Failed requests":               "tie", // 0% vs 0%
		"Math accuracy":                 "tie", // 80% vs 78%
	}
	for m, w := range want {
		if got[m] != w {
			t.Errorf("%s: winner %q, want %q", m, got[m], w)
		}
	}
	// Without the optimized side, the pick beats its opposite outright.
	r := race()
	r.Sides = r.Sides[:2]
	got = map[string]string{}
	for _, row := range Rows(r) {
		got[row.Measure] = row.Winner
		if len(row.Values) != 2 {
			t.Errorf("%s: %d values", row.Measure, len(row.Values))
		}
	}
	for _, m := range []string{"Requests completed per minute", "Tokens per second", "Answer time, slowest 5%", "Failed requests"} {
		if got[m] != "audience" {
			t.Errorf("%s: winner %q, want audience", m, got[m])
		}
	}
	r.Sides[1].Serving = false
	if Rows(r) != nil {
		t.Error("rows with only one side serving")
	}
}

func TestStandings(t *testing.T) {
	st := Standings(race())
	if len(st) != 2 {
		t.Fatalf("got %d standings, want the pick against its opposite and against the optimized", len(st))
	}
	if st[0].Left != "audience" || st[0].Right != "opposite" || st[0].Winner != "audience" || st[0].LeftLabel != "Audience pick" {
		t.Errorf("pick vs opposite: %+v", st[0])
	}
	if st[1].Right != "optimized" || st[1].Winner != "tie" {
		t.Errorf("pick vs optimized: %+v", st[1])
	}
	// The pick is the optimized config: one deployment, both roles, one head-to-head.
	r := race()
	r.Sides = []Side{
		{Name: "optimized", Roles: []string{"audience", "optimized"}, Config: "BAA", Serving: true, Requests: 700, Seconds: 240},
		{Name: "opposite", Roles: []string{"opposite"}, Config: "ABB", Serving: true, Requests: 200, Seconds: 240},
	}
	st = Standings(r)
	if len(st) != 1 || st[0].Winner != "optimized" || st[0].LeftLabel != "Audience pick = Optimized" {
		t.Errorf("merged: %+v", st)
	}
	// Before the opposite serves, nobody is ahead yet.
	r.Sides[1].Serving = false
	if st = Standings(r); st[0].Winner != "" {
		t.Errorf("a winner against a side that isn't serving: %+v", st[0])
	}
}

func TestPrompt(t *testing.T) {
	r := race()
	p := Prompt(r, Rows(r))
	for _, want := range []string{
		"4:12 (still running)",
		"- The audience's pick: question 1 (quantization) B,",
		"- The opposite: question 1 (quantization) A,",
		"- The optimized configuration:",
		"Head to head, requests completed per minute: Audience pick 175 vs Opposite 50 (Audience pick wins).",
		"- Tokens per second: Audience pick 260, Opposite 80, Optimized 270 (tie)",
		"Opposite:\n- Engines: 1\n- Full-length conversations the KV cache can hold: 14.3 all engines together",
		"(preemptions): 41",
		"- Guessed tokens accepted: 30%",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if !strings.Contains(Prompt(Race{Sides: []Side{{Name: "optimized", Roles: []string{"optimized"}, Config: "BBA"}}}, nil), "not available") {
		t.Error("missing evidence isn't said")
	}
}

func TestPromptMerged(t *testing.T) {
	r := race()
	r.Sides = []Side{
		{Name: "optimized", Roles: []string{"audience", "optimized"}, Config: "BAA", Serving: true, Requests: 700, Seconds: 240},
		{Name: "opposite", Roles: []string{"opposite"}, Config: "ABB", Serving: true, Requests: 200, Seconds: 240},
	}
	p := Prompt(r, Rows(r))
	if !strings.Contains(p, "- The audience's pick, which is exactly the optimized configuration:") {
		t.Errorf("merged side not described:\n%s", p)
	}
}

func TestFallback(t *testing.T) {
	r := race()
	f := Fallback(r, Rows(r))
	if !strings.HasPrefix(f, "After 4:12: Audience pick beats opposite, 175 to 50 requests a minute. Audience pick and optimized are too close to call") {
		t.Errorf("got %q", f)
	}
	r.Sides = r.Sides[:2]
	if f := Fallback(r, Rows(r)); !strings.Contains(f, "Audience pick did best on requests completed per minute (175)") {
		t.Errorf("got %q", f)
	}
	if !strings.Contains(Fallback(r, nil), "nothing to compare") {
		t.Error("no rows should say there is nothing to compare")
	}
}

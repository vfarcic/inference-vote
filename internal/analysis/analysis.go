// Package analysis turns the race's numbers into results. The code decides
// who won each measure, so the table is always right; the optimized model
// explains why, from those numbers, the engines' own evidence, and a fixed
// description of what each answer does, so it reasons from correct premises
// instead of from memory.
package analysis

import (
	"fmt"
	"math"
	"strings"

	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/engines"
)

// Side is what one deployment measured.
type Side struct {
	Config       string
	Serving      bool
	TokensPerSec float64
	P50, P95     float64 // seconds
	Requests     int
	Failures     int
	Correct      int
	Answered     int
	Evidence     *engines.Evidence
}

// Race is both sides, after Elapsed seconds of the load test.
type Race struct {
	Elapsed   float64
	Final     bool
	Swapped   bool // the room picked the optimized config, so Audience is its opposite
	Optimized Side
	Audience  Side
}

// names are what the screen calls the two sides: when the room picked the
// optimized config, that is their pick, and the other is its opposite.
func (r Race) names() (optimized, audience string) {
	if r.Swapped {
		return "your pick", "unoptimized"
	}
	return "optimized", "your pick"
}

// Row is one measure, with the winner: "optimized", "audience", or "tie".
type Row struct {
	Measure   string `json:"measure"`
	Optimized string `json:"optimized"`
	Audience  string `json:"audience"`
	Winner    string `json:"winner"`
}

// Rows compares the measures that both sides have. A margin under 10% is a
// tie: one race can't tell a smaller difference from noise.
func Rows(r Race) []Row {
	o, a := r.Optimized, r.Audience
	if !a.Serving || a.Requests == 0 {
		return nil
	}
	rows := []Row{
		row("Tokens per second", o.TokensPerSec, a.TokensPerSec, "%.0f", true),
		row("Answer time, median", o.P50, a.P50, "%.1fs", false),
		row("Answer time, slowest 5%", o.P95, a.P95, "%.1fs", false),
	}
	of, af := failRate(o), failRate(a)
	fails := Row{Measure: "Failed requests", Optimized: fmt.Sprintf("%.1f%%", of), Audience: fmt.Sprintf("%.1f%%", af), Winner: "tie"}
	if math.Abs(of-af) >= 1 {
		fails.Winner = map[bool]string{true: "optimized", false: "audience"}[of < af]
	}
	rows = append(rows, fails)
	// Accuracy differences of a few points are noise on a few dozen problems.
	if o.Answered >= 20 && a.Answered >= 20 {
		oa, aa := 100*float64(o.Correct)/float64(o.Answered), 100*float64(a.Correct)/float64(a.Answered)
		acc := Row{Measure: "Math accuracy", Optimized: fmt.Sprintf("%.0f%%", oa), Audience: fmt.Sprintf("%.0f%%", aa), Winner: "tie"}
		if math.Abs(oa-aa) >= 5 {
			acc.Winner = map[bool]string{true: "optimized", false: "audience"}[oa > aa]
		}
		rows = append(rows, acc)
	}
	return rows
}

func row(measure string, o, a float64, format string, higherWins bool) Row {
	r := Row{Measure: measure, Optimized: fmt.Sprintf(format, o), Audience: fmt.Sprintf(format, a), Winner: "tie"}
	hi, lo := max(o, a), min(o, a)
	if hi > 0 && (hi-lo)/hi >= 0.1 {
		if (o > a) == higherWins {
			r.Winner = "optimized"
		} else {
			r.Winner = "audience"
		}
	}
	return r
}

func failRate(s Side) float64 {
	if s.Requests == 0 {
		return 0
	}
	return 100 * float64(s.Failures) / float64(s.Requests)
}

// Background is what each answer does, for the model to reason from. It
// states mechanisms, not outcomes: the outcome is whatever the race measured.
const Background = `What each answer does:

Question 1, precision. A keeps the model at full precision (BF16). On one 24 GB GPU the weights take about 16 GB, which leaves little room for the KV cache, the memory each conversation in progress needs. B uses FP8 weights and an FP8 KV cache: the weights take about 8 GB and every cached token half the space, so several times more conversations fit. When the KV cache is full, requests wait in the queue, or the engine evicts a conversation (a preemption) and recomputes it later. FP8 usually costs this model little accuracy.

Question 2, two GPUs. A splits one engine across both GPUs (tensor parallelism). Each GPU holds half the weights, so there is more room for the KV cache, but the two GPUs exchange results after every layer, over PCIe because these GPUs have no NVLink, which costs time on every token. B runs two independent copies, one per GPU, with nothing to coordinate. The gateway sends each request to one of them, preferring the copy that already has the conversation's opening in its prefix cache.

Question 3, writing the answer. A produces one token per step. B guesses the next few tokens by copying sequences that already appear in the conversation (n-gram speculation), and the model checks several guesses in one step. That pays off when most guesses are accepted and the GPU has spare compute. Under heavy load the GPU is already busy, so checking guesses that are rejected is wasted work.`

// Instructions tell the model how to write the analysis.
const Instructions = `You explain the result of a live inference race to a conference audience. The audience voted on three questions, and their answers were deployed as "your pick". It raced the "optimized" configuration on identical GPUs under the same load: 100 coding agents at once.

Write 4 to 6 short sentences of plain prose. No lists, headings, or markdown. Address the audience as "you".
- Start with the overall outcome: who served more and answered faster, with the numbers.
- Then explain why, using only the questions marked DIFFERENT. For each, name the engine evidence that shows its effect. Don't mention questions with the same answer at all: they can't explain a difference.
- Use only the numbers given. Never invent one.
- A measure marked as a tie is a tie, even if the numbers differ a little. Don't call a winner on it.
- If the evidence for a cause is missing or small, say the effect was too small to see rather than guessing.
- Mention accuracy only if one side is clearly worse.
- Three answers changed at once, so one race can't fully separate their effects. Where the evidence doesn't single one out, say so.

` + Background

// Prompt is the user message: the configurations, the table, and the evidence.
func Prompt(r Race, rows []Row) string {
	var b strings.Builder
	state := "still running"
	if r.Final {
		state = "finished"
	}
	fmt.Fprintf(&b, "The load test has run for %s (%s).\n\n", clock(r.Elapsed), state)
	on, an := r.names()
	if r.Swapped {
		b.WriteString(`This time the room voted exactly the optimized configuration, so it raced the opposite of every answer instead. Call the two sides "your pick" and "the unoptimized configuration", tell the room they matched the expert, and explain what each flipped answer cost.` + "\n\n")
	}
	oc, _ := choice.Parse(r.Optimized.Config)
	ac, _ := choice.Parse(r.Audience.Config)
	for i, q := range choice.Questions {
		same := "same answer"
		if oc[i] != ac[i] {
			same = "DIFFERENT"
		}
		fmt.Fprintf(&b, "Question %d (%s): %s %s, %s %s (%s).\n", i+1, q.Topic, on, answer(q, oc[i]), an, answer(q, ac[i]), same)
	}
	fmt.Fprintf(&b, "\nMeasured, %s vs %s:\n", on, an)
	for _, row := range rows {
		fmt.Fprintf(&b, "- %s: %s vs %s (%s)\n", row.Measure, row.Optimized, row.Audience, r.winner(row.Winner))
	}
	fmt.Fprintf(&b, "\nEngine evidence over the race, %s vs %s:\n", on, an)
	oe, ae := r.Optimized.Evidence, r.Audience.Evidence
	if oe == nil || ae == nil {
		b.WriteString("- not available\n")
		return b.String()
	}
	fmt.Fprintf(&b, "- Engines: %d vs %d\n", oe.Engines, ae.Engines)
	fmt.Fprintf(&b, "- Full-length conversations the KV cache can hold, all engines together: %.1f vs %.1f (per engine: %.1f vs %.1f)\n",
		oe.MaxConcurrency*float64(oe.Engines), ae.MaxConcurrency*float64(ae.Engines), oe.MaxConcurrency, ae.MaxConcurrency)
	fmt.Fprintf(&b, "- KV cache in use, average: %.0f%% vs %.0f%%; peak: %.0f%% vs %.0f%%\n", 100*oe.KVUsageAvg, 100*ae.KVUsageAvg, 100*oe.KVUsageMax, 100*ae.KVUsageMax)
	fmt.Fprintf(&b, "- Requests waiting in the queue, average: %.0f vs %.0f; peak: %.0f vs %.0f\n", oe.WaitingAvg, ae.WaitingAvg, oe.WaitingMax, ae.WaitingMax)
	fmt.Fprintf(&b, "- Conversations evicted for lack of KV cache (preemptions): %.0f vs %.0f\n", oe.Preemptions, ae.Preemptions)
	fmt.Fprintf(&b, "- Prompt tokens found in the prefix cache: %s vs %s\n", pct(oe.PrefixHitRate), pct(ae.PrefixHitRate))
	fmt.Fprintf(&b, "- Guessed tokens accepted: %s vs %s\n", pct(oe.SpecAcceptance), pct(ae.SpecAcceptance))
	return b.String()
}

// Fallback is the results in words, from the table alone, for when the model
// doesn't answer in time.
func Fallback(r Race, rows []Row) string {
	on, an := r.names()
	if len(rows) == 0 {
		if r.Swapped {
			return "You picked exactly the optimized configuration, and its opposite never served, so there is nothing to compare: you won by default."
		}
		return "Your pick never served, so there is nothing to compare: the optimized configuration won by default."
	}
	var wins, losses, ties []string
	for _, row := range rows {
		m := strings.ToLower(row.Measure)
		switch row.Winner {
		case "optimized":
			wins = append(wins, fmt.Sprintf("%s (%s vs %s)", m, row.Optimized, row.Audience))
		case "audience":
			losses = append(losses, fmt.Sprintf("%s (%s vs %s)", m, row.Audience, row.Optimized))
		default:
			ties = append(ties, m)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "After %s: ", clock(r.Elapsed))
	if len(wins) > 0 {
		fmt.Fprintf(&b, "%s won on %s. ", sideName(on), strings.Join(wins, ", "))
	}
	if len(losses) > 0 {
		fmt.Fprintf(&b, "%s won on %s. ", upper(sideName(an)), strings.Join(losses, ", "))
	}
	if len(ties) > 0 {
		fmt.Fprintf(&b, "Too close to call: %s.", strings.Join(ties, ", "))
	}
	return strings.TrimSpace(b.String())
}

func answer(q choice.Question, o choice.Option) string {
	if o == choice.A {
		return fmt.Sprintf("A, %q", q.A)
	}
	return fmt.Sprintf("B, %q", q.B)
}

func (r Race) winner(w string) string {
	on, an := r.names()
	switch w {
	case "optimized":
		return on + " wins"
	case "audience":
		return an + " wins"
	}
	return "tie"
}

// sideName is a side's name in a sentence: "the optimized configuration",
// "your pick".
func sideName(n string) string {
	if n == "your pick" {
		return n
	}
	return "the " + n + " configuration"
}

func upper(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

func pct(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100**v)
}

func clock(seconds float64) string {
	s := int(seconds)
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

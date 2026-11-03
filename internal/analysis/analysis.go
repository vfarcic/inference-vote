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

// Side is what one deployment measured, and what it is to the room.
type Side struct {
	Name         string   // the deployment: optimized, audience, or opposite
	Roles        []string // audience, opposite, optimized: two when the pick or its opposite is the optimized config
	Config       string
	Serving      bool
	TokensPerSec float64
	P50, P95     float64 // seconds
	Requests     int
	Failures     int
	Seconds      float64 // of load test on this side, which starts when it serves
	Correct      int
	Answered     int
	Evidence     *engines.Evidence
}

// Label is what the screen calls a side: "Audience pick", "Opposite",
// "Optimized", or two of them joined when one deployment is both.
func (s Side) Label() string { return Label(s.Roles) }

var roleLabels = map[string]string{"audience": "Audience pick", "opposite": "Opposite", "optimized": "Optimized"}

func Label(roles []string) string {
	parts := make([]string, len(roles))
	for i, r := range roles {
		parts[i] = roleLabels[r]
	}
	return strings.Join(parts, " = ")
}

// Race is every side, after Elapsed seconds of the load test.
type Race struct {
	Elapsed float64
	Final   bool
	Sides   []Side
}

func (r Race) side(role string) (Side, bool) {
	for _, s := range r.Sides {
		for _, x := range s.Roles {
			if x == role {
				return s, true
			}
		}
	}
	return Side{}, false
}

// Row is one measure: each side's value by deployment name, and the winner,
// a deployment name or "tie".
type Row struct {
	Measure string            `json:"measure"`
	Values  map[string]string `json:"values"`
	Winner  string            `json:"winner"`
}

// measured are the sides with load-test numbers to compare.
func (r Race) measured() []Side {
	var out []Side
	for _, s := range r.Sides {
		if s.Serving && s.Requests > 0 {
			out = append(out, s)
		}
	}
	return out
}

// Rows compares the measures that the measured sides have, when there are at
// least two. The winner must beat the runner-up by 10%: one race can't tell a
// smaller difference from noise.
func Rows(r Race) []Row {
	sides := r.measured()
	if len(sides) < 2 {
		return nil
	}
	pick := func(f func(Side) float64) []float64 {
		v := make([]float64, len(sides))
		for i, s := range sides {
			v[i] = f(s)
		}
		return v
	}
	rows := []Row{
		row("Requests completed per minute", sides, pick(perMinute), "%.0f", true, relative(0.1)),
		row("Tokens per second", sides, pick(func(s Side) float64 { return s.TokensPerSec }), "%.0f", true, relative(0.1)),
		row("Answer time, median", sides, pick(func(s Side) float64 { return s.P50 }), "%.1fs", false, relative(0.1)),
		row("Answer time, slowest 5%", sides, pick(func(s Side) float64 { return s.P95 }), "%.1fs", false, relative(0.1)),
		row("Failed requests", sides, pick(failRate), "%.1f%%", false, absolute(1)),
	}
	// Accuracy differences of a few points are noise on a few dozen problems.
	enough := true
	for _, s := range sides {
		enough = enough && s.Answered >= 20
	}
	if enough {
		rows = append(rows, row("Math accuracy", sides, pick(func(s Side) float64 { return 100 * float64(s.Correct) / float64(s.Answered) }), "%.0f%%", true, absolute(5)))
	}
	return rows
}

// perMinute is a side's completed requests per minute of its own load. Each
// side's load starts when it serves, so a rate, not a count, compares them
// fairly.
func perMinute(s Side) float64 { return RequestsPerMinute(s.Requests, s.Failures, s.Seconds) }

// RequestsPerMinute is completed requests per minute over seconds of load.
func RequestsPerMinute(requests, failures int, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return 60 * float64(requests-failures) / seconds
}

// A margin says whether the best value beats the runner-up by enough.
type margin func(best, next float64) bool

func relative(share float64) margin {
	return func(best, next float64) bool {
		hi, lo := max(best, next), min(best, next)
		return hi > 0 && (hi-lo)/hi >= share
	}
}

func absolute(points float64) margin {
	return func(best, next float64) bool { return math.Abs(best-next) >= points }
}

func row(measure string, sides []Side, values []float64, format string, higherWins bool, enough margin) Row {
	r := Row{Measure: measure, Values: map[string]string{}, Winner: "tie"}
	best, next := -1, -1
	better := func(a, b float64) bool { return (a > b) == higherWins && a != b }
	for i, v := range values {
		r.Values[sides[i].Name] = fmt.Sprintf(format, v)
		switch {
		case best < 0 || better(v, values[best]):
			best, next = i, best
		case next < 0 || better(v, values[next]):
			next = i
		}
	}
	if best >= 0 && next >= 0 && enough(values[best], values[next]) {
		r.Winner = sides[best].Name
	}
	return r
}

func failRate(s Side) float64 {
	if s.Requests == 0 {
		return 0
	}
	return 100 * float64(s.Failures) / float64(s.Requests)
}

// Standing is who is ahead in one head-to-head, by requests completed per
// minute: the one number the race screen calls a winner on.
type Standing struct {
	Left       string  `json:"left"` // deployment names
	Right      string  `json:"right"`
	LeftRate   float64 `json:"leftRate"`
	RightRate  float64 `json:"rightRate"`
	Winner     string  `json:"winner"` // a deployment name, "tie", or "" before both have numbers
	LeftLabel  string  `json:"leftLabel"`
	RightLabel string  `json:"rightLabel"`
}

// Standings are the head-to-heads that matter: the audience's pick against
// its opposite, which the room can win, and against the optimized config
// when that is a third deployment.
func Standings(r Race) []Standing {
	aud, ok1 := r.side("audience")
	opp, ok2 := r.side("opposite")
	if !ok1 || !ok2 {
		return nil
	}
	pairs := [][2]Side{{aud, opp}}
	if aud.Name != "optimized" && opp.Name != "optimized" {
		if o, ok := r.side("optimized"); ok {
			pairs = append(pairs, [2]Side{aud, o})
		}
	}
	var out []Standing
	for _, p := range pairs {
		st := Standing{Left: p[0].Name, Right: p[1].Name, LeftLabel: p[0].Label(), RightLabel: p[1].Label()}
		if p[0].Serving && p[1].Serving && p[0].Requests > 0 && p[1].Requests > 0 {
			st.LeftRate, st.RightRate = perMinute(p[0]), perMinute(p[1])
			st.Winner = "tie"
			if relative(0.1)(st.LeftRate, st.RightRate) {
				st.Winner = map[bool]string{true: p[0].Name, false: p[1].Name}[st.LeftRate > st.RightRate]
			}
		}
		out = append(out, st)
	}
	return out
}

// Background is what each answer does, for the model to reason from. It
// states mechanisms, not outcomes: the outcome is whatever the race measured.
const Background = `What each answer does:

Question 1, precision. A keeps the model at full precision (BF16). On one 24 GB GPU the weights take about 16 GB, which leaves little room for the KV cache, the memory each conversation in progress needs. B uses FP8 weights: they take about 8 GB, and the memory that frees goes to the KV cache, so about three times more conversations fit. The KV cache itself stays 16-bit in both. When the KV cache is full, requests wait in the queue, or the engine evicts a conversation (a preemption) and recomputes it later. FP8 usually costs this model little accuracy.

Question 2, two GPUs. A splits one engine across both GPUs (tensor parallelism). Each GPU holds half the weights, so there is more room for the KV cache, but the two GPUs exchange results after every layer, over PCIe because these GPUs have no NVLink, which costs time on every token. B runs two independent copies, one per GPU, with nothing to coordinate. The gateway sends each request to one of them, preferring the copy that already has the conversation's opening in its prefix cache.

Question 3, writing the answer. A produces one token per step. B guesses the next few tokens by copying sequences that already appear in the conversation (n-gram speculation), and the model checks several guesses in one step. That pays off when most guesses are accepted and the GPU has spare compute. Under heavy load the GPU is already busy, so checking guesses that are rejected is wasted work.`

// Instructions tell the model how to write the analysis.
const Instructions = `You explain the result of a live inference race to a conference audience. The audience voted on three questions, and their answers were deployed as "the audience's pick". It raced "the opposite": the same three questions with every answer flipped. It also raced "the optimized configuration", an expert's answers. All of them ran on identical GPUs under the same load: 100 coding agents at once. When the audience's pick or its opposite is exactly the optimized configuration, one deployment is both, and you are told so.

Write 4 to 6 short sentences of plain prose. No lists, headings, or markdown. Address the audience as "you".
- Start with the head-to-head the room can win: did the audience's pick beat its opposite on requests completed per minute? Give the numbers. Then say how it did against the optimized configuration, if that is a separate deployment.
- Then explain why, using the questions on which the two sides you compare answered differently. For each, name the engine evidence that shows its effect.
- Use only the numbers given. Never invent one.
- A measure marked as a tie is a tie, even if the numbers differ a little. Don't call a winner on it.
- If the evidence for a cause is missing or small, say the effect was too small to see rather than guessing.
- Mention accuracy only if one side is clearly worse.
- The pick and its opposite differ in all three answers at once, so one race can't fully separate their effects. Where the evidence doesn't single one out, say so.

` + Background

// Prompt is the user message: the configurations, the table, and the evidence.
func Prompt(r Race, rows []Row) string {
	var b strings.Builder
	state := "still running"
	if r.Final {
		state = "finished"
	}
	fmt.Fprintf(&b, "The load test has run for %s (%s).\n\n", clock(r.Elapsed), state)
	b.WriteString("The configurations:\n")
	for _, s := range r.Sides {
		c, _ := choice.Parse(s.Config)
		fmt.Fprintf(&b, "- %s:", describe(s))
		for i, q := range choice.Questions {
			fmt.Fprintf(&b, " question %d (%s) %s;", i+1, q.Topic, answer(q, c[i]))
		}
		b.WriteString("\n")
	}
	for _, st := range Standings(r) {
		fmt.Fprintf(&b, "\nHead to head, requests completed per minute: %s %.0f vs %s %.0f (%s).", st.LeftLabel, st.LeftRate, st.RightLabel, st.RightRate, r.winner(st.Winner))
	}
	b.WriteString("\n\nMeasured:\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "- %s: ", row.Measure)
		var parts []string
		for _, s := range r.Sides {
			if v, ok := row.Values[s.Name]; ok {
				parts = append(parts, fmt.Sprintf("%s %s", s.Label(), v))
			}
		}
		fmt.Fprintf(&b, "%s (%s)\n", strings.Join(parts, ", "), r.winner(row.Winner))
	}
	b.WriteString("\nEngine evidence over the race:\n")
	found := false
	for _, s := range r.Sides {
		e := s.Evidence
		if e == nil {
			continue
		}
		found = true
		fmt.Fprintf(&b, "%s:\n", s.Label())
		fmt.Fprintf(&b, "- Engines: %d\n", e.Engines)
		fmt.Fprintf(&b, "- Full-length conversations the KV cache can hold: %.1f all engines together, %.1f per engine\n", e.MaxConcurrency*float64(e.Engines), e.MaxConcurrency)
		fmt.Fprintf(&b, "- KV cache in use: %.0f%% average, %.0f%% peak\n", 100*e.KVUsageAvg, 100*e.KVUsageMax)
		fmt.Fprintf(&b, "- Requests waiting in the queue: %.0f average, %.0f peak\n", e.WaitingAvg, e.WaitingMax)
		fmt.Fprintf(&b, "- Conversations evicted for lack of KV cache (preemptions): %.0f\n", e.Preemptions)
		fmt.Fprintf(&b, "- Prompt tokens found in the prefix cache: %s\n", pct(e.PrefixHitRate))
		fmt.Fprintf(&b, "- Guessed tokens accepted: %s\n", pct(e.SpecAcceptance))
	}
	if !found {
		b.WriteString("- not available\n")
	}
	return b.String()
}

// describe names a side in a sentence, saying when one deployment is two.
func describe(s Side) string {
	names := map[string]string{"audience": "the audience's pick", "opposite": "the opposite", "optimized": "the optimized configuration"}
	parts := make([]string, len(s.Roles))
	for i, r := range s.Roles {
		parts[i] = names[r]
	}
	if len(parts) == 1 {
		return upper(parts[0])
	}
	return upper(parts[0]) + ", which is exactly " + strings.Join(parts[1:], " and ")
}

// Fallback is the results in words, from the table alone, for when the model
// doesn't answer in time.
func Fallback(r Race, rows []Row) string {
	if len(rows) == 0 {
		return "Fewer than two of the models served, so there is nothing to compare yet."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "After %s: ", clock(r.Elapsed))
	for _, st := range Standings(r) {
		switch st.Winner {
		case "":
		case "tie":
			fmt.Fprintf(&b, "%s and %s are too close to call (%.0f vs %.0f requests a minute). ", st.LeftLabel, strings.ToLower(st.RightLabel), st.LeftRate, st.RightRate)
		case st.Left:
			fmt.Fprintf(&b, "%s beats %s, %.0f to %.0f requests a minute. ", st.LeftLabel, strings.ToLower(st.RightLabel), st.LeftRate, st.RightRate)
		default:
			fmt.Fprintf(&b, "%s beats %s, %.0f to %.0f requests a minute. ", st.RightLabel, strings.ToLower(st.LeftLabel), st.RightRate, st.LeftRate)
		}
	}
	won := map[string][]string{}
	var ties []string
	for _, row := range rows {
		m := strings.ToLower(row.Measure)
		if row.Winner == "tie" {
			ties = append(ties, m)
		} else {
			won[row.Winner] = append(won[row.Winner], fmt.Sprintf("%s (%s)", m, row.Values[row.Winner]))
		}
	}
	for _, s := range r.Sides {
		if w := won[s.Name]; len(w) > 0 {
			fmt.Fprintf(&b, "%s did best on %s. ", s.Label(), strings.Join(w, ", "))
		}
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
	for _, s := range r.Sides {
		if s.Name == w {
			return s.Label() + " wins"
		}
	}
	return "tie"
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

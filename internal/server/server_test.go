package server

import (
	"slices"
	"testing"
	"time"

	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/eval"
	"github.com/vfarcic/inference-vote/internal/load"
	"github.com/vfarcic/inference-vote/internal/poll"
)

func TestReasonFromLog(t *testing.T) {
	cases := map[string]string{
		// What full precision plus a draft model logged on one L4.
		"(EngineCore pid=193) INFO 10-17 00:39:40 [gpu_worker.py:563] Available KV cache memory: 0.25 GiB\n" +
			"(EngineCore pid=193) ERROR 10-17 00:39:40 [core.py:1349] EngineCore failed to start.\n" +
			"RuntimeError: Engine core initialization failed. See root cause above.": "not enough GPU memory left for the KV cache",
		"torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB": "ran out of GPU memory",
		"NCCL error: unhandled system error":                                     "the GPUs could not connect to each other (NCCL)",
		"INFO starting\nValueError: something else broke":                        "ValueError: something else broke",
		"INFO starting\nINFO stopping":                                           "crashed",
	}
	for logs, want := range cases {
		if got := ReasonFromLog(logs); got != want {
			t.Errorf("ReasonFromLog(%q) = %q, want %q", logs, got, want)
		}
	}
}

func TestRaceEnded(t *testing.T) {
	end := time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)
	done := &load.View{Done: true, Ended: end}
	running := &load.View{}
	finished := &eval.ScoreView{Done: true}
	side := func(serving bool, l *load.View, a *eval.ScoreView) raceSide {
		return raceSide{Serving: serving, Load: l, Accuracy: a}
	}
	cases := []struct {
		name string
		v    raceView
		over bool
	}{
		{"all done", raceView{Sides: []raceSide{side(true, done, finished), side(true, done, finished)}}, true},
		{"one still running", raceView{Sides: []raceSide{side(true, done, nil), side(true, running, nil)}}, false},
		{"accuracy still running", raceView{Sides: []raceSide{side(true, done, &eval.ScoreView{})}}, false},
		{"one never served", raceView{Sides: []raceSide{side(true, done, nil), side(false, nil, nil)}}, true},
		{"one serves but hasn't started", raceView{Sides: []raceSide{side(true, done, nil), side(true, nil, nil)}}, false},
		{"nothing started", raceView{Sides: []raceSide{side(true, nil, nil)}}, false},
	}
	for _, c := range cases {
		if got := raceEnded(c.v); (got != nil) != c.over {
			t.Errorf("%s: ended = %v, want over %v", c.name, got, c.over)
		}
	}
}

// The race's sides follow the votes: three deployments, or two when the pick
// or its opposite is exactly the optimized config.
func TestLineup(t *testing.T) {
	cases := []struct {
		optimized string
		votes     [3]choice.Option
		want      []string // label of each side, in order
		pairs     [][2]string
	}{
		{"BAA", [3]choice.Option{choice.B, choice.B, choice.A}, []string{"Audience pick", "Opposite", "Optimized"}, [][2]string{{"audience", "opposite"}, {"audience", "optimized"}}},
		{"BAA", [3]choice.Option{choice.B, choice.A, choice.A}, []string{"Audience pick = Optimized", "Opposite"}, [][2]string{{"optimized", "opposite"}}},
		{"BAA", [3]choice.Option{choice.A, choice.B, choice.B}, []string{"Audience pick", "Opposite = Optimized"}, [][2]string{{"audience", "optimized"}}},
	}
	for _, c := range cases {
		opt, _ := choice.Parse(c.optimized)
		p := poll.New(opt)
		p.Next()
		for q, o := range c.votes {
			p.Vote("x", q, o)
			p.Next()
		}
		p.Next() // the race
		st := p.Snapshot()
		var got []string
		for _, side := range lineup(st) {
			got = append(got, side.Label)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%v: sides %v, want %v", c.votes, got, c.want)
		}
		if gp := pairs(st); !slices.Equal(gp, c.pairs) {
			t.Errorf("%v: pairs %v, want %v", c.votes, gp, c.pairs)
		}
		if side := lineup(st)[0]; side.Serving != (side.Name == "optimized") {
			t.Errorf("%v: %s serving %v before its gateway answered", c.votes, side.Name, side.Serving)
		}
	}
}

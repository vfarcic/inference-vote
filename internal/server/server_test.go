package server

import (
	"testing"
	"time"

	"github.com/vfarcic/inference-vote/internal/eval"
	"github.com/vfarcic/inference-vote/internal/load"
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
	cases := []struct {
		name    string
		v       raceView
		serving bool
		over    bool
	}{
		{"both done", raceView{Optimized: raceSide{Load: done, Accuracy: finished}, Audience: raceSide{Load: done, Accuracy: finished}}, true, true},
		{"one still running", raceView{Optimized: raceSide{Load: done}, Audience: raceSide{Load: running}}, true, false},
		{"accuracy still running", raceView{Optimized: raceSide{Load: done, Accuracy: &eval.ScoreView{}}}, false, false},
		{"audience never served", raceView{Optimized: raceSide{Load: done}}, false, true},
		{"audience serves but hasn't started", raceView{Optimized: raceSide{Load: done}}, true, false},
	}
	for _, c := range cases {
		if got := raceEnded(c.v, c.serving); (got != nil) != c.over {
			t.Errorf("%s: ended = %v, want over %v", c.name, got, c.over)
		}
	}
}

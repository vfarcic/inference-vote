package server

import "testing"

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

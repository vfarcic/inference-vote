package engines

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sample = `# HELP vllm:kv_cache_usage_perc KV-cache usage.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{engine="0",model_name="ml-team/optimized"} 0.5
vllm:num_requests_waiting{engine="0",model_name="ml-team/optimized"} 7.0
vllm:num_requests_waiting_by_reason{engine="0",model_name="ml-team/optimized",reason="capacity"} 3.0
vllm:num_preemptions_total{engine="0",model_name="ml-team/optimized"} %d
vllm:prefix_cache_queries_total{engine="0",model_name="ml-team/optimized"} %d
vllm:prefix_cache_hits_total{engine="0",model_name="ml-team/optimized"} %d
vllm:cache_config_info{block_size="16",cache_dtype="fp8",kv_cache_max_concurrency="17.029296875",num_gpu_blocks="8719"} 1.0
`

func TestParse(t *testing.T) {
	m := parse(strings.NewReader(fmt.Sprintf(sample, 2, 1000, 600)))
	for k, want := range map[string]float64{
		"vllm:kv_cache_usage_perc":            0.5,
		"vllm:num_requests_waiting":           7,
		"vllm:num_preemptions_total":          2,
		"vllm:prefix_cache_queries_total":     1000,
		"kv_cache_max_concurrency":            17.029296875,
		"vllm:num_requests_waiting_by_reason": 3,
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
}

func TestTrackerEvidence(t *testing.T) {
	round := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Counters grow between readings: 5 preemptions and 1000 lookups,
		// 800 of them hits, per engine.
		fmt.Fprintf(w, sample, 2+5*round, 1000+1000*round, 600+800*round)
	}))
	defer srv.Close()
	eps := []Endpoint{{"optimized", srv.URL}, {"optimized", srv.URL}}
	tr := &Tracker{first: map[string]Reading{}, last: map[string]Reading{}, sums: map[string]*sums{}}
	for round = 0; round < 2; round++ {
		tr.add(Read(context.Background(), srv.Client(), eps))
	}
	e := tr.Evidence("optimized")
	if e == nil || e.Engines != 2 || e.Samples != 2 || e.MaxConcurrency != 17.029296875 || e.KVUsageAvg != 0.5 || e.WaitingMax != 14 {
		t.Fatalf("got %+v", e)
	}
	if e.Preemptions != 10 || e.PrefixHitRate == nil || *e.PrefixHitRate != 0.8 || e.SpecAcceptance != nil {
		t.Errorf("got preemptions %v, hit rate %v, acceptance %v", e.Preemptions, e.PrefixHitRate, e.SpecAcceptance)
	}
	if tr.Evidence("audience") != nil {
		t.Error("evidence for a deployment never read")
	}
}

// Package engines reads what the vLLM engines say about themselves during the
// race: how full their KV cache is, how many requests wait, how many
// conversations they evicted, how much of each prompt they found cached, and
// how many guessed tokens they accepted. The race screen's numbers say who
// won; these say why, one per question.
package engines

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reading is one deployment's metrics at one moment, summed or averaged over
// its engines.
type Reading struct {
	Engines        int
	KVUsage        float64 // 0-1, averaged over engines
	Waiting        float64 // requests queued, all engines
	Running        float64 // requests being served, all engines
	Preemptions    float64 // counters, all engines, since each engine started
	PrefixHits     float64
	PrefixQueries  float64
	SpecAccepted   float64
	SpecDrafted    float64
	MaxConcurrency float64 // full-length conversations the KV cache holds, per engine
}

// Endpoint is an engine to read: its deployment and its metrics URL.
type Endpoint struct {
	Deployment string
	URL        string
}

// Read fetches every endpoint and combines them per deployment. An engine that
// doesn't answer is left out, so a crashed one doesn't stop the others.
func Read(ctx context.Context, client *http.Client, eps []Endpoint) map[string]Reading {
	out := map[string]Reading{}
	for _, ep := range eps {
		m, err := fetch(ctx, client, ep.URL)
		if err != nil {
			continue
		}
		r := out[ep.Deployment]
		r.Engines++
		r.KVUsage += m["vllm:kv_cache_usage_perc"]
		r.Waiting += m["vllm:num_requests_waiting"]
		r.Running += m["vllm:num_requests_running"]
		r.Preemptions += m["vllm:num_preemptions_total"]
		r.PrefixHits += m["vllm:prefix_cache_hits_total"]
		r.PrefixQueries += m["vllm:prefix_cache_queries_total"]
		r.SpecAccepted += m["vllm:spec_decode_num_accepted_tokens_total"]
		r.SpecDrafted += m["vllm:spec_decode_num_draft_tokens_total"]
		r.MaxConcurrency += m["kv_cache_max_concurrency"]
		out[ep.Deployment] = r
	}
	for d, r := range out {
		r.KVUsage /= float64(r.Engines)
		r.MaxConcurrency /= float64(r.Engines)
		out[d] = r
	}
	return out
}

func fetch(ctx context.Context, client *http.Client, url string) (map[string]float64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %d", url, resp.StatusCode)
	}
	return parse(resp.Body), nil
}

var maxConcurrency = regexp.MustCompile(`kv_cache_max_concurrency="([0-9.]+)"`)

// parse reads Prometheus text, summing a metric's series (one per engine
// process). The KV cache's capacity is a label on cache_config_info.
func parse(r io.Reader) map[string]float64 {
	m := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		if strings.HasPrefix(line, "vllm:cache_config_info{") {
			if g := maxConcurrency.FindStringSubmatch(line); g != nil {
				v, _ := strconv.ParseFloat(g[1], 64)
				m["kv_cache_max_concurrency"] = v
			}
			continue
		}
		name, rest, _ := strings.Cut(line, "{")
		if i := strings.LastIndex(rest, "} "); i >= 0 {
			rest = rest[i+2:]
		} else {
			name, rest, _ = strings.Cut(line, " ")
		}
		v, err := strconv.ParseFloat(strings.Fields(rest)[0], 64)
		if err != nil || math.IsNaN(v) {
			continue
		}
		m[name] += v
	}
	return m
}

// Evidence is what one deployment's engines did over the race.
type Evidence struct {
	Engines        int      `json:"engines"`
	MaxConcurrency float64  `json:"maxConcurrency"` // full-length conversations each engine's KV cache holds
	KVUsageAvg     float64  `json:"kvUsageAvg"`     // 0-1
	KVUsageMax     float64  `json:"kvUsageMax"`
	WaitingAvg     float64  `json:"waitingAvg"` // requests queued, all engines
	WaitingMax     float64  `json:"waitingMax"`
	Preemptions    float64  `json:"preemptions"`    // conversations evicted for lack of KV cache
	PrefixHitRate  *float64 `json:"prefixHitRate"`  // 0-1, of prompt tokens found cached
	SpecAcceptance *float64 `json:"specAcceptance"` // 0-1, of guessed tokens accepted; nil without guessing
	Samples        int      `json:"samples"`
}

// Tracker samples the engines during the race and keeps what Evidence needs:
// the first reading, to turn counters into what happened since, and running
// sums and peaks of the gauges.
type Tracker struct {
	mu    sync.Mutex
	first map[string]Reading
	last  map[string]Reading
	sums  map[string]*sums
}

type sums struct {
	n                  int
	kv, kvMax          float64
	waiting, waitingMx float64
}

// Track samples every interval until ctx ends. endpoints is called on every
// sample, so engines that come up during the race are picked up.
func Track(ctx context.Context, interval time.Duration, endpoints func(context.Context) []Endpoint) *Tracker {
	t := &Tracker{first: map[string]Reading{}, last: map[string]Reading{}, sums: map[string]*sums{}}
	client := &http.Client{Timeout: 5 * time.Second}
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			t.add(Read(ctx, client, endpoints(ctx)))
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return t
}

func (t *Tracker) add(readings map[string]Reading) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for d, r := range readings {
		if _, ok := t.first[d]; !ok {
			t.first[d] = r
			t.sums[d] = &sums{}
		}
		t.last[d] = r
		s := t.sums[d]
		s.n++
		s.kv += r.KVUsage
		s.kvMax = max(s.kvMax, r.KVUsage)
		s.waiting += r.Waiting
		s.waitingMx = max(s.waitingMx, r.Waiting)
	}
}

// Evidence summarizes a deployment so far, or nil before its first reading.
func (t *Tracker) Evidence(deployment string) *Evidence {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	first, ok := t.first[deployment]
	if !ok {
		return nil
	}
	last, s := t.last[deployment], t.sums[deployment]
	// Counters only grow, unless an engine restarted; then what it did before
	// is lost, and the difference is clamped rather than negative.
	delta := func(a, b float64) float64 { return max(b-a, 0) }
	e := &Evidence{
		Engines:        last.Engines,
		MaxConcurrency: last.MaxConcurrency,
		KVUsageAvg:     s.kv / float64(s.n),
		KVUsageMax:     s.kvMax,
		WaitingAvg:     s.waiting / float64(s.n),
		WaitingMax:     s.waitingMx,
		Preemptions:    delta(first.Preemptions, last.Preemptions),
		Samples:        s.n,
	}
	if q := delta(first.PrefixQueries, last.PrefixQueries); q > 0 {
		r := delta(first.PrefixHits, last.PrefixHits) / q
		e.PrefixHitRate = &r
	}
	if d := delta(first.SpecDrafted, last.SpecDrafted); d > 0 {
		r := delta(first.SpecAccepted, last.SpecAccepted) / d
		e.SpecAcceptance = &r
	}
	return e
}

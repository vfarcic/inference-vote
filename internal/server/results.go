package server

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/vfarcic/inference-vote/internal/analysis"
	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/engines"
	"github.com/vfarcic/inference-vote/internal/gateway"
)

// Results are the race's numbers at one moment, the winner of each, and the
// optimized model's explanation of why. The presenter asks for them whenever
// time runs short, as often as they like; they also come on their own once
// every test has finished.
type results struct {
	mu       sync.Mutex
	At       time.Time                    `json:"at"`
	Elapsed  float64                      `json:"elapsed"` // seconds of load test they rest on
	Final    bool                         `json:"final"`
	Rows     []analysis.Row               `json:"rows"`
	Evidence map[string]*engines.Evidence `json:"evidence"`
	Text     string                       `json:"text"`
	Source   string                       `json:"source"` // writing, model, or fallback
}

func (r *results) view() *results {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &results{At: r.At, Elapsed: r.Elapsed, Final: r.Final, Rows: r.Rows, Evidence: r.Evidence, Text: r.Text, Source: r.Source}
}

// How long the model gets: to start writing, and to finish. Under the load
// test, its first token can take most of a minute.
const (
	firstTokenWait = 90 * time.Second
	writingWait    = 3 * time.Minute
)

// showResults is the presenter's "Show results".
func (s *Server) showResults(w http.ResponseWriter, r *http.Request) {
	if !tokenOK(bearer(r), s.cfg.PresenterToken) {
		http.Error(w, "presenter token required", http.StatusUnauthorized)
		return
	}
	v, ok := s.raceView()
	if !ok {
		http.Error(w, "results come with the race", http.StatusConflict)
		return
	}
	s.analyze(v, false)
	w.WriteHeader(http.StatusAccepted)
}

// analyze replaces the race's results with new ones from v, and has the
// optimized model explain them in the background. A newer request cancels an
// older one that is still writing.
func (s *Server) analyze(v raceView, final bool) {
	a := analysis.Race{Final: final, Swapped: v.Swapped, Optimized: side(v.Optimized), Audience: side(v.Audience)}
	if v.Optimized.Load != nil {
		a.Elapsed = v.Optimized.Load.Seconds
	}
	s.raceMu.Lock()
	if s.race == nil {
		s.raceMu.Unlock()
		return
	}
	a.Optimized.Evidence = s.race.engines.Evidence("optimized")
	a.Audience.Evidence = s.race.engines.Evidence("audience")
	rows := analysis.Rows(a)
	res := &results{At: time.Now(), Elapsed: a.Elapsed, Final: final, Rows: rows, Source: "writing",
		Evidence: map[string]*engines.Evidence{"optimized": a.Optimized.Evidence, "audience": a.Audience.Evidence}}
	if s.race.cancelResults != nil {
		s.race.cancelResults()
	}
	ctx, cancel := context.WithTimeout(s.race.ctx, writingWait)
	s.race.results, s.race.cancelResults = res, cancel
	s.raceMu.Unlock()

	if len(rows) == 0 || s.cfg.GatewayKey == "" {
		res.mu.Lock()
		res.Text, res.Source = analysis.Fallback(a, rows), "fallback"
		res.mu.Unlock()
		cancel()
		return
	}
	go func() {
		defer cancel()
		// No first token in time means the model is too busy: cut it short.
		started := make(chan struct{})
		go func() {
			select {
			case <-started:
			case <-time.After(firstTokenWait):
				cancel()
			case <-ctx.Done():
			}
		}()
		var once sync.Once
		_, err := gateway.ChatStream(ctx, http.DefaultClient, s.cfg.GatewayURL, s.cfg.GatewayKey, map[string]any{
			"model":       choice.Namespace + "/optimized",
			"messages":    []map[string]string{{"role": "system", "content": analysis.Instructions}, {"role": "user", "content": analysis.Prompt(a, rows)}},
			"max_tokens":  400,
			"temperature": 0.3,
		}, func(d string) {
			once.Do(func() { close(started) })
			res.mu.Lock()
			res.Text += d
			res.mu.Unlock()
		})
		res.mu.Lock()
		defer res.mu.Unlock()
		switch {
		case err == nil && res.Text != "":
			res.Source = "model"
		case res.Text != "" && ctx.Err() == context.DeadlineExceeded:
			res.Source = "model" // cut short, but what it wrote stands
		default:
			if err != nil {
				log.Printf("results: the optimized model didn't explain them: %v", err)
			}
			res.Text, res.Source = analysis.Fallback(a, rows), "fallback"
		}
	}()
}

func side(rs raceSide) analysis.Side {
	sd := analysis.Side{Config: rs.Config, Serving: rs.Serving}
	if l := rs.Load; l != nil {
		sd.TokensPerSec, sd.P50, sd.P95, sd.Requests, sd.Failures = l.TokensPerSec, l.P50, l.P95, l.Requests, l.Failures
	}
	if acc := rs.Accuracy; acc != nil {
		sd.Correct, sd.Answered = acc.Correct, acc.Answered
	}
	return sd
}

// engineEndpoints lists both deployments' engines, for the evidence.
func (s *Server) engineEndpoints(ctx context.Context) []engines.Endpoint {
	if s.kube == nil {
		return nil
	}
	var list podList
	if err := s.kube.List(ctx, "/api/v1/pods", "modelplane.ai/engine,modelplane.ai/deployment in (audience,optimized)", &list); err != nil {
		log.Printf("listing engines for evidence: %v", err)
		return nil
	}
	var eps []engines.Endpoint
	for _, p := range list.Items {
		if p.Status.PodIP != "" {
			eps = append(eps, engines.Endpoint{Deployment: p.Metadata.Labels["modelplane.ai/deployment"], URL: "http://" + p.Status.PodIP + ":8000/metrics"})
		}
	}
	return eps
}

// finalResults produces the results once every test has finished, so the
// race ends with them even if nobody asks.
func (s *Server) finalResults(ctx context.Context, session int) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		v, ok := s.raceView()
		if !ok || v.Ended == nil {
			continue
		}
		s.raceMu.Lock()
		current := s.race != nil && s.race.session == session
		s.raceMu.Unlock()
		if current {
			s.analyze(v, true)
		}
		return
	}
}

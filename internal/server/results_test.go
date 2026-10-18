package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vfarcic/inference-vote/internal/load"
)

func raceForResults(gatewayURL string) (*Server, raceView) {
	s := &Server{cfg: Config{GatewayURL: gatewayURL, GatewayKey: "k"}}
	ctx, cancel := context.WithCancel(context.Background())
	s.race = &race{session: 1, ctx: ctx, cancel: cancel}
	v := raceView{
		Optimized: raceSide{Config: "BBA", Serving: true, Load: &load.View{Seconds: 200, Requests: 500, TokensPerSec: 260, P50: 40, P95: 80}},
		Audience:  raceSide{Config: "ABB", Serving: true, Load: &load.View{Seconds: 200, Requests: 200, Failures: 40, TokensPerSec: 90, P50: 50, P95: 170}},
	}
	return s, v
}

func waitForResults(t *testing.T, s *Server) *results {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r := s.race.results.view(); r.Source != "writing" {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the results are still being written")
	return nil
}

func TestResultsFromTheModel(t *testing.T) {
	var body string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		for _, part := range []string{"The optimized ", "configuration won."} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer gw.Close()
	s, v := raceForResults(gw.URL)
	s.analyze(v, false)
	r := waitForResults(t, s)
	if r.Source != "model" || r.Text != "The optimized configuration won." || r.Elapsed != 200 || len(r.Rows) == 0 {
		t.Errorf("got %+v", r)
	}
	if !strings.Contains(body, "ml-team/optimized") {
		t.Errorf("asked %s, not the optimized model", body)
	}
}

func TestResultsFallBackWhenTheModelFails(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
	}))
	defer gw.Close()
	s, v := raceForResults(gw.URL)
	s.analyze(v, true)
	r := waitForResults(t, s)
	if r.Source != "fallback" || !r.Final || !strings.HasPrefix(r.Text, "After 3:20: the optimized configuration won on tokens per second (260 vs 90)") {
		t.Errorf("got %+v", r)
	}
}

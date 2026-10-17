package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/diff"
	"github.com/vfarcic/inference-vote/internal/eval"
	"github.com/vfarcic/inference-vote/internal/load"
	"github.com/vfarcic/inference-vote/internal/poll"
)

// The race is the last screen. When the presenter starts it, the load test and
// the accuracy test start against every deployment that serves; one that comes
// up later joins when it does. Each deployment gets the same workload for the
// same length of time, and every number on the screen is a rate or a ratio, so
// one that joins late still compares fairly, on fewer samples.
type race struct {
	session int
	ctx     context.Context
	cancel  context.CancelFunc
	targets map[string]*raceTarget
}

type raceTarget struct {
	name  string
	load  *load.Stats
	score *eval.Score
	chats int // messages the audience sent it during the race
}

var raceTargets = []string{"optimized", "audience"}

// RunRace starts and stops what the race runs, following the poll: started for
// a deployment once per session, stopped when the presenter resets.
func (s *Server) RunRace(ctx context.Context) {
	ch, stop := s.poll.Subscribe()
	defer stop()
	for {
		s.syncRace(ctx, s.poll.Snapshot())
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
	}
}

func (s *Server) syncRace(ctx context.Context, st poll.State) {
	s.raceMu.Lock()
	defer s.raceMu.Unlock()
	if s.race != nil && s.race.session != st.Session {
		s.race.cancel() // a reset: whatever the last session started stops
		s.race = nil
	}
	if st.Phase != poll.Race {
		return
	}
	if s.race == nil {
		rctx, cancel := context.WithCancel(ctx)
		s.race = &race{session: st.Session, ctx: rctx, cancel: cancel, targets: map[string]*raceTarget{}}
	}
	for _, target := range raceTargets {
		serving := target == "optimized" || st.Serving
		if _, started := s.race.targets[target]; started || !serving {
			continue
		}
		t := &raceTarget{name: target}
		if s.cfg.Load.Key != "" && s.cfg.Load.Agents > 0 {
			t.load = load.Start(s.race.ctx, s.cfg.Load, target)
		}
		if s.cfg.Eval.Key != "" && s.cfg.Eval.Problems > 0 {
			t.score = eval.Start(s.race.ctx, s.cfg.Eval, target)
		}
		s.race.targets[target] = t
		log.Printf("race: started %s (%d agents for %s, %d accuracy problems)", target, s.cfg.Load.Agents, s.cfg.Load.Duration, s.cfg.Eval.Problems)
		go logWhenDone(s.race.ctx, t)
	}
}

// logWhenDone writes a deployment's final numbers to the log, which outlives
// the screen.
func logWhenDone(ctx context.Context, t *raceTarget) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		loadDone := t.load == nil || t.load.View().Done
		scoreDone := t.score == nil || t.score.View().Done
		if loadDone && scoreDone {
			if t.load != nil {
				log.Printf("race: %s", t.load.View())
			}
			if t.score != nil {
				log.Printf("race: %s accuracy %s", t.name, t.score.View())
			}
			return
		}
	}
}

// countChat records an audience message to a deployment, if a race is on.
func (s *Server) countChat(target string) {
	s.raceMu.Lock()
	defer s.raceMu.Unlock()
	if s.race != nil {
		if t, ok := s.race.targets[target]; ok {
			t.chats++
		}
	}
}

type raceSide struct {
	Config   string          `json:"config"`
	Summary  []string        `json:"summary"`
	Serving  bool            `json:"serving"`
	Load     *load.View      `json:"load,omitempty"`
	Accuracy *eval.ScoreView `json:"accuracy,omitempty"`
	Chats    int             `json:"chats"`
	Pods     []poll.PodState `json:"pods,omitempty"` // the audience's, so a crash shows
}

type raceView struct {
	Started   time.Time  `json:"started"`
	Ended     *time.Time `json:"ended,omitempty"` // when the last test finished, once all have
	Optimized raceSide   `json:"optimized"`
	Audience  raceSide   `json:"audience"`
	Manifests []diff.Row `json:"manifests"` // optimized on the left, the audience's on the right
}

// raceState is what the presenter's race screen polls. It is empty outside the
// race, so nothing about the optimized deployment shows before it.
func (s *Server) raceState(w http.ResponseWriter, r *http.Request) {
	st := s.poll.Snapshot()
	if st.Phase != poll.Race {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	opt, _ := choice.Parse(st.Optimized)
	dep, _ := choice.Parse(st.Deployed)
	v := raceView{
		Started:   st.RaceStarted,
		Optimized: raceSide{Config: st.Optimized, Summary: opt.Describe(), Serving: true},
		Audience:  raceSide{Config: st.Deployed, Summary: dep.Describe(), Serving: st.Serving, Pods: st.Progress.Pods},
		Manifests: diff.Lines(
			strings.Split(choice.YAML("optimized", opt, s.cfg.Options), "\n"),
			strings.Split(choice.YAML("audience", dep, s.cfg.Options), "\n"),
		),
	}
	s.raceMu.Lock()
	if s.race != nil {
		for target, side := range map[string]*raceSide{"optimized": &v.Optimized, "audience": &v.Audience} {
			t, ok := s.race.targets[target]
			if !ok {
				continue
			}
			side.Chats = t.chats
			if t.load != nil {
				lv := t.load.View()
				side.Load = &lv
			}
			if t.score != nil {
				sv := t.score.View()
				side.Accuracy = &sv
			}
		}
	}
	s.raceMu.Unlock()
	v.Ended = raceEnded(v, st.Serving)
	writeJSON(w, http.StatusOK, v)
}

// raceEnded is when the last test finished, or nil while any still runs. A
// serving audience deployment whose tests haven't started yet means the race
// isn't over; one that never served doesn't hold it up.
func raceEnded(v raceView, audienceServing bool) *time.Time {
	if v.Optimized.Load == nil || (audienceServing && v.Audience.Load == nil) {
		return nil
	}
	var last time.Time
	for _, side := range []raceSide{v.Optimized, v.Audience} {
		if side.Load == nil {
			continue
		}
		if !side.Load.Done || (side.Accuracy != nil && !side.Accuracy.Done) {
			return nil
		}
		if side.Load.Ended.After(last) {
			last = side.Load.Ended
		}
	}
	return &last
}

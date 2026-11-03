package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vfarcic/inference-vote/internal/analysis"
	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/diff"
	"github.com/vfarcic/inference-vote/internal/engines"
	"github.com/vfarcic/inference-vote/internal/eval"
	"github.com/vfarcic/inference-vote/internal/load"
	"github.com/vfarcic/inference-vote/internal/poll"
)

// The race is the last screen. When the presenter starts it, the load test and
// the accuracy test start against every deployment that serves: the optimized
// one, the audience's pick, and its opposite. One that comes up later joins
// when it does. Each deployment gets the same workload, which runs until a
// reset, and every number on the screen is a rate or a ratio, so one that
// joins late still compares fairly, on fewer samples.
type race struct {
	session int
	ctx     context.Context
	cancel  context.CancelFunc
	targets map[string]*raceTarget
	engines *engines.Tracker // what the engines report, the evidence behind the results

	results       *results // nil until asked for, or until every test finished
	cancelResults context.CancelFunc
}

type raceTarget struct {
	name  string
	load  *load.Stats
	score *eval.Score
	chats int // messages the audience sent it during the race
}

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
		s.race.engines = engines.Track(rctx, 10*time.Second, s.engineEndpoints)
		go s.finalResults(rctx, st.Session)
	}
	for _, side := range lineup(st) {
		target := side.Name
		if _, started := s.race.targets[target]; started || !side.Serving {
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
		length := "until a reset"
		if s.cfg.Load.Duration > 0 {
			length = "for " + s.cfg.Load.Duration.String()
		}
		log.Printf("race: started %s (%d agents %s, %d accuracy problems)", target, s.cfg.Load.Agents, length, s.cfg.Eval.Problems)
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

// raceSide is one deployment on the race screen, and what it is to the room.
type raceSide struct {
	Name     string          `json:"name"`  // the deployment: optimized, audience, or opposite
	Roles    []string        `json:"roles"` // audience, opposite, optimized: two when one deployment is both
	Label    string          `json:"label"` // what the screen calls it
	Config   string          `json:"config"`
	Summary  []string        `json:"summary"`
	Serving  bool            `json:"serving"`
	Load     *load.View      `json:"load,omitempty"`
	Accuracy *eval.ScoreView `json:"accuracy,omitempty"`
	Chats    int             `json:"chats"`
	Pods     []poll.PodState `json:"pods,omitempty"` // the bridge's deployments', so a crash shows
}

// lineup is the race's sides: the audience's pick, its opposite, and the
// optimized config, one side per deployment. The pick or its opposite can be
// exactly the optimized config, and then one deployment plays both roles.
func lineup(st poll.State) []raceSide {
	var out []raceSide
	add := func(name, role, config string) {
		for i := range out {
			if out[i].Name == name {
				out[i].Roles = append(out[i].Roles, role)
				return
			}
		}
		c, _ := choice.Parse(config)
		out = append(out, raceSide{Name: name, Roles: []string{role}, Config: config, Summary: c.Describe()})
	}
	add(st.Serves(poll.AudienceName), "audience", st.Audience)
	add(st.Serves(poll.OppositeName), "opposite", st.Opposite)
	add("optimized", "optimized", st.Optimized)
	for i := range out {
		out[i].Label = analysis.Label(out[i].Roles)
		out[i].Serving = st.Serving(out[i].Name)
		if d, ok := st.Deployment(out[i].Name); ok {
			out[i].Pods = d.Progress.Pods
		}
	}
	return out
}

// pairs are the deployments the screen compares, left against right: the
// pick against its opposite, and against the optimized config when that is a
// third deployment.
func pairs(st poll.State) [][2]string {
	aud, opp := st.Serves(poll.AudienceName), st.Serves(poll.OppositeName)
	out := [][2]string{{aud, opp}}
	if aud != "optimized" && opp != "optimized" {
		out = append(out, [2]string{aud, "optimized"})
	}
	return out
}

// comparison is two deployments' manifests, line by line.
type comparison struct {
	Left  string     `json:"left"`
	Right string     `json:"right"`
	Rows  []diff.Row `json:"rows"`
}

type raceView struct {
	Started   time.Time           `json:"started"`
	Ended     *time.Time          `json:"ended,omitempty"` // when the last test finished, once all have
	Sides     []raceSide          `json:"sides"`
	Standings []analysis.Standing `json:"standings"` // who is ahead, right now
	Manifests []comparison        `json:"manifests"`
	Results   *results            `json:"results,omitempty"`
}

// raceState is what the presenter's race screen polls. It is empty outside the
// race, so nothing about the optimized deployment shows before it.
func (s *Server) raceState(w http.ResponseWriter, r *http.Request) {
	v, ok := s.raceView()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// raceView is the race as it stands, or false outside the race.
func (s *Server) raceView() (raceView, bool) {
	st := s.poll.Snapshot()
	if st.Phase != poll.Race {
		return raceView{}, false
	}
	v := raceView{Started: st.RaceStarted, Sides: lineup(st)}
	configs := map[string]string{}
	for _, side := range v.Sides {
		configs[side.Name] = side.Config
	}
	for _, p := range pairs(st) {
		l, _ := choice.Parse(configs[p[0]])
		r, _ := choice.Parse(configs[p[1]])
		v.Manifests = append(v.Manifests, comparison{Left: p[0], Right: p[1], Rows: diff.Lines(
			strings.Split(choice.YAML(p[0], l, s.cfg.Options), "\n"),
			strings.Split(choice.YAML(p[1], r, s.cfg.Options), "\n"),
		)})
	}
	s.raceMu.Lock()
	if s.race != nil {
		for i := range v.Sides {
			side := &v.Sides[i]
			t, ok := s.race.targets[side.Name]
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
		if s.race.results != nil {
			v.Results = s.race.results.view()
		}
	}
	s.raceMu.Unlock()
	v.Standings = analysis.Standings(raceOf(v, false))
	v.Ended = raceEnded(v)
	return v, true
}

// raceEnded is when the last test finished, or nil while any still runs. A
// serving deployment whose tests haven't started yet means the race isn't
// over; one that never served doesn't hold it up. A load test without an end
// never finishes, so neither does the race: it runs until a reset.
func raceEnded(v raceView) *time.Time {
	var last time.Time
	for _, side := range v.Sides {
		if !side.Serving {
			continue
		}
		if side.Load == nil || !side.Load.Done || (side.Accuracy != nil && !side.Accuracy.Done) {
			return nil
		}
		if side.Load.Ended.After(last) {
			last = side.Load.Ended
		}
	}
	if last.IsZero() {
		return nil
	}
	return &last
}

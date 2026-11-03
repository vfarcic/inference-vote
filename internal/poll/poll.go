// Package poll is the vote itself: which question is open, who voted for what,
// what won, and how far the winning configuration has got towards serving.
//
// It lives in memory. The talk is one session on a cluster that is created
// before it and destroyed after, so there is nothing worth persisting, and a
// restart mid-talk is better answered by re-running the votes than by restoring
// half a state.
package poll

import (
	"slices"
	"sync"
	"time"

	"github.com/vfarcic/inference-vote/internal/choice"
)

// Phase is where the session is. The presenter moves it forward; the audience
// only ever sees it.
type Phase string

const (
	Lobby     Phase = "lobby"     // QR code on screen, nothing open yet
	Voting    Phase = "voting"    // Question is open, its tally live on the presenter's screen
	Deploying Phase = "deploying" // The winning config and its opposite have been handed to the bridge
	Race      Phase = "race"      // Load test and accuracy run on whatever serves; the audience chats
)

// Progress is what the screen shows while a deployment comes up.
// The control plane half comes from the bridge, the pod half from the cluster the
// app runs on.
type Progress struct {
	Applied       bool       `json:"applied"`
	ReplicasReady int        `json:"replicasReady"`
	ReplicasTotal int        `json:"replicasTotal"`
	Conditions    []string   `json:"conditions,omitempty"`
	Pods          []PodState `json:"pods,omitempty"`
	ReportedAt    time.Time  `json:"reportedAt"`
}

// PodState is one engine pod, reduced to the step it is on.
type PodState struct {
	Name     string `json:"name"`
	Step     string `json:"step"`               // scheduling, pulling image, loading model, ready
	Restarts int    `json:"restarts,omitempty"` // how many times the engine crashed
	Reason   string `json:"reason,omitempty"`   // why it last crashed, from its log
}

// The deployments the bridge owns. The optimized one is the setup's.
const (
	AudienceName = "audience" // what the votes chose
	OppositeName = "opposite" // every answer flipped, so the room has something it can beat
)

// Deployment is one of the bridge's deployments in this session: the
// audience's pick, or its opposite. When either is the optimized config, it
// isn't deployed again: the optimized deployment serves it.
type Deployment struct {
	Name     string   `json:"name"` // audience or opposite
	Config   string   `json:"config"`
	Summary  []string `json:"summary"`
	Progress Progress `json:"progress"`
	// EnginesReady is every engine pod ready; Serving is the gateway answering
	// for the deployment, which Modelplane's route makes possible some minutes
	// later. Neither is a phase: the presenter can start the race before either,
	// or without them at all.
	EnginesReady bool `json:"enginesReady"`
	Serving      bool `json:"serving"`
}

// State is a snapshot, safe to serialize to anyone.
type State struct {
	Version   int               `json:"version"`
	Phase     Phase             `json:"phase"`
	Question  int               `json:"question"` // index into choice.Questions
	Questions []choice.Question `json:"questions"`
	Tally     [3][2]int         `json:"tally"`
	Winners   [3]choice.Option  `json:"winners"`
	Audience  string            `json:"audience,omitempty"` // what the votes chose
	Opposite  string            `json:"opposite,omitempty"` // every answer of it flipped
	Optimized string            `json:"optimized"`
	// Which of the two is the optimized config, already running, so not
	// deployed again. At most one is: they differ in every answer.
	AudienceIsOptimized bool         `json:"audienceIsOptimized"`
	OppositeIsOptimized bool         `json:"oppositeIsOptimized"`
	Deployments         []Deployment `json:"deployments,omitempty"` // in that order, without the one that is the optimized config
	Started             time.Time    `json:"started,omitempty"`     // when the votes were applied
	RaceStarted         time.Time    `json:"raceStarted,omitempty"`
	// Session changes on every reset, so whatever runs once per session (the
	// race) knows a new one has begun.
	Session int `json:"session"`
}

// Serves is the deployment that serves a role, "audience" or "opposite":
// its own, or the optimized one when it is the optimized config.
func (s State) Serves(role string) string {
	if (role == AudienceName && s.AudienceIsOptimized) || (role == OppositeName && s.OppositeIsOptimized) {
		return "optimized"
	}
	return role
}

// Deployment returns one of the session's deployments by name.
func (s State) Deployment(name string) (Deployment, bool) {
	for _, d := range s.Deployments {
		if d.Name == name {
			return d, true
		}
	}
	return Deployment{}, false
}

// Serving reports whether a deployment answers through the gateway. The
// optimized one always does: the setup waited for it.
func (s State) Serving(name string) bool {
	if name == "optimized" {
		return true
	}
	d, ok := s.Deployment(name)
	return ok && d.Serving
}

// Poll is safe for concurrent use.
type Poll struct {
	mu        sync.Mutex
	state     State
	votes     [3]map[string]choice.Option
	optimized choice.Config
	subs      map[chan struct{}]struct{}
}

func New(optimized choice.Config) *Poll {
	p := &Poll{optimized: optimized, subs: map[chan struct{}]struct{}{}}
	p.reset()
	return p
}

func (p *Poll) reset() {
	p.state = State{
		Version:   p.state.Version + 1,
		Session:   p.state.Session + 1,
		Phase:     Lobby,
		Questions: choice.Questions,
		Optimized: p.optimized.String(),
	}
	for i := range p.votes {
		p.votes[i] = map[string]choice.Option{}
	}
}

// Snapshot returns the current state. The deployments are copied: reports
// change them in place. What they hold, pods and conditions, is only ever
// replaced, so a shallow copy is enough.
func (p *Poll) Snapshot() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.state
	s.Deployments = slices.Clone(s.Deployments)
	return s
}

// VoteOf returns what a voter picked on a question, if anything.
func (p *Poll) VoteOf(voter string, q int) choice.Option {
	p.mu.Lock()
	defer p.mu.Unlock()
	if q < 0 || q >= len(p.votes) {
		return ""
	}
	return p.votes[q][voter]
}

// Subscribe returns a channel that receives a signal after every change, and a
// function to stop. Signals coalesce: a slow reader sees the latest state, not
// every intermediate one.
func (p *Poll) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

// changed must be called with the lock held.
func (p *Poll) changed() {
	p.state.Version++
	for ch := range p.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Vote records or replaces a voter's answer to the open question. Votes for any
// other question, or outside voting, are ignored rather than rejected: a phone
// that is a second behind should not show an error.
func (p *Poll) Vote(voter string, q int, o choice.Option) bool {
	if o != choice.A && o != choice.B {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Phase != Voting || q != p.state.Question {
		return false
	}
	p.votes[q][voter] = o
	p.tally(q)
	p.changed()
	return true
}

func (p *Poll) tally(q int) {
	var t [2]int
	for _, o := range p.votes[q] {
		if o == choice.A {
			t[0]++
		} else {
			t[1]++
		}
	}
	p.state.Tally[q] = t
}

// winner breaks a tie towards the answer the optimized config did not pick, so a
// split room still produces something worth comparing.
func (p *Poll) winner(q int) choice.Option {
	t := p.state.Tally[q]
	switch {
	case t[0] > t[1]:
		return choice.A
	case t[1] > t[0]:
		return choice.B
	case p.optimized[q] == choice.A:
		return choice.B
	default:
		return choice.A
	}
}

// Next moves the session one step forward: open the first question, close one
// and open the next, apply after the last, and start the race. It returns false
// when there is nothing left to advance to.
func (p *Poll) Next() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &p.state
	switch s.Phase {
	case Lobby:
		s.Phase, s.Question = Voting, 0
	case Voting:
		// One press closes the question and opens the next: the tally was on
		// the presenter's screen all along, so there is nothing left to reveal.
		s.Winners[s.Question] = p.winner(s.Question)
		if s.Question < len(choice.Questions)-1 {
			s.Question++
		} else {
			p.apply()
		}
	case Deploying:
		// Whether or not the audience's deployments serve yet: the talk's time
		// is fixed, a deployment's isn't.
		s.Phase, s.RaceStarted = Race, time.Now()
	default:
		return false
	}
	p.changed()
	return true
}

func (p *Poll) apply() {
	s := &p.state
	audience := choice.Config(s.Winners)
	opposite := audience.Opposite()
	s.Audience, s.Opposite = audience.String(), opposite.String()
	s.AudienceIsOptimized, s.OppositeIsOptimized = audience == p.optimized, opposite == p.optimized
	s.Deployments = nil
	for _, d := range []struct {
		name string
		c    choice.Config
	}{{AudienceName, audience}, {OppositeName, opposite}} {
		if d.c != p.optimized {
			s.Deployments = append(s.Deployments, Deployment{Name: d.name, Config: d.c.String(), Summary: d.c.Describe()})
		}
	}
	s.Phase = Deploying
	s.Started = time.Now()
}

// Reset starts the session over, dropping every vote. Whatever was deployed
// stays deployed until a new one is applied.
func (p *Poll) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reset()
	p.changed()
}

// Desired is what the bridge should apply, by deployment name, if anything yet.
func (p *Poll) Desired() (map[string]choice.Config, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Audience == "" {
		return nil, false
	}
	out := map[string]choice.Config{}
	for _, d := range p.state.Deployments {
		if c, err := choice.Parse(d.Config); err == nil {
			out[d.Name] = c
		}
	}
	return out, true
}

// deployment returns one of the session's deployments to change. It must be
// called with the lock held.
func (p *Poll) deployment(name string) *Deployment {
	for i := range p.state.Deployments {
		if p.state.Deployments[i].Name == name {
			return &p.state.Deployments[i]
		}
	}
	return nil
}

// ReportControlPlane records what the bridge saw on the control plane for a
// deployment. The bridge reports every few seconds; phones hear about it only
// when something in the report changed, since every change redraws their
// screens.
func (p *Poll) ReportControlPlane(name string, applied bool, ready, total int, conditions []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.deployment(name)
	if d == nil {
		return
	}
	pr := &d.Progress
	pr.ReportedAt = time.Now()
	if pr.Applied == applied && pr.ReplicasReady == ready && pr.ReplicasTotal == total && slices.Equal(pr.Conditions, conditions) {
		return
	}
	pr.Applied, pr.ReplicasReady, pr.ReplicasTotal, pr.Conditions = applied, ready, total, conditions
	p.promote(d)
	p.changed()
}

// ReportPods records a deployment's engine pods as the app's own cluster sees
// them: only those running its current config.
func (p *Poll) ReportPods(name string, pods []PodState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.deployment(name)
	if d == nil || slices.Equal(d.Progress.Pods, pods) {
		return
	}
	d.Progress.Pods = pods
	p.promote(d)
	p.changed()
}

// promote marks a deployment's engines ready once every one of its engine
// pods is. Only the pods count: the control plane says a replica is ready
// while engines of an earlier config still serve, and the pods reported are
// only those running the deployment's current config. Ready engines are not
// yet serving: that takes the gateway's route, which MarkServing confirms.
func (p *Poll) promote(d *Deployment) {
	if d.EnginesReady || (p.state.Phase != Deploying && p.state.Phase != Race) {
		return
	}
	ready := len(d.Progress.Pods) > 0
	for _, pod := range d.Progress.Pods {
		if pod.Step != "ready" {
			ready = false
		}
	}
	d.EnginesReady = ready
}

// MarkServing records that the gateway answered for a deployment. Once
// serving, it stays so for the session: a late crash shows in the pods, not
// by taking the race away.
func (p *Poll) MarkServing(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.deployment(name)
	if d == nil || d.Serving || !d.EnginesReady || (p.state.Phase != Deploying && p.state.Phase != Race) {
		return
	}
	d.Serving = true
	p.changed()
}

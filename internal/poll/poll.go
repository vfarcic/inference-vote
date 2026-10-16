// Package poll is the vote itself: which question is open, who voted for what,
// what won, and how far the winning configuration has got towards serving.
//
// It lives in memory. The talk is one session on a cluster that is created
// before it and destroyed after, so there is nothing worth persisting, and a
// restart mid-talk is better answered by re-running the votes than by restoring
// half a state.
package poll

import (
	"sync"
	"time"

	"github.com/vfarcic/inference-vote/internal/choice"
)

// Phase is where the session is. The presenter moves it forward; the audience
// only ever sees it.
type Phase string

const (
	Lobby     Phase = "lobby"     // QR code on screen, nothing open yet
	Voting    Phase = "voting"    // Question is open
	Results   Phase = "results"   // Question is closed, its tally on screen
	Deploying Phase = "deploying" // The winning config has been handed to the bridge
	Ready     Phase = "ready"     // It serves, and the audience can chat with both
)

// Progress is what the screen shows while the audience's configuration comes up.
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
	Name string `json:"name"`
	Step string `json:"step"` // scheduling, pulling image, loading model, ready
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
	Deployed  string            `json:"deployed,omitempty"` // what is actually deployed against the optimized one
	Optimized string            `json:"optimized"`
	Swapped   bool              `json:"swapped"` // the audience picked the optimized config, so the opposite is deployed
	Summary   []string          `json:"summary,omitempty"`
	Progress  Progress          `json:"progress"`
	Started   time.Time         `json:"started,omitempty"`
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
		Phase:     Lobby,
		Questions: choice.Questions,
		Optimized: p.optimized.String(),
	}
	for i := range p.votes {
		p.votes[i] = map[string]choice.Option{}
	}
}

// Snapshot returns the current state.
func (p *Poll) Snapshot() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
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

// Next moves the session one step forward: open the next question, close the
// open one, or, after the last results, apply. It returns false when there is
// nothing left to advance to.
func (p *Poll) Next() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &p.state
	switch s.Phase {
	case Lobby:
		s.Phase, s.Question = Voting, 0
	case Voting:
		s.Winners[s.Question] = p.winner(s.Question)
		s.Phase = Results
	case Results:
		if s.Question < len(choice.Questions)-1 {
			s.Question++
			s.Phase = Voting
		} else {
			p.apply()
		}
	default:
		return false
	}
	p.changed()
	return true
}

func (p *Poll) apply() {
	s := &p.state
	audience := choice.Config(s.Winners)
	deployed := audience
	if audience == p.optimized {
		deployed, s.Swapped = audience.Opposite(), true
	}
	s.Audience, s.Deployed = audience.String(), deployed.String()
	s.Summary = deployed.Describe()
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

// Desired is what the bridge should apply, if anything yet.
func (p *Poll) Desired() (choice.Config, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.Deployed == "" {
		return choice.Config{}, false
	}
	c, err := choice.Parse(p.state.Deployed)
	return c, err == nil
}

// ReportControlPlane records what the bridge saw on the control plane.
func (p *Poll) ReportControlPlane(applied bool, ready, total int, conditions []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := &p.state.Progress
	pr.Applied, pr.ReplicasReady, pr.ReplicasTotal, pr.Conditions = applied, ready, total, conditions
	pr.ReportedAt = time.Now()
	p.promote()
	p.changed()
}

// ReportPods records the engine pods as the app's own cluster sees them.
func (p *Poll) ReportPods(pods []PodState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Progress.Pods = pods
	p.promote()
	p.changed()
}

// promote moves Deploying to Ready once every engine pod is ready, or the
// control plane says the replica is. Either is enough: the pods are the faster
// signal, the replica the authoritative one.
func (p *Poll) promote() {
	if p.state.Phase != Deploying {
		return
	}
	pr := p.state.Progress
	podsReady := len(pr.Pods) > 0
	for _, pod := range pr.Pods {
		if pod.Step != "ready" {
			podsReady = false
		}
	}
	if podsReady || (pr.ReplicasTotal > 0 && pr.ReplicasReady == pr.ReplicasTotal) {
		p.state.Phase = Ready
	}
}

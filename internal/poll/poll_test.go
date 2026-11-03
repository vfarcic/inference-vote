package poll

import (
	"maps"
	"testing"

	"github.com/vfarcic/inference-vote/internal/choice"
)

func mustParse(t *testing.T, s string) choice.Config {
	t.Helper()
	c, err := choice.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// run votes through all three questions, each voter list giving that
// question's answers, and applies.
func run(t *testing.T, p *Poll, answers [3][]choice.Option) State {
	t.Helper()
	p.Next() // open question 1
	for q, list := range answers {
		for i, o := range list {
			if !p.Vote(string(rune('a'+i)), q, o) {
				t.Fatalf("vote %d on question %d was refused", i, q)
			}
		}
		p.Next() // close it and open the next, or apply after the last
	}
	return p.Snapshot()
}

func TestMajorityWins(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	s := run(t, p, [3][]choice.Option{
		{choice.A, choice.A, choice.B},
		{choice.A, choice.B, choice.A},
		{choice.B, choice.B, choice.A},
	})
	if s.Phase != Deploying || s.Audience != "AAB" || s.Opposite != "BBA" || s.AudienceIsOptimized || !s.OppositeIsOptimized {
		t.Fatalf("got %+v", s)
	}
	// BBA is the optimized config, already running: only the pick deploys.
	want := map[string]string{"audience": "AAB"}
	if got := desired(t, p); !maps.Equal(got, want) {
		t.Fatalf("Desired = %v, want %v", got, want)
	}
	if s.Serves("opposite") != "optimized" || s.Serves("audience") != "audience" {
		t.Fatalf("serves: audience by %s, opposite by %s", s.Serves("audience"), s.Serves("opposite"))
	}
}

func desired(t *testing.T, p *Poll) map[string]string {
	t.Helper()
	d, ok := p.Desired()
	if !ok {
		t.Fatal("nothing desired")
	}
	out := map[string]string{}
	for name, c := range d {
		out[name] = c.String()
	}
	return out
}

func TestThePickAndItsOppositeBothDeploy(t *testing.T) {
	p := New(mustParse(t, "BAA"))
	s := run(t, p, [3][]choice.Option{{choice.B}, {choice.B}, {choice.A}})
	want := map[string]string{"audience": "BBA", "opposite": "AAB"}
	if got := desired(t, p); !maps.Equal(got, want) || len(s.Deployments) != 2 || s.Deployments[0].Name != "audience" {
		t.Fatalf("Desired = %v, deployments %+v", got, s.Deployments)
	}
}

func TestPickingTheOptimizedDeploysOnlyTheOpposite(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	s := run(t, p, [3][]choice.Option{{choice.B}, {choice.B}, {choice.A}})
	if !s.AudienceIsOptimized || s.Serves("audience") != "optimized" {
		t.Fatalf("got %+v", s)
	}
	want := map[string]string{"opposite": "AAB"}
	if got := desired(t, p); !maps.Equal(got, want) {
		t.Fatalf("Desired = %v, want %v", got, want)
	}
}

func TestTieGoesAgainstTheOptimized(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	s := run(t, p, [3][]choice.Option{
		{choice.A, choice.B},
		{choice.A, choice.B},
		{choice.A, choice.B},
	})
	if s.Audience != "AAB" {
		t.Fatalf("ties should go against BBA, got %s", s.Audience)
	}
}

func TestChangingAVote(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	p.Next()
	p.Vote("x", 0, choice.A)
	p.Vote("x", 0, choice.B)
	if got := p.Snapshot().Tally[0]; got != [2]int{0, 1} {
		t.Fatalf("tally %v", got)
	}
}

func TestVotesOutsideTheOpenQuestionAreIgnored(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	if p.Vote("x", 0, choice.A) {
		t.Fatal("voted in the lobby")
	}
	p.Next()
	if p.Vote("x", 1, choice.A) {
		t.Fatal("voted on a question that is not open")
	}
}

func TestServingWhenPodsAre(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	run(t, p, [3][]choice.Option{{choice.A}, {choice.A}, {choice.A}})
	get := func() Deployment { d, _ := p.Snapshot().Deployment("audience"); return d }
	p.ReportPods("audience", []PodState{{Name: "a", Step: "ready"}, {Name: "b", Step: "loading model"}})
	p.MarkServing("audience")
	if d := get(); d.EnginesReady || d.Serving {
		t.Fatal("ready or serving before every pod was")
	}
	p.ReportPods("audience", []PodState{{Name: "a", Step: "ready"}, {Name: "b", Step: "ready"}})
	if d := get(); !d.EnginesReady || d.Serving {
		t.Fatal("ready engines should not count as serving before the gateway answers")
	}
	p.MarkServing("audience")
	if s := p.Snapshot(); !s.Serving("audience") || s.Serving("opposite") || s.Phase != Deploying {
		t.Fatal("not serving after the gateway answered, the opposite serving with it, or the race started on its own")
	}
	// The opposite here is BBB, its own deployment, with its own progress.
	if !p.Snapshot().Serving("optimized") {
		t.Fatal("the optimized deployment always serves")
	}
}

func TestTheRaceStartsWithoutTheAudiencesDeployment(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	run(t, p, [3][]choice.Option{{choice.A}, {choice.A}, {choice.A}})
	if !p.Next() || p.Snapshot().Phase != Race {
		t.Fatal("Next during deploying did not start the race")
	}
	if p.Next() {
		t.Fatal("advanced past the race")
	}
	p.ReportPods("audience", []PodState{{Name: "a", Step: "ready"}})
	p.MarkServing("audience")
	if !p.Snapshot().Serving("audience") {
		t.Fatal("a deployment that comes up during the race should still count as serving")
	}
}

func TestResetStartsANewSession(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	first := p.Snapshot().Session
	p.Reset()
	if p.Snapshot().Session == first {
		t.Fatal("session did not change on reset")
	}
}

// Every change redraws every phone, so a report that changes nothing must not
// count as one: it closed the keyboard on phones mid-typing.
func TestUnchangedReportsAreNotChanges(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	run(t, p, [3][]choice.Option{{choice.A}, {choice.A}, {choice.A}})
	pods := []PodState{{Name: "a", Step: "loading model"}}
	p.ReportPods("audience", pods)
	p.ReportControlPlane("audience", true, 0, 1, []string{"Ready=False"})
	v := p.Snapshot().Version
	p.ReportPods("audience", []PodState{{Name: "a", Step: "loading model"}})
	p.ReportControlPlane("audience", true, 0, 1, []string{"Ready=False"})
	if p.Snapshot().Version != v {
		t.Fatal("an unchanged report counted as a change")
	}
	p.ReportPods("audience", []PodState{{Name: "a", Step: "ready"}})
	if p.Snapshot().Version == v {
		t.Fatal("a changed report did not count")
	}
}

// A snapshot is the caller's: later reports don't change it under them.
func TestSnapshotsDontChange(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	run(t, p, [3][]choice.Option{{choice.A}, {choice.A}, {choice.A}})
	s := p.Snapshot()
	p.ReportPods("opposite", []PodState{{Name: "a", Step: "ready"}})
	if d, _ := s.Deployment("opposite"); len(d.Progress.Pods) != 0 {
		t.Fatal("a report changed an earlier snapshot")
	}
}

// The control plane says a replica is ready while an earlier config's engines
// still serve, so only the pods make a deployment's engines ready.
func TestTheReplicaAloneIsNotReady(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	run(t, p, [3][]choice.Option{{choice.A}, {choice.A}, {choice.A}})
	p.ReportControlPlane("audience", true, 1, 1, []string{"Ready=True"})
	if d, _ := p.Snapshot().Deployment("audience"); d.EnginesReady {
		t.Fatal("ready on the control plane's word alone")
	}
}

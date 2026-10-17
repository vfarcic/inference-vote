package poll

import (
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
	if s.Phase != Deploying || s.Audience != "AAB" || s.Deployed != "AAB" || s.Swapped {
		t.Fatalf("got phase %s audience %s deployed %s swapped %v", s.Phase, s.Audience, s.Deployed, s.Swapped)
	}
	if c, ok := p.Desired(); !ok || c.String() != "AAB" {
		t.Fatalf("Desired = %v %v", c, ok)
	}
}

func TestPickingTheOptimizedDeploysTheOpposite(t *testing.T) {
	p := New(mustParse(t, "BBA"))
	s := run(t, p, [3][]choice.Option{{choice.B}, {choice.B}, {choice.A}})
	if s.Audience != "BBA" || s.Deployed != "AAB" || !s.Swapped {
		t.Fatalf("audience %s deployed %s swapped %v", s.Audience, s.Deployed, s.Swapped)
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
	p.ReportPods([]PodState{{Name: "a", Step: "ready"}, {Name: "b", Step: "loading model"}})
	p.MarkServing()
	if s := p.Snapshot(); s.EnginesReady || s.Serving {
		t.Fatal("ready or serving before every pod was")
	}
	p.ReportPods([]PodState{{Name: "a", Step: "ready"}, {Name: "b", Step: "ready"}})
	if s := p.Snapshot(); !s.EnginesReady || s.Serving {
		t.Fatal("ready engines should not count as serving before the gateway answers")
	}
	p.MarkServing()
	if s := p.Snapshot(); !s.Serving || s.Phase != Deploying {
		t.Fatal("not serving after the gateway answered, or the race started on its own")
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
	p.ReportPods([]PodState{{Name: "a", Step: "ready"}})
	p.MarkServing()
	if !p.Snapshot().Serving {
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
	pods := []PodState{{Name: "a", Step: "loading model"}}
	p.ReportPods(pods)
	p.ReportControlPlane(true, 0, 1, []string{"Ready=False"})
	v := p.Snapshot().Version
	p.ReportPods([]PodState{{Name: "a", Step: "loading model"}})
	p.ReportControlPlane(true, 0, 1, []string{"Ready=False"})
	if p.Snapshot().Version != v {
		t.Fatal("an unchanged report counted as a change")
	}
	p.ReportPods([]PodState{{Name: "a", Step: "ready"}})
	if p.Snapshot().Version == v {
		t.Fatal("a changed report did not count")
	}
}

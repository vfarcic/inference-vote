package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/vfarcic/inference-vote/internal/kube"
	"github.com/vfarcic/inference-vote/internal/poll"
	"github.com/vfarcic/inference-vote/internal/stack"
)

// What each deployment became, collected from the inference cluster while it
// comes up and during the race: the resources Modelplane composed, what
// controllers created from them, and what vLLM did inside the engines.
type stacks struct {
	mu         sync.Mutex
	session    int
	sides      map[string]*stackSide
	pairs      []stack.Pair
	logs       map[string]string // engine pod -> the startup lines read so far
	metrics    map[string]string // engine pod -> its metrics, read once
	types      []kube.Type
	typesAt    time.Time
	unreadable []string // kinds the app isn't allowed to list, so missing from the screen
}

type stackSide struct {
	Composed []composedItem  `json:"composed"`
	Created  []stack.Created `json:"created"`
	Engines  []stack.Engine  `json:"engines"`
}

// composedItem is a composed resource as the deployment screen lists it:
// what it is, not yet what it holds.
type composedItem struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// WatchStacks collects both deployments' stacks every few seconds while there
// is something to show. Engines are read in every phase: what vLLM logs at
// startup is gone once serving rotates its log, and the optimized engine
// starts long before the race.
func (s *Server) WatchStacks(ctx context.Context) {
	if s.kube == nil {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var pods podList
		if err := s.kube.List(ctx, "/api/v1/pods", "modelplane.ai/engine,modelplane.ai/deployment in (audience,optimized)", &pods); err != nil {
			log.Printf("listing engines: %v", err)
			continue
		}
		s.captureEngines(ctx, client, pods)
		st := s.poll.Snapshot()
		if len(pods.Items) == 0 || (st.Phase != poll.Deploying && st.Phase != poll.Race) {
			continue
		}
		if err := s.collectStacks(ctx, pods, st.Session); err != nil {
			log.Printf("collecting what the deployments became: %v", err)
		}
	}
}

// captureEngines keeps each engine pod's startup lines and metrics, for as
// long as the pod exists. A log is read until vLLM has sized its KV cache, the
// last line that matters; metrics once the engine is ready.
func (s *Server) captureEngines(ctx context.Context, client *http.Client, pods podList) {
	s.stacks.mu.Lock()
	if s.stacks.logs == nil {
		s.stacks.logs, s.stacks.metrics = map[string]string{}, map[string]string{}
	}
	alive := map[string]bool{}
	for _, p := range pods.Items {
		alive[p.Metadata.Name] = true
	}
	for name := range s.stacks.logs {
		if !alive[name] {
			delete(s.stacks.logs, name)
			delete(s.stacks.metrics, name)
		}
	}
	s.stacks.mu.Unlock()
	for _, p := range pods.Items {
		name := p.Metadata.Name
		s.stacks.mu.Lock()
		l, m := s.stacks.logs[name], s.stacks.metrics[name]
		s.stacks.mu.Unlock()
		if !stack.Sized(l) {
			if b, err := s.kube.Raw(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", p.Metadata.Namespace, name)); err == nil {
				if fresh := stack.StartupLines(string(b)); fresh != "" {
					l = fresh // a rotated log has none, and what was read earlier stands
				}
			}
		}
		ready := len(p.Status.ContainerStatuses) > 0 && p.Status.ContainerStatuses[0].Ready
		if m == "" && ready && p.Status.PodIP != "" {
			if resp, err := client.Get("http://" + p.Status.PodIP + ":8000/metrics"); err == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				resp.Body.Close()
				m = stack.ConfigLine(string(b))
			}
		}
		s.stacks.mu.Lock()
		s.stacks.logs[name], s.stacks.metrics[name] = l, m
		s.stacks.mu.Unlock()
	}
}

func (s *Server) collectStacks(ctx context.Context, pods podList, session int) error {
	ns := pods.Items[0].Metadata.Namespace

	s.stacks.mu.Lock()
	s.stacks.session = session
	types := s.stacks.types
	if time.Since(s.stacks.typesAt) > 10*time.Minute {
		types = nil
	}
	s.stacks.mu.Unlock()
	if types == nil {
		var err error
		if types, err = s.kube.NamespacedTypes(ctx); err != nil {
			return fmt.Errorf("discovering resource types: %w", err)
		}
		s.stacks.mu.Lock()
		s.stacks.types, s.stacks.typesAt, s.stacks.unreadable = types, time.Now(), nil
		s.stacks.mu.Unlock()
	}

	var all []stack.Object
	readable := types[:0:0]
	var unreadable []string
	for _, t := range types {
		items, err := s.kube.ListType(ctx, ns, t)
		var se *kube.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			// Not a kind the app may read. It isn't asked for again, but the
			// screen says it couldn't be: a resource of that kind would be
			// missing from it.
			unreadable = append(unreadable, t.Kind+" ("+t.APIVersion()+")")
			continue
		}
		readable = append(readable, t)
		for _, it := range items {
			all = append(all, stack.Object(it))
		}
	}
	s.stacks.mu.Lock()
	s.stacks.types = readable
	s.stacks.unreadable = append(s.stacks.unreadable, unreadable...)
	s.stacks.mu.Unlock()

	sides := map[string]*stackSide{}
	composedBy := map[string][]stack.Object{}
	for _, dep := range []string{"optimized", "audience"} {
		var mine []stack.Object
		for _, o := range all {
			if stack.Belongs(o.Name(), dep) {
				mine = append(mine, o)
			}
		}
		composed, created := stack.Split(mine)
		composedBy[dep] = composed
		logs, metrics := s.engineOutput(pods, dep)
		side := &stackSide{Created: stack.Summarize(composed, created, dep), Engines: stack.Engines(created, logs, metrics)}
		for _, o := range composed {
			side.Composed = append(side.Composed, composedItem{APIVersion: o.APIVersion(), Kind: o.Kind(), Name: o.Name()})
		}
		sides[dep] = side
	}
	pairs := stack.Compare(composedBy["optimized"], composedBy["audience"])

	s.stacks.mu.Lock()
	s.stacks.sides, s.stacks.pairs = sides, pairs
	s.stacks.mu.Unlock()
	return nil
}

// engineOutput is what captureEngines kept for a deployment's engine pods.
func (s *Server) engineOutput(pods podList, dep string) (logs, metrics map[string]string) {
	logs, metrics = map[string]string{}, map[string]string{}
	s.stacks.mu.Lock()
	defer s.stacks.mu.Unlock()
	for _, p := range pods.Items {
		if p.Metadata.Labels["modelplane.ai/deployment"] == dep {
			logs[p.Metadata.Name], metrics[p.Metadata.Name] = s.stacks.logs[p.Metadata.Name], s.stacks.metrics[p.Metadata.Name]
		}
	}
	return logs, metrics
}

// stackState is what the presenter's screen polls: during the deployment,
// only the audience's side, since the optimized one is revealed with the race;
// during the race, both, and what differs between them.
func (s *Server) stackState(w http.ResponseWriter, r *http.Request) {
	st := s.poll.Snapshot()
	s.stacks.mu.Lock()
	defer s.stacks.mu.Unlock()
	if s.stacks.sides == nil || s.stacks.session != st.Session {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	unreadable := s.stacks.unreadable
	switch st.Phase {
	case poll.Deploying:
		writeJSON(w, http.StatusOK, map[string]any{"audience": s.stacks.sides["audience"], "unreadable": unreadable})
	case poll.Race:
		writeJSON(w, http.StatusOK, map[string]any{"optimized": s.stacks.sides["optimized"], "audience": s.stacks.sides["audience"], "pairs": s.stacks.pairs, "unreadable": unreadable})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

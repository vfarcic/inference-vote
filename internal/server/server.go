// Package server is the voting app: the page the audience opens from the QR
// code, the presenter's controls, the endpoints the bridge polls, and a chat
// proxy so the audience can talk to both deployments without ever holding a
// gateway key.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/kube"
	"github.com/vfarcic/inference-vote/internal/load"
	"github.com/vfarcic/inference-vote/internal/poll"
	"github.com/vfarcic/inference-vote/web"
)

type Config struct {
	PresenterToken string // guards next and reset
	BridgeToken    string // guards the bridge endpoints
	GatewayURL     string // InferenceGateway base URL, for chat
	GatewayKey     string // the audience caller's API key on that gateway
	PodSelector    string // label selector for the audience's engine pods
	Load           load.Config
}

type Server struct {
	cfg  Config
	poll *poll.Poll
	chat *limiter
	kube *kube.Client // nil outside a cluster; pod progress is then skipped
}

func New(cfg Config, p *poll.Poll, k *kube.Client) *Server {
	return &Server{cfg: cfg, poll: p, chat: newLimiter(6, time.Minute), kube: k}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web.Files))
	mux.HandleFunc("GET /presenter", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, web.Files, "presenter.html")
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/vote", s.vote)
	mux.HandleFunc("POST /api/chat", s.chatProxy)
	mux.HandleFunc("POST /api/presenter/next", s.presenter(func() { s.poll.Next() }))
	mux.HandleFunc("POST /api/presenter/reset", s.presenter(s.poll.Reset))
	mux.HandleFunc("GET /api/bridge/desired", s.bridgeDesired)
	mux.HandleFunc("POST /api/bridge/report", s.bridgeReport)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

// voter identifies a phone by a random cookie. It is not authentication, only a
// way to let someone change their vote instead of adding a second one.
func voter(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("voter"); err == nil && len(c.Value) == 32 {
		return c.Value
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: "voter", Value: id, Path: "/", MaxAge: 86400, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return id
}

type view struct {
	poll.State
	Mine [3]choice.Option `json:"mine"`
}

func (s *Server) viewFor(id string) view {
	v := view{State: s.poll.Snapshot()}
	for q := range v.Mine {
		v.Mine[q] = s.poll.VoteOf(id, q)
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.viewFor(voter(w, r)))
}

// events streams the state to a phone whenever it changes, so nobody navigates:
// the screen follows the presenter.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := voter(w, r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, stop := s.poll.Subscribe()
	defer stop()
	send := func() bool {
		b, _ := json.Marshal(s.viewFor(id))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	// A comment every 20s keeps proxies and phones from closing an idle stream.
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) vote(w http.ResponseWriter, r *http.Request) {
	id := voter(w, r)
	var body struct {
		Question int           `json:"question"`
		Option   choice.Option `json:"option"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad vote", http.StatusBadRequest)
		return
	}
	s.poll.Vote(id, body.Question, body.Option)
	writeJSON(w, http.StatusOK, s.viewFor(id))
}

func tokenOK(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) presenter(action func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !tokenOK(bearer(r), s.cfg.PresenterToken) {
			http.Error(w, "presenter token required", http.StatusUnauthorized)
			return
		}
		action()
		writeJSON(w, http.StatusOK, s.poll.Snapshot())
	}
}

// bridgeDesired tells the bridge what to apply: 204 until the votes are in.
func (s *Server) bridgeDesired(w http.ResponseWriter, r *http.Request) {
	if !tokenOK(bearer(r), s.cfg.BridgeToken) {
		http.Error(w, "bridge token required", http.StatusUnauthorized)
		return
	}
	c, ok := s.poll.Desired()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"config": c.String()})
}

func (s *Server) bridgeReport(w http.ResponseWriter, r *http.Request) {
	if !tokenOK(bearer(r), s.cfg.BridgeToken) {
		http.Error(w, "bridge token required", http.StatusUnauthorized)
		return
	}
	var body struct {
		Applied       bool     `json:"applied"`
		ReplicasReady int      `json:"replicasReady"`
		ReplicasTotal int      `json:"replicasTotal"`
		Conditions    []string `json:"conditions"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	s.poll.ReportControlPlane(body.Applied, body.ReplicasReady, body.ReplicasTotal, body.Conditions)
	w.WriteHeader(http.StatusNoContent)
}

// chatProxy forwards one chat turn to the gateway as the audience caller. The
// browser names which deployment it wants; the key stays on the server.
func (s *Server) chatProxy(w http.ResponseWriter, r *http.Request) {
	id := voter(w, r)
	if s.poll.Snapshot().Phase != poll.Ready {
		http.Error(w, "chat opens once both deployments serve", http.StatusConflict)
		return
	}
	if !s.chat.allow(id) {
		http.Error(w, "slow down: a few messages a minute each", http.StatusTooManyRequests)
		return
	}
	var body struct {
		Target   string            `json:"target"` // audience or optimized
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || len(body.Messages) == 0 {
		http.Error(w, "bad chat request", http.StatusBadRequest)
		return
	}
	if body.Target != "audience" && body.Target != "optimized" {
		http.Error(w, "target must be audience or optimized", http.StatusBadRequest)
		return
	}
	req, _ := json.Marshal(map[string]any{
		"model":      choice.Namespace + "/" + body.Target,
		"messages":   body.Messages,
		"max_tokens": 400,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	out, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.GatewayURL, "/")+"/v1/chat/completions", bytes.NewReader(req))
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("Authorization", "Bearer "+s.cfg.GatewayKey)
	start := time.Now()
	resp, err := http.DefaultClient.Do(out)
	if err != nil {
		http.Error(w, "gateway unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&completion) != nil || len(completion.Choices) == 0 {
		http.Error(w, "the model did not answer", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content": completion.Choices[0].Message.Content,
		"seconds": time.Since(start).Seconds(),
		"tokens":  completion.Usage.CompletionTokens,
	})
}

// WatchPods reports the audience's engine pods to the poll every two seconds
// while their deployment is coming up. Modelplane composes them onto this same
// cluster, so the app sees them directly; the bridge only sees the control plane.
func (s *Server) WatchPods(ctx context.Context) {
	if s.kube == nil {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.poll.Snapshot().Phase != poll.Deploying {
			continue
		}
		var list podList
		if err := s.kube.List(ctx, "/api/v1/pods", s.cfg.PodSelector, &list); err != nil {
			log.Printf("listing engine pods: %v", err)
			continue
		}
		pods := make([]poll.PodState, 0, len(list.Items))
		for _, p := range list.Items {
			pods = append(pods, poll.PodState{Name: p.Metadata.Name, Step: p.step()})
		}
		s.poll.ReportPods(pods)
	}
}

// StartLoadWhenReady runs the load test once, against both deployments at the
// same time, as soon as the audience's deployment serves. Starting it from here
// rather than by hand means nobody has to remember it on stage, and both
// deployments get exactly the same window.
func (s *Server) StartLoadWhenReady(ctx context.Context) {
	if s.cfg.Load.Key == "" || s.cfg.Load.Agents == 0 {
		log.Print("load test disabled: no load key or no agents")
		return
	}
	ch, stop := s.poll.Subscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
		if s.poll.Snapshot().Phase != poll.Ready {
			continue
		}
		log.Printf("load test: %d agents on each of %v for %s", s.cfg.Load.Agents, s.cfg.Load.Targets, s.cfg.Load.Duration)
		for _, r := range load.Run(ctx, s.cfg.Load) {
			log.Printf("load test: %s", r)
		}
		return
	}
}

type podList struct {
	Items []pod `json:"items"`
}

type pod struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		ContainerStatuses []struct {
			Ready bool `json:"ready"`
			State struct {
				Waiting *struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Running *struct{} `json:"running"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// step reduces a pod to what the audience cares about. vLLM is a running
// container long before it is ready: that stretch is the weights loading and
// the engine warming up.
func (p pod) step() string {
	if p.Spec.NodeName == "" {
		return "scheduling"
	}
	if len(p.Status.ContainerStatuses) == 0 {
		return "pulling image"
	}
	c := p.Status.ContainerStatuses[0]
	switch {
	case c.Ready:
		return "ready"
	case c.State.Running != nil:
		return "loading model"
	case c.State.Waiting != nil && (c.State.Waiting.Reason == "ContainerCreating" || c.State.Waiting.Reason == "PodInitializing"):
		return "pulling image"
	case c.State.Waiting != nil:
		return "failing: " + c.State.Waiting.Reason
	default:
		return "starting"
	}
}

// limiter allows n events per window per key.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	seen   map[string][]time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, seen: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.n {
		l.seen[key] = recent
		return false
	}
	l.seen[key] = append(recent, now)
	return true
}

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
	"github.com/vfarcic/inference-vote/internal/eval"
	"github.com/vfarcic/inference-vote/internal/gateway"
	"github.com/vfarcic/inference-vote/internal/kube"
	"github.com/vfarcic/inference-vote/internal/load"
	"github.com/vfarcic/inference-vote/internal/poll"
	"github.com/vfarcic/inference-vote/web"
	"rsc.io/qr"
)

type Config struct {
	PresenterToken string // guards next and reset
	BridgeToken    string // guards the bridge endpoints
	GatewayURL     string // InferenceGateway base URL, for chat
	GatewayKey     string // the audience caller's API key on that gateway
	PodSelector    string // label selector for the audience's engine pods
	Load           load.Config
	Eval           eval.Config
	Options        choice.Options // how both deployments are deployed, for the manifests on screen
}

type Server struct {
	cfg  Config
	poll *poll.Poll
	chat *limiter
	kube *kube.Client // nil outside a cluster; pod progress is then skipped

	raceMu sync.Mutex
	race   *race // nil until the presenter starts it

	reasonsMu sync.Mutex
	reasons   map[string]string // crash reasons read from logs, by pod and restart count
}

func New(cfg Config, p *poll.Poll, k *kube.Client) *Server {
	return &Server{cfg: cfg, poll: p, chat: newLimiter(6, time.Minute), kube: k, reasons: map[string]string{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// The pages are embedded, so they change only with a new image; no-cache
	// makes browsers check, so a fix rolled out before the talk reaches every
	// phone that already has the page open.
	files := http.FileServerFS(web.Files)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /presenter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, web.Files, "presenter.html")
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/race", s.raceState)
	mux.HandleFunc("POST /api/vote", s.vote)
	mux.HandleFunc("POST /api/chat", s.chatProxy)
	mux.HandleFunc("POST /api/presenter/next", s.presenter(func() { s.poll.Next() }))
	mux.HandleFunc("POST /api/presenter/reset", s.presenter(s.poll.Reset))
	mux.HandleFunc("POST /api/presenter/results", s.showResults)
	mux.HandleFunc("GET /api/bridge/desired", s.bridgeDesired)
	mux.HandleFunc("POST /api/bridge/report", s.bridgeReport)
	mux.HandleFunc("GET /qr.png", qrCode)
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
	// The optimized config is revealed on the race screen, not before: the
	// room should vote on the questions, not against an answer key.
	if v.Phase != poll.Race {
		v.Optimized, v.Swapped = "", false
	}
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
	st := s.poll.Snapshot()
	if st.Phase != poll.Race {
		http.Error(w, "chat opens when the race starts", http.StatusConflict)
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
	if body.Target == "audience" && !st.Serving {
		http.Error(w, "your pick is not serving", http.StatusConflict)
		return
	}
	s.countChat(body.Target)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	start := time.Now()
	a, err := gateway.Chat(ctx, http.DefaultClient, s.cfg.GatewayURL, s.cfg.GatewayKey, map[string]any{
		"model":      choice.Namespace + "/" + body.Target,
		"messages":   body.Messages,
		"max_tokens": 400,
	})
	if err != nil {
		http.Error(w, "the model did not answer", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content": a.Content,
		"seconds": time.Since(start).Seconds(),
		"tokens":  a.Tokens,
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
		// During the race too: a deployment can still come up, or crash.
		if ph := s.poll.Snapshot().Phase; ph != poll.Deploying && ph != poll.Race {
			continue
		}
		var list podList
		if err := s.kube.List(ctx, "/api/v1/pods", s.cfg.PodSelector, &list); err != nil {
			log.Printf("listing engine pods: %v", err)
			continue
		}
		pods := make([]poll.PodState, 0, len(list.Items))
		for _, p := range list.Items {
			state := poll.PodState{Name: p.Metadata.Name, Step: p.step()}
			if len(p.Status.ContainerStatuses) > 0 {
				state.Restarts = p.Status.ContainerStatuses[0].RestartCount
			}
			if state.Restarts > 0 {
				state.Reason = s.crashReason(ctx, p, state.Restarts)
			}
			pods = append(pods, state)
		}
		s.poll.ReportPods(pods)
	}
}

type podList struct {
	Items []pod `json:"items"`
}

type pod struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		PodIP             string `json:"podIP"`
		ContainerStatuses []struct {
			Ready        bool `json:"ready"`
			RestartCount int  `json:"restartCount"`
			LastState    struct {
				Terminated *struct {
					Reason   string `json:"reason"`
					ExitCode int    `json:"exitCode"`
				} `json:"terminated"`
			} `json:"lastState"`
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
	case c.State.Running != nil && c.RestartCount > 0:
		return "loading model again"
	case c.State.Running != nil:
		return "loading model"
	case c.State.Waiting != nil && c.State.Waiting.Reason == "CrashLoopBackOff":
		return "crashed, waiting to retry"
	case c.State.Waiting != nil && (c.State.Waiting.Reason == "ContainerCreating" || c.State.Waiting.Reason == "PodInitializing"):
		return "pulling image"
	case c.State.Waiting != nil:
		return "failing: " + c.State.Waiting.Reason
	default:
		return "starting"
	}
}

// ProbeGateway confirms the audience's deployment actually answers. Ready
// engines are not enough: Modelplane routes the gateway to them some minutes
// later, and until then every request is a 404. Nor is the first answer: the
// gateway is still taking in the new route, and requests that fail while it
// does can get either deployment shut out of it for a while. So once the
// engines are ready, this asks the gateway for a single token from both
// deployments every few seconds, and marks the audience's as serving once
// both have answered every time for settleTime.
func (s *Server) ProbeGateway(ctx context.Context) {
	client := &http.Client{Timeout: 20 * time.Second}
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	var since time.Time // when the current run of answers began
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st := s.poll.Snapshot()
		if !st.EnginesReady || st.Serving {
			since = time.Time{}
			continue
		}
		if !s.answers(ctx, client, "audience") || !s.answers(ctx, client, "optimized") {
			since = time.Time{}
			continue
		}
		if since.IsZero() {
			since = time.Now()
			log.Printf("gateway answers for %s/audience, %s after the votes; waiting for it to settle", choice.Namespace, time.Since(st.Started).Round(time.Second))
		}
		if time.Since(since) >= settleTime {
			log.Printf("gateway settled, %s after the votes", time.Since(st.Started).Round(time.Second))
			s.poll.MarkServing()
		}
	}
}

// settleTime is how long both deployments must keep answering before the
// audience's counts as serving.
const settleTime = 20 * time.Second

func (s *Server) answers(ctx context.Context, client *http.Client, target string) bool {
	body, _ := json.Marshal(map[string]any{
		"model":      choice.Namespace + "/" + target,
		"messages":   []map[string]string{{"role": "user", "content": "Hi"}},
		"max_tokens": 1,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.GatewayURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.Load.Key)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// crashReason says why an engine pod last crashed, in words, from the end of
// its previous container's log. It is read once per crash.
func (s *Server) crashReason(ctx context.Context, p pod, restarts int) string {
	key := fmt.Sprintf("%s/%d", p.Metadata.Name, restarts)
	s.reasonsMu.Lock()
	reason, ok := s.reasons[key]
	s.reasonsMu.Unlock()
	if ok {
		return reason
	}
	if t := p.Status.ContainerStatuses[0].LastState.Terminated; t != nil && t.Reason == "OOMKilled" {
		reason = "ran out of memory (the container's, not the GPU's)"
	} else {
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?previous=true&tailLines=400", p.Metadata.Namespace, p.Metadata.Name)
		logs, err := s.kube.Raw(ctx, path)
		if err != nil {
			log.Printf("reading %s's crash log: %v", p.Metadata.Name, err)
			return ""
		}
		reason = ReasonFromLog(string(logs))
	}
	s.reasonsMu.Lock()
	s.reasons[key] = reason
	s.reasonsMu.Unlock()
	return reason
}

// ReasonFromLog turns a crashed vLLM's log into one short sentence for the
// screen, recognizing the failures this setup can actually hit.
func ReasonFromLog(logs string) string {
	switch {
	case strings.Contains(logs, "No available memory for the cache blocks"),
		strings.Contains(logs, "larger than the available KV cache memory"),
		strings.Contains(logs, "Available KV cache memory") && strings.Contains(logs, "EngineCore failed to start"):
		return "not enough GPU memory left for the KV cache"
	case strings.Contains(logs, "CUDA out of memory"), strings.Contains(logs, "OutOfMemoryError"):
		return "ran out of GPU memory"
	case strings.Contains(logs, "NCCL"):
		return "the GPUs could not connect to each other (NCCL)"
	case strings.Contains(logs, "huggingface") && (strings.Contains(logs, "429") || strings.Contains(logs, "Timeout") || strings.Contains(logs, "ConnectionError")):
		return "could not download the model"
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.Contains(l, "Error") {
			if len(l) > 160 {
				l = l[:160] + "..."
			}
			return l
		}
	}
	return "crashed"
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

// qrCode is the QR code the presenter puts on screen. It encodes the address
// the presenter page was opened on, so it is right for nip.io and the real
// domain alike without being told either.
func qrCode(w http.ResponseWriter, r *http.Request) {
	code, err := qr.Encode("https://"+r.Host, qr.M)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	code.Scale = 16
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(code.PNG())
}

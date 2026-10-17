// Package load generates the traffic the verdict is based on. The audience's
// chat is too small and too uneven a sample, so this runs the same agent
// workload against both deployments, as its own gateway caller, and the race
// screen and Datadog compare what comes back.
//
// The workload is shaped like a coding agent, because that is what decides
// question 2: every request resends one long, identical opening (system prompt
// and tool definitions) followed by a conversation that only grows, as tool
// results come back and get appended.
package load

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type Config struct {
	GatewayURL string
	Key        string
	Targets    []string // ModelService names, each gets the same workload
	Agents     int      // concurrent agents per target
	Duration   time.Duration
	MaxTokens  int
	Seed       int64
}

// Stats is one target's running totals, safe for concurrent use: agents add to
// it while the race screen reads it.
type Stats struct {
	mu        sync.Mutex
	target    string
	started   time.Time
	finished  time.Time
	requests  int
	failures  int
	reasons   map[string]int // why requests failed, so a failure count can be explained
	tokens    int
	latencies []time.Duration
}

func (s *Stats) record(d time.Duration, tokens int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	if err != nil {
		s.failures++
		if s.reasons == nil {
			s.reasons = map[string]int{}
		}
		s.reasons[err.Error()]++
		return
	}
	s.tokens += tokens
	s.latencies = append(s.latencies, d)
}

// View is a snapshot of Stats, ready to serialize.
type View struct {
	Target       string    `json:"target"`
	Seconds      float64   `json:"seconds"` // how long it has run
	Done         bool      `json:"done"`
	Ended        time.Time `json:"ended,omitempty"`
	Requests     int       `json:"requests"`
	Failures     int       `json:"failures"`
	TokensPerSec float64   `json:"tokensPerSec"`
	P50          float64   `json:"p50"` // seconds per answer
	P95          float64   `json:"p95"`
	Reasons      []string  `json:"reasons,omitempty"` // the most common failures, counted
}

func (s *Stats) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.finished
	if end.IsZero() {
		end = time.Now()
	}
	v := View{Target: s.target, Seconds: end.Sub(s.started).Seconds(), Done: !s.finished.IsZero(), Ended: s.finished, Requests: s.requests, Failures: s.failures}
	if v.Seconds > 0 {
		v.TokensPerSec = float64(s.tokens) / v.Seconds
	}
	if n := len(s.latencies); n > 0 {
		sorted := slices.Clone(s.latencies)
		slices.Sort(sorted)
		v.P50 = sorted[int(0.50*float64(n-1))].Seconds()
		v.P95 = sorted[int(0.95*float64(n-1))].Seconds()
	}
	keys := make([]string, 0, len(s.reasons))
	for k := range s.reasons {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return s.reasons[b] - s.reasons[a] })
	for i, k := range keys {
		if i == 3 {
			break
		}
		v.Reasons = append(v.Reasons, fmt.Sprintf("%dx %s", s.reasons[k], k))
	}
	return v
}

func (v View) String() string {
	line := fmt.Sprintf("%-10s %5d requests %4d failures %7.1f tokens/s  p50 %5.1fs  p95 %5.1fs",
		v.Target, v.Requests, v.Failures, v.TokensPerSec, v.P50, v.P95)
	for _, r := range v.Reasons {
		line += "\n    " + r
	}
	return line
}

// Start runs the workload against one target in the background, for
// cfg.Duration, and returns its live totals. Each agent's sequence of requests
// is seeded by its index, so agent n sends the same conversation to every
// target, whenever each target starts.
func Start(ctx context.Context, cfg Config, target string) *Stats {
	s := &Stats{target: target, started: time.Now()}
	ctx, cancel := context.WithTimeout(ctx, cfg.Duration)
	var wg sync.WaitGroup
	for a := 0; a < cfg.Agents; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			agent(ctx, cfg, target, cfg.Seed+int64(a), s.record)
		}(a)
	}
	go func() {
		wg.Wait()
		cancel()
		s.mu.Lock()
		s.finished = time.Now()
		s.mu.Unlock()
	}()
	return s
}

// Run loads every target at once and waits for the end: the command-line load
// test, outside a talk.
func Run(ctx context.Context, cfg Config) []View {
	stats := make([]*Stats, len(cfg.Targets))
	for i, target := range cfg.Targets {
		stats[i] = Start(ctx, cfg, target)
	}
	views := make([]View, len(stats))
	for i, s := range stats {
		for !s.View().Done {
			time.Sleep(time.Second)
		}
		views[i] = s.View()
	}
	return views
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// One agent works through tasks until the context ends. A task is a few turns:
// ask, get an answer, append a tool result, ask again. When the conversation
// nears the model's 8192 token context, the agent starts a new task, keeping the
// shared opening.
func agent(ctx context.Context, cfg Config, target string, seed int64, record func(time.Duration, int, error)) {
	rng := rand.New(rand.NewSource(seed))
	client := &http.Client{Timeout: 3 * time.Minute}
	history := []message{{Role: "system", Content: systemPrompt}}
	history = append(history, message{Role: "user", Content: task(rng)})
	for ctx.Err() == nil {
		start := time.Now()
		answer, tokens, err := complete(ctx, client, cfg, target, history)
		if ctx.Err() != nil {
			return // cut off by the end of the run, not a failure
		}
		record(time.Since(start), tokens, err)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		history = append(history, message{Role: "assistant", Content: answer}, message{Role: "user", Content: toolResult(rng)})
		if approxTokens(history) > 6000 {
			history = []message{history[0], {Role: "user", Content: task(rng)}}
		}
	}
}

func complete(ctx context.Context, client *http.Client, cfg Config, target string, history []message) (string, int, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       "ml-team/" + target,
		"messages":    history,
		"max_tokens":  cfg.MaxTokens,
		"temperature": 0,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.GatewayURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The first line of the body says why, such as a context length error;
		// numbers in it are dropped so the same reason counts once.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		reason := regexp.MustCompile(`[0-9]+`).ReplaceAllString(strings.SplitN(string(msg), "\n", 2)[0], "N")
		return "", 0, fmt.Errorf("%d %s", resp.StatusCode, reason)
	}
	var out struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
		return "", 0, fmt.Errorf("%s: unreadable answer", target)
	}
	return out.Choices[0].Message.Content, out.Usage.CompletionTokens, nil
}

// approxTokens is good enough for staying under the context limit: English and
// code average about four characters a token.
func approxTokens(history []message) int {
	n := 0
	for _, m := range history {
		n += len(m.Content) / 4
	}
	return n
}

var tasks = []string{
	"Add input validation to the createOrder handler and return 400 on bad input.",
	"The checkout tests are flaky. Find out why and fix them.",
	"Refactor the payment client to retry on 503 with exponential backoff.",
	"Add a /healthz endpoint that checks the database connection.",
	"Rename the Customer.address field to shippingAddress everywhere.",
	"Write unit tests for the discount calculation.",
}

func task(rng *rand.Rand) string { return tasks[rng.Intn(len(tasks))] }

// toolResult stands in for what an agent's tools return: file contents and
// command output, which is most of the text an agent feeds back to a model.
func toolResult(rng *rand.Rand) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Tool result (read_file orders/handler_%d.go):\n", rng.Intn(40))
	for i := 0; i < 30+rng.Intn(30); i++ {
		fmt.Fprintf(&b, "func step%d(ctx context.Context, o *Order) error { if o.Total < %d { return errInvalid } ; return store.Save(ctx, o) }\n", i, rng.Intn(1000))
	}
	b.WriteString("Continue with the task. Say what you would change next and why.")
	return b.String()
}

// systemPrompt is the shared opening every request starts with, sized like a
// real agent's instructions plus tool definitions: a couple of thousand tokens.
var systemPrompt = func() string {
	var b strings.Builder
	b.WriteString("You are a coding agent working in a Go repository. Follow the repository conventions, keep changes minimal, and explain each change briefly.\n\nYou can call these tools:\n")
	tools := []string{"read_file", "write_file", "list_dir", "grep", "run_tests", "run_command", "git_diff", "git_log"}
	for i := 0; i < 4; i++ {
		for _, t := range tools {
			fmt.Fprintf(&b, `{"name": %q, "description": "Use %s to inspect or change the repository. Arguments must be valid JSON. Paths are relative to the repository root. Never touch files outside it.", "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "pattern": {"type": "string"}, "args": {"type": "array", "items": {"type": "string"}}}}}`+"\n", fmt.Sprintf("%s_v%d", t, i), t)
		}
	}
	return b.String()
}()

// Package eval measures answer quality, the other half of question 1: FP8 buys
// room for more conversations, and this is where it would show what it costs.
//
// It sends the same grade-school math problems (GSM8K, MIT licensed, see
// LICENSE-gsm8k) to a deployment at temperature 0 and checks each final number.
// Math is where quantization loses accuracy first, and a number is graded
// without anyone's judgement. Questions 2 and 3 are lossless, so any difference
// in score comes from question 1, or from noise: a point or two either way on
// 200 problems is within it.
package eval

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed gsm8k.jsonl
var data []byte

type Problem struct {
	Question string `json:"q"`
	Answer   string `json:"a"`
}

// Problems are the first 200 of the GSM8K test set.
var Problems = func() []Problem {
	var ps []Problem
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 64<<10), 64<<10)
	for s.Scan() {
		var p Problem
		if err := json.Unmarshal(s.Bytes(), &p); err != nil {
			panic(err)
		}
		ps = append(ps, p)
	}
	return ps
}()

type Config struct {
	GatewayURL string
	Key        string
	Problems   int // how many of the 200; 0 disables
	Workers    int // concurrent requests per deployment, kept low so it barely loads the GPUs
}

// Score is safe for concurrent use: Run updates it while the race screen reads it.
type Score struct {
	mu       sync.Mutex
	total    int
	answered int
	correct  int
	errors   int
	done     bool
}

type ScoreView struct {
	Total    int  `json:"total"`
	Answered int  `json:"answered"`
	Correct  int  `json:"correct"`
	Errors   int  `json:"errors"`
	Done     bool `json:"done"`
}

func (s *Score) View() ScoreView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ScoreView{Total: s.total, Answered: s.answered, Correct: s.correct, Errors: s.errors, Done: s.done}
}

func (v ScoreView) String() string {
	if v.Answered == 0 {
		return fmt.Sprintf("0/%d answered", v.Total)
	}
	return fmt.Sprintf("%d/%d correct (%.1f%%), %d errors", v.Correct, v.Answered, 100*float64(v.Correct)/float64(v.Answered), v.Errors)
}

// Start runs the problems against a deployment in the background.
func Start(ctx context.Context, cfg Config, target string) *Score {
	n := min(cfg.Problems, len(Problems))
	s := &Score{total: n}
	go func() {
		defer func() { s.mu.Lock(); s.done = true; s.mu.Unlock() }()
		work := make(chan Problem)
		var wg sync.WaitGroup
		client := &http.Client{Timeout: 3 * time.Minute}
		for range max(cfg.Workers, 1) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for p := range work {
					answer, err := ask(ctx, client, cfg, target, p.Question)
					if ctx.Err() != nil {
						return
					}
					s.mu.Lock()
					s.answered++
					switch {
					case err != nil:
						s.errors++
					case Correct(answer, p.Answer):
						s.correct++
					}
					s.mu.Unlock()
				}
			}()
		}
		for _, p := range Problems[:n] {
			select {
			case work <- p:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
		}
		close(work)
		wg.Wait()
	}()
	return s
}

const instruction = "Solve the problem step by step. End with a final line in the form \"Answer: <number>\".\n\n"

func ask(ctx context.Context, client *http.Client, cfg Config, target, question string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       "ml-team/" + target,
		"messages":    []map[string]string{{"role": "user", "content": instruction + question}},
		"max_tokens":  512,
		"temperature": 0,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.GatewayURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %d", target, resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("%s: unreadable answer", target)
	}
	return out.Choices[0].Message.Content, nil
}

var (
	answerLine = regexp.MustCompile(`(?i)answer:\s*\**\s*\$?\s*(-?[0-9][0-9,]*(?:\.[0-9]+)?)`)
	anyNumber  = regexp.MustCompile(`-?[0-9][0-9,]*(?:\.[0-9]+)?`)
)

// Correct compares the reply's final number with the expected one. It takes the
// number after the last "Answer:", or failing that the last number anywhere,
// since a model that skips the format but gets it right should still count.
func Correct(reply, expected string) bool {
	var got string
	if m := answerLine.FindAllStringSubmatch(reply, -1); len(m) > 0 {
		got = m[len(m)-1][1]
	} else if m := anyNumber.FindAllString(reply, -1); len(m) > 0 {
		got = m[len(m)-1]
	} else {
		return false
	}
	g, err1 := strconv.ParseFloat(strings.ReplaceAll(got, ",", ""), 64)
	e, err2 := strconv.ParseFloat(expected, 64)
	return err1 == nil && err2 == nil && g == e
}

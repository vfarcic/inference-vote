// Package gateway calls the Modelplane gateway's chat completions endpoint.
//
// It always streams. The gateway gives up on an attempt when no bytes come back
// for 60 seconds and retries it, so under load an answer delivered all at once
// gets abandoned and generated again, and enough of those get the deployment
// ejected from the gateway. Streamed tokens keep the attempt alive.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Answer is a completed reply.
type Answer struct {
	Content string
	Tokens  int // completion tokens, as the engine counted them
}

// StatusError is a reply that never started: the gateway or the engine
// refused the request.
type StatusError struct {
	Code int
	Body string // the first line of what came back
}

func (e *StatusError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Body) }

// Chat sends one chat completion request. Body holds the request fields, such
// as model, messages and max_tokens; Chat adds the streaming ones.
func Chat(ctx context.Context, client *http.Client, url, key string, body map[string]any) (Answer, error) {
	return ChatStream(ctx, client, url, key, body, nil)
}

// ChatStream is Chat that also hands each piece of the answer to onDelta as it
// arrives.
func ChatStream(ctx context.Context, client *http.Client, url, key string, body map[string]any, onDelta func(string)) (Answer, error) {
	req := map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": true}}
	for k, v := range body {
		req[k] = v
	}
	raw, _ := json.Marshal(req)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/v1/chat/completions", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(r)
	if err != nil {
		return Answer{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return Answer{}, &StatusError{Code: resp.StatusCode, Body: strings.SplitN(string(msg), "\n", 2)[0]}
	}
	return read(resp.Body, onDelta)
}

// read collects a server-sent event stream of completion chunks.
func read(r io.Reader, onDelta func(string)) (Answer, error) {
	var a Answer
	var content strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			a.Content = content.String()
			return a, nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return Answer{}, fmt.Errorf("unreadable answer")
		}
		if chunk.Error != nil {
			return Answer{}, fmt.Errorf("failed mid-answer: %s", chunk.Error.Message)
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
			if onDelta != nil && c.Delta.Content != "" {
				onDelta(c.Delta.Content)
			}
		}
		if chunk.Usage != nil {
			a.Tokens = chunk.Usage.CompletionTokens
		}
	}
	if err := sc.Err(); err != nil {
		return Answer{}, err
	}
	return Answer{}, fmt.Errorf("the answer stopped before it ended")
}

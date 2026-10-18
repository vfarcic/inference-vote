package eval

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProblemsLoad(t *testing.T) {
	if len(Problems) != 200 || Problems[0].Answer != "18" {
		t.Fatalf("got %d problems, first answer %q", len(Problems), Problems[0].Answer)
	}
}

func TestCorrect(t *testing.T) {
	cases := []struct {
		reply, expected string
		want            bool
	}{
		{"9 eggs, 9 * 2 = 18.\nAnswer: 18", "18", true},
		{"Answer: $1,250", "1250", true},
		{"Answer: **42**", "42", true},
		{"Answer: 3.50", "3.5", true},
		{"So she makes 18 dollars.", "18", true},
		{"Answer: 17", "18", false},
		{"First 18, then Answer: 20", "18", false},
		{"I don't know.", "18", false},
	}
	for _, c := range cases {
		if got := Correct(c.reply, c.expected); got != c.want {
			t.Errorf("Correct(%q, %q) = %v, want %v", c.reply, c.expected, got, c.want)
		}
	}
}

func TestStartRetriesFailures(t *testing.T) {
	retryDelay = time.Millisecond
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 3 {
			http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Answer: 18\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	s := Start(context.Background(), Config{GatewayURL: srv.URL, Key: "k", Problems: 2, Workers: 1}, "optimized")
	deadline := time.Now().Add(5 * time.Second)
	for !s.View().Done && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// The first problem's answer is 18, the second's isn't: both answered once.
	if v := s.View(); !v.Done || v.Answered != 2 || v.Correct != 1 || v.Errors != 3 {
		t.Errorf("got %+v, want 2 answered, 1 correct, 3 errors", v)
	}
}

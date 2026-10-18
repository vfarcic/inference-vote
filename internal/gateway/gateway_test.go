package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatStreams(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		for _, part := range []string{"Hel", "lo", "!"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	var pieces []string
	a, err := ChatStream(context.Background(), srv.Client(), srv.URL+"/", "k", map[string]any{"model": "ml-team/optimized"}, func(d string) { pieces = append(pieces, d) })
	if err != nil {
		t.Fatal(err)
	}
	if a.Content != "Hello!" || a.Tokens != 3 || strings.Join(pieces, "|") != "Hel|lo|!" {
		t.Errorf("got %+v, pieces %q", a, pieces)
	}
	if got["stream"] != true || got["model"] != "ml-team/optimized" {
		t.Errorf("request = %v", got)
	}
}

func TestChatErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		handler func(http.ResponseWriter)
		want    string
	}{
		"refused": {func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "no healthy upstream\nmore")
		}, "503 no healthy upstream"},
		"error mid-answer": {func(w http.ResponseWriter) {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"error\":{\"message\":\"engine died\"}}\n\n")
		}, "failed mid-answer: engine died"},
		"cut off": {func(w http.ResponseWriter) {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		}, "the answer stopped before it ended"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tc.handler(w) }))
			defer srv.Close()
			_, err := Chat(context.Background(), srv.Client(), srv.URL, "k", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer srv.Close()
	_, err := Chat(context.Background(), srv.Client(), srv.URL, "k", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Errorf("err = %v, want a StatusError with 404", err)
	}
}

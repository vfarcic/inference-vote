package diff

import (
	"reflect"
	"testing"
)

func TestLines(t *testing.T) {
	a := []string{"name: optimized", "args:", "- --model=fp8", "- --kv-cache-dtype=fp8", "image: vllm"}
	b := []string{"name: audience", "args:", "- --model=bf16", "- --tensor-parallel-size=2", "- '--spec={}'", "image: vllm"}
	got := Lines(a, b)
	want := []Row{
		{"name: optimized", "name: audience", "changed"},
		{"args:", "args:", "same"},
		{"- --model=fp8", "- --model=bf16", "changed"},
		{"- --kv-cache-dtype=fp8", "", "left"},
		{"", "- --tensor-parallel-size=2", "right"},
		{"", "- '--spec={}'", "right"},
		{"image: vllm", "image: vllm", "same"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
}

func TestLinesOneSided(t *testing.T) {
	got := Lines([]string{"x", "y"}, []string{"x"})
	want := []Row{{"x", "x", "same"}, {"y", "", "left"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

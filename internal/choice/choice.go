// Package choice holds the three questions the audience votes on and turns a
// set of answers into the Modelplane ModelDeployment that serves them.
//
// Every one of the eight possible answer sets maps to exactly one manifest, and
// this package is the only place that mapping lives: the setup renders the
// optimized deployment with it, and the bridge renders the audience's.
package choice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Option is one answer to a question.
type Option string

const (
	A Option = "A"
	B Option = "B"
)

// Question is what the audience sees on their phones. The wording is plain
// language on purpose: they vote before any of the concepts are explained.
type Question struct {
	ID    int    `json:"id"`
	Text  string `json:"text"`
	A     string `json:"a"`
	B     string `json:"b"`
	Topic string `json:"topic"`
}

var Questions = []Question{
	{
		ID:    1,
		Text:  "What do you care about more?",
		A:     "Keep the model as precise as it was trained",
		B:     "Make room for more conversations at the same time",
		Topic: "quantization",
	},
	{
		ID:    2,
		Text:  "You have two GPUs. How do you use them?",
		A:     "One big engine split across both",
		B:     "Two separate copies, one per GPU",
		Topic: "tensor parallelism vs replicas",
	},
	{
		ID:    3,
		Text:  "How should the model write its answers?",
		A:     "One word at a time, the normal way",
		B:     "Guess ahead from what's already in the conversation, and let the model check",
		Topic: "speculative decoding",
	},
}

// Config is one answer per question, in question order.
type Config [3]Option

// Parse reads a config written as three letters, such as "BBA".
func Parse(s string) (Config, error) {
	var c Config
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != len(c) {
		return c, fmt.Errorf("config %q must be three letters, one per question", s)
	}
	for i, r := range s {
		o := Option(string(r))
		if o != A && o != B {
			return c, fmt.Errorf("config %q: answer %d must be A or B", s, i+1)
		}
		c[i] = o
	}
	return c, nil
}

func (c Config) String() string {
	return string(c[0]) + string(c[1]) + string(c[2])
}

// Opposite flips every answer. It is the challenger when the audience happens to
// pick the optimized configuration, so there is still something to compare.
func (c Config) Opposite() Config {
	var o Config
	for i, v := range c {
		if v == A {
			o[i] = B
		} else {
			o[i] = A
		}
	}
	return o
}

// Describe is the one-line summary shown on screen once a config is chosen.
func (c Config) Describe() []string {
	d := make([]string, len(c))
	for i, v := range c {
		if v == A {
			d[i] = Questions[i].A
		} else {
			d[i] = Questions[i].B
		}
	}
	return d
}

const (
	Image     = "vllm/vllm-openai:v0.27.1"
	Model16   = "Qwen/Qwen3-8B"
	Model8    = "Qwen/Qwen3-8B-FP8"
	Namespace = "ml-team"
)

// Options are how a config is deployed rather than what it is: the same for
// both deployments, so they never decide the comparison.
type Options struct {
	// Cache serves the weights from a Modelplane ModelCache on shared cluster
	// storage instead of downloading them on every start. Whether that is faster
	// depends on the storage and the loader, so it is a switch to measure in
	// rehearsal rather than a decision made here.
	Cache bool
}

// CacheName is the ModelCache holding a config's weights: one per precision,
// since question 1 is the only answer that changes which weights load.
func CacheName(c Config) string {
	if c[0] == B {
		return "qwen3-8b-fp8"
	}
	return "qwen3-8b"
}

// Args returns the vLLM flags for a config. Everything that differs between the
// eight deployments is in here or in the GPU count, never elsewhere.
func Args(c Config, o Options) []string {
	model := Model16
	if c[0] == B {
		// Pre-quantized weights: half the download and no conversion at load.
		model = Model8
	}
	// vLLM sizes the KV cache from what's left of this share after a trial
	// run, then captures its CUDA graphs. One engine split across two GPUs
	// needs more for those graphs and for the GPUs' communication than the
	// trial shows: at 0.90, FP8 split across two L4s fills the GPUs with KV
	// cache and runs out of memory capturing them, every start.
	utilization := "0.90"
	if c[1] == A {
		utilization = "0.85"
	}
	args := []string{
		"--model=" + model,
		"--served-model-name=$(MODELPLANE_SERVED_MODEL_NAME)",
		"--max-model-len=8192",
		"--gpu-memory-utilization=" + utilization,
		`--default-chat-template-kwargs={"enable_thinking": false}`,
	}
	if o.Cache {
		// The default loader reads shared storage slowly enough to lose to a
		// fresh download; the streamer reads it in parallel.
		args = append(args, "--load-format=runai_streamer")
	}
	if c[0] == B {
		args = append(args, "--kv-cache-dtype=fp8")
	}
	if c[1] == A {
		args = append(args, "--tensor-parallel-size=2")
	}
	if c[2] == B {
		// N-gram speculation: guesses come from matching the last few tokens
		// against earlier in the conversation, which agent workloads repeat a
		// lot. A draft model would guess better, but its weights and its own KV
		// cache don't fit next to full-precision Qwen3-8B on one L4: that
		// combination is left with 0.25 GiB of KV cache and won't start.
		args = append(args, `--speculative-config={"method": "ngram", "num_speculative_tokens": 4, "prompt_lookup_min": 2, "prompt_lookup_max": 4}`)
	}
	return args
}

// Deployment returns the ModelDeployment for a config as a JSON-ready object.
// Both topologies use two GPUs and one ModelReplica, so the comparison is always
// two GPUs against two GPUs: one engine holding both, or two copies holding one
// each. Copies share the replica's endpoint picker, which is what lets it route
// by prefix cache between them.
func Deployment(name string, c Config, o Options) map[string]any {
	copies, gpus := 1, 2
	if c[1] == B {
		copies, gpus = 2, 1
	}
	spec := map[string]any{
		"engines": []any{map[string]any{
			"name":   "qwen3-8b",
			"copies": copies,
			"members": []any{map[string]any{
				"role": "Standalone",
				"nodeSelector": map[string]any{
					"devices": []any{map[string]any{
						"name":  "gpu",
						"count": gpus,
						"selectors": []any{map[string]any{
							"cel": `device.capacity["gpu.nvidia.com"].memory.compareTo(quantity("20Gi")) >= 0`,
						}},
					}},
				},
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{map[string]any{
							"name":  "engine",
							"image": Image,
							"args":  Args(c, o),
						}},
					},
				},
			}},
		}},
	}
	if o.Cache {
		spec["modelCacheRef"] = map[string]any{"name": CacheName(c)}
	}
	return map[string]any{
		"apiVersion": "modelplane.ai/v1alpha1",
		"kind":       "ModelDeployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": Namespace,
			"labels": map[string]any{
				"inference-vote/config": c.String(),
			},
		},
		"spec": map[string]any{
			"replicas": 1,
			"template": map[string]any{"spec": spec},
		},
	}
}

// Manifest renders a config's ModelDeployment as JSON, which kubectl and the
// Kubernetes API both accept wherever they accept YAML.
func Manifest(name string, c Config, o Options) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// Keep the CEL selector's ">=" readable instead of escaping it.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(Deployment(name, c, o))
	return bytes.TrimSpace(b.Bytes()), err
}

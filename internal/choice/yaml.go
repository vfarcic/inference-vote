package choice

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// YAML renders a config's ModelDeployment the way people write one: the same
// object the bridge applies, in YAML, with the fields in the usual Kubernetes
// order rather than alphabetically, and a comment on what each setting is for.
// It is the file the setup applies for the optimized deployment, and the one
// the race screen shows for every deployment.
func YAML(name string, c Config, o Options) string {
	var b strings.Builder
	writeYAML(&b, Deployment(name, c, o), 0)
	return strings.TrimRight(b.String(), "\n")
}

// keyOrder puts the fields of a ModelDeployment in reading order. Anything not
// listed follows, alphabetically.
var keyOrder = []string{
	"apiVersion", "kind", "metadata", "name", "namespace", "labels", "spec",
	"replicas", "modelCacheRef", "engines", "copies", "members", "role",
	"nodeSelector", "devices", "count", "selectors", "cel", "template", "containers", "image", "args",
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		if i := slices.Index(keyOrder, k); i >= 0 {
			return i
		}
		return len(keyOrder)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	return keys
}

// keyComment says what a field is for, from its name and value. Fields that
// explain themselves get none.
func keyComment(key string, v any) string {
	switch key {
	case "labels":
		return "The three answers, one letter per question"
	case "replicas":
		return "ModelReplicas: each one holds the engines below and gets its own endpoint picker"
	case "modelCacheRef":
		return "Weights come from a ModelCache on shared storage instead of a download on every start"
	case "engines":
		return "What runs in each replica"
	case "copies":
		if v == 2 {
			return "Question 2: two copies of the engine, and the endpoint picker routes between them"
		}
		return "Question 2: a single engine"
	case "role":
		return "One pod does all the work: no separate prefill and decode"
	case "devices":
		return "Requested through Dynamic Resource Allocation"
	case "count":
		if v == 2 {
			return "Question 2: two GPUs, for the one engine split across them"
		}
		return "One GPU for each copy"
	case "cel":
		return "Any GPU with at least 20 GiB, which is an L4 here"
	case "image":
		return "vLLM, the inference engine"
	case "args":
		return "vLLM's flags: everything the three answers change, except the GPU count, is here"
	}
	return ""
}

// argComment says what a vLLM flag does.
func argComment(arg string) string {
	switch {
	case arg == "--model="+Model16:
		return "Question 1: the weights as trained, 16-bit, about 16 GB"
	case arg == "--model="+Model8:
		return "Question 1: FP8 weights, half the memory, which the KV cache gets instead"
	case strings.HasPrefix(arg, "--served-model-name="):
		return "The name the gateway routes by, filled in by Modelplane"
	case strings.HasPrefix(arg, "--max-model-len="):
		return "The longest conversation, in tokens"
	case strings.HasPrefix(arg, "--gpu-memory-utilization=0.85"):
		return "The share of GPU memory vLLM takes, a little less when split, for the GPUs' communication"
	case strings.HasPrefix(arg, "--gpu-memory-utilization="):
		return "The share of GPU memory vLLM takes; the KV cache gets what the weights leave"
	case strings.HasPrefix(arg, "--default-chat-template-kwargs="):
		return "Qwen3 answers without its thinking step"
	case strings.HasPrefix(arg, "--load-format="):
		return "Reads the cached weights in parallel"
	case strings.HasPrefix(arg, "--tensor-parallel-size="):
		return "Question 2: one engine split across both GPUs (tensor parallelism)"
	case strings.HasPrefix(arg, "--speculative-config="):
		return "Question 3: guess the next tokens from earlier in the conversation, and let the model check them"
	}
	return ""
}

func writeComment(b *strings.Builder, pad, comment string) {
	if comment != "" {
		fmt.Fprintf(b, "%s# %s\n", pad, comment)
	}
}

func writeYAML(b *strings.Builder, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch v := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(v) {
			writeComment(b, pad, keyComment(k, v[k]))
			switch child := v[k].(type) {
			case map[string]any:
				fmt.Fprintf(b, "%s%s:\n", pad, k)
				writeYAML(b, child, indent+1)
			case []any, []string:
				// Lists sit at their key's indentation, as kubectl writes them.
				fmt.Fprintf(b, "%s%s:\n", pad, k)
				writeYAML(b, child, indent)
			default:
				fmt.Fprintf(b, "%s%s: %s\n", pad, k, scalar(child))
			}
		}
	case []any:
		for _, item := range v {
			writeItem(b, item, indent)
		}
	case []string:
		for _, item := range v {
			writeItem(b, item, indent)
		}
	}
}

// writeItem writes one list entry. A map's first field goes on the dash line,
// the rest indented under it, which is how lists of objects are usually written.
// A comment on that first field goes above the dash.
func writeItem(b *strings.Builder, item any, indent int) {
	pad := strings.Repeat("  ", indent)
	m, ok := item.(map[string]any)
	if !ok {
		if s, ok := item.(string); ok {
			writeComment(b, pad, argComment(s))
		}
		fmt.Fprintf(b, "%s- %s\n", pad, scalar(item))
		return
	}
	var inner strings.Builder
	writeYAML(&inner, m, indent+1)
	lines := strings.SplitAfter(inner.String(), "\n")
	inPad := strings.Repeat("  ", indent+1)
	i := 0
	for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "#"); i++ {
		b.WriteString(pad + strings.TrimPrefix(lines[i], inPad))
	}
	fmt.Fprintf(b, "%s- %s", pad, strings.TrimPrefix(lines[i], inPad))
	for _, l := range lines[i+1:] {
		b.WriteString(l)
	}
}

// scalar quotes a string only when YAML would read it as something else.
func scalar(v any) string {
	s, ok := v.(string)
	if !ok {
		out, _ := json.Marshal(v)
		return string(out)
	}
	if s == "" {
		return "''"
	}
	_, numeric := strconv.ParseFloat(s, 64)
	// "-", "?" and ":" only start something else when a space follows them.
	indicator := strings.ContainsAny(s[:1], `,[]{}#&*!|>'"%@`+"`") ||
		(strings.ContainsAny(s[:1], "-?:") && (len(s) == 1 || s[1] == ' '))
	plain := numeric != nil && !indicator &&
		!strings.Contains(s, ": ") && !strings.Contains(s, " #") &&
		!slices.Contains([]string{"true", "false", "null", "yes", "no", "on", "off", "~"}, strings.ToLower(s))
	if plain {
		return s
	}
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	out, _ := json.Marshal(s)
	return string(out)
}

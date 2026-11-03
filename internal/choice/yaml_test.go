package choice

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestYAML(t *testing.T) {
	c, _ := Parse("ABB")
	y := YAML("audience", c, Options{})
	for _, want := range []string{
		"apiVersion: modelplane.ai/v1alpha1\nkind: ModelDeployment\nmetadata:\n  name: audience",
		"      engines:\n      - name: qwen3-8b\n        # Question 2: two copies of the engine, and the endpoint picker routes between them\n        copies: 2\n",
		"                args:\n                # Question 1: the weights as trained, 16-bit, about 16 GB\n                - --model=" + Model16 + "\n",
		`'--speculative-config={"method": "ngram"`,
	} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in\n%s", want, y)
		}
	}
}

// The YAML is applied, not only shown, so it must say exactly what the object
// says, comments and all, for every config.
func TestYAMLParses(t *testing.T) {
	for _, s := range []string{"AAA", "AAB", "ABA", "ABB", "BAA", "BAB", "BBA", "BBB"} {
		c, _ := Parse(s)
		for _, o := range []Options{{}, {Cache: true}} {
			var fromYAML, fromJSON any
			if err := yaml.Unmarshal([]byte(YAML("audience", c, o)), &fromYAML); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
			j, _ := json.Marshal(Deployment("audience", c, o))
			json.Unmarshal(j, &fromJSON)
			y, _ := json.Marshal(fromYAML)
			want, _ := json.Marshal(fromJSON)
			if string(y) != string(want) {
				t.Errorf("%s %+v: the YAML says\n%s\nthe object\n%s", s, o, y, want)
			}
		}
	}
}

func TestYAMLComments(t *testing.T) {
	c, _ := Parse("BAB")
	y := YAML("audience", c, Options{})
	for _, want := range []string{
		"        # Question 2: a single engine\n        copies: 1\n",
		"        # One pod does all the work: no separate prefill and decode\n        - role: Standalone\n",
		"                # Question 1: FP8 weights, half the memory, which the KV cache gets instead\n                - --model=" + Model8 + "\n",
		"# Question 2: one engine split across both GPUs (tensor parallelism)\n",
		"# Question 3: guess the next tokens",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in\n%s", want, y)
		}
	}
}

package choice

import (
	"strings"
	"testing"
)

func TestYAML(t *testing.T) {
	c, _ := Parse("ABB")
	y := YAML("audience", c, Options{})
	for _, want := range []string{
		"apiVersion: modelplane.ai/v1alpha1\nkind: ModelDeployment\nmetadata:\n  name: audience",
		"      engines:\n      - name: qwen3-8b\n        copies: 2\n        members:\n        - role: Standalone\n",
		"                args:\n                - --model=" + Model16 + "\n",
		`'--speculative-config={"method": "ngram"`,
	} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in\n%s", want, y)
		}
	}
}

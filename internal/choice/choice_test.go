package choice

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	c, err := Parse(" bba ")
	if err != nil || c.String() != "BBA" {
		t.Fatalf("Parse(bba) = %v, %v", c, err)
	}
	for _, bad := range []string{"", "AB", "ABCD", "ABC"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestOpposite(t *testing.T) {
	c, _ := Parse("ABA")
	if got := c.Opposite().String(); got != "BAB" {
		t.Fatalf("Opposite(ABA) = %s", got)
	}
}

func TestArgs(t *testing.T) {
	has := func(args []string, prefix string) bool {
		return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, prefix) })
	}
	cases := []struct {
		config      string
		want, avoid []string
	}{
		{"AAA", []string{"--model=" + Model16, "--tensor-parallel-size=2"}, []string{"--kv-cache-dtype", "--speculative-config"}},
		{"BBB", []string{"--model=" + Model8, "--kv-cache-dtype=fp8", "--speculative-config="}, []string{"--tensor-parallel-size"}},
	}
	for _, tc := range cases {
		c, _ := Parse(tc.config)
		args := Args(c, Options{})
		for _, w := range tc.want {
			if !has(args, w) {
				t.Errorf("%s: missing %s in %v", tc.config, w, args)
			}
		}
		for _, a := range tc.avoid {
			if has(args, a) {
				t.Errorf("%s: unexpected %s in %v", tc.config, a, args)
			}
		}
	}
}

// Every config uses exactly two GPUs, whichever way it splits them.
func TestTwoGPUsAlways(t *testing.T) {
	for _, s := range []string{"AAA", "ABA", "BAB", "BBB"} {
		c, _ := Parse(s)
		engine := Deployment("x", c, Options{})["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["engines"].([]any)[0].(map[string]any)
		copies := engine["copies"].(int)
		gpus := engine["members"].([]any)[0].(map[string]any)["nodeSelector"].(map[string]any)["devices"].([]any)[0].(map[string]any)["count"].(int)
		if copies*gpus != 2 {
			t.Errorf("%s: %d copies x %d GPUs", s, copies, gpus)
		}
	}
}

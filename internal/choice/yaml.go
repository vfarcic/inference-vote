package choice

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// YAML renders a config's ModelDeployment the way people write one, for the
// race screen: the same object the bridge applies, in YAML, with the fields in
// the usual Kubernetes order rather than alphabetically.
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

func writeYAML(b *strings.Builder, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch v := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(v) {
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
func writeItem(b *strings.Builder, item any, indent int) {
	pad := strings.Repeat("  ", indent)
	m, ok := item.(map[string]any)
	if !ok {
		fmt.Fprintf(b, "%s- %s\n", pad, scalar(item))
		return
	}
	var inner strings.Builder
	writeYAML(&inner, m, indent+1)
	lines := strings.SplitAfter(inner.String(), "\n")
	first := strings.TrimPrefix(lines[0], strings.Repeat("  ", indent+1))
	fmt.Fprintf(b, "%s- %s", pad, first)
	for _, l := range lines[1:] {
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

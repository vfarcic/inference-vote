// Package stack shows what one ModelDeployment became: the resources
// Modelplane composed from it, what controllers then created from those (pods,
// their GPU claims, endpoints, the gateway's own routes), and what vLLM did
// inside each engine. Both deployments go through it, so the race screen can
// put them side by side and mark only what differs.
package stack

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vfarcic/inference-vote/internal/diff"
)

// Object is one Kubernetes resource, as the API returned it.
type Object map[string]any

func (o Object) str(path ...string) string {
	var v any = map[string]any(o)
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[p]
	}
	s, _ := v.(string)
	return s
}

func (o Object) APIVersion() string { return o.str("apiVersion") }
func (o Object) Kind() string       { return o.str("kind") }
func (o Object) Name() string       { return o.str("metadata", "name") }

// owned reports whether a controller created o from another resource.
func (o Object) owned() bool {
	md, _ := o["metadata"].(map[string]any)
	refs, _ := md["ownerReferences"].([]any)
	return len(refs) > 0
}

// Belongs reports whether a resource in Modelplane's namespace is one of
// deployment's: everything Modelplane and the controllers create for it is
// named after it, at the start or, for the AI gateway's filters, the end.
func Belongs(name, deployment string) bool {
	return name == deployment || strings.HasPrefix(name, deployment+"-") || strings.HasSuffix(name, "-"+deployment)
}

// Split sorts a deployment's resources into what Modelplane composed and what
// controllers created from that.
func Split(objs []Object) (composed, created []Object) {
	for _, o := range objs {
		if o.owned() {
			created = append(created, o)
		} else {
			composed = append(composed, o)
		}
	}
	sortObjects(composed)
	sortObjects(created)
	return composed, created
}

func sortObjects(objs []Object) {
	sort.SliceStable(objs, func(i, j int) bool {
		if objs[i].Kind() != objs[j].Kind() {
			return objs[i].Kind() < objs[j].Kind()
		}
		return objs[i].Name() < objs[j].Name()
	})
}

// Generated parts of names: the hash Modelplane adds per deployment, per
// resource, and the ones Kubernetes adds to ReplicaSets.
var (
	shortHash = regexp.MustCompile(`-[0-9a-f]{5}\b`)
	longHash  = regexp.MustCompile(`-[0-9a-f]{8,10}\b`)
)

const placeholder = "<deployment>"

// Key is what a resource is, whichever deployment it belongs to: its name with
// the deployment's name and the generated hashes taken out.
func Key(name, deployment string) string {
	return generic(name, deployment)
}

func generic(s, deployment string) string {
	if !strings.Contains(s, deployment) {
		return s
	}
	s = strings.ReplaceAll(s, deployment, placeholder)
	s = longHash.ReplaceAllString(s, "")
	return shortHash.ReplaceAllString(s, "")
}

// Normalize is a resource as both deployments could have it: without what the
// API adds (status, UIDs, versions, timestamps), and with the deployment's name
// and generated hashes replaced, so only real differences remain.
func Normalize(o Object, deployment string) Object {
	out, _ := scrub(map[string]any(o), deployment, 0).(map[string]any)
	delete(out, "status")
	// The API server assigns a Service its IPs; they always differ.
	if spec, ok := out["spec"].(map[string]any); ok && o.Kind() == "Service" {
		delete(spec, "clusterIP")
		delete(spec, "clusterIPs")
	}
	if md, ok := out["metadata"].(map[string]any); ok {
		for _, k := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "ownerReferences", "selfLink", "deletionTimestamp", "deletionGracePeriodSeconds"} {
			delete(md, k)
		}
		if an, ok := md["annotations"].(map[string]any); ok {
			for _, k := range []string{"deployment.kubernetes.io/revision", "kubectl.kubernetes.io/last-applied-configuration"} {
				delete(an, k)
			}
			if len(an) == 0 {
				delete(md, "annotations")
			}
		}
	}
	return out
}

func scrub(v any, deployment string, depth int) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[generic(k, deployment)] = scrub(val, deployment, depth+1)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, val := range t {
			s[i] = scrub(val, deployment, depth+1)
		}
		return s
	case string:
		return generic(t, deployment)
	}
	return v
}

// Pair is one composed resource on both sides.
type Pair struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Key        string     `json:"key"`
	Optimized  string     `json:"optimized,omitempty"` // its real name, empty if only the audience has it
	Audience   string     `json:"audience,omitempty"`
	State      string     `json:"state"`          // same, differs, optimized (only), audience (only)
	Rows       []diff.Row `json:"rows,omitempty"` // for differs: the two, line by line, unchanged runs folded
}

// Compare pairs the two deployments' composed resources by what they are, and
// diffs the ones that differ.
func Compare(optimized, audience []Object) []Pair {
	type side struct {
		obj  Object
		yaml []string
	}
	index := func(objs []Object, deployment string) map[string]side {
		m := map[string]side{}
		for _, o := range objs {
			k := o.APIVersion() + " " + o.Kind() + " " + Key(o.Name(), deployment)
			m[k] = side{o, strings.Split(YAML(Normalize(o, deployment)), "\n")}
		}
		return m
	}
	opt, aud := index(optimized, "optimized"), index(audience, "audience")
	keys := map[string]bool{}
	for k := range opt {
		keys[k] = true
	}
	for k := range aud {
		keys[k] = true
	}
	var pairs []Pair
	for k := range keys {
		o, inO := opt[k]
		a, inA := aud[k]
		ref := o.obj
		if !inO {
			ref = a.obj
		}
		p := Pair{APIVersion: ref.APIVersion(), Kind: ref.Kind(), Key: strings.SplitN(k, " ", 3)[2]}
		switch {
		case inO && inA:
			p.Optimized, p.Audience = o.obj.Name(), a.obj.Name()
			if strings.Join(o.yaml, "\n") == strings.Join(a.yaml, "\n") {
				p.State = "same"
			} else {
				p.State, p.Rows = "differs", Fold(diff.Lines(o.yaml, a.yaml), 2)
			}
		case inO:
			p.Optimized, p.State = o.obj.Name(), "optimized"
		default:
			p.Audience, p.State = a.obj.Name(), "audience"
		}
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Kind != pairs[j].Kind {
			return pairs[i].Kind < pairs[j].Kind
		}
		return pairs[i].Key < pairs[j].Key
	})
	return pairs
}

// Fold replaces each run of unchanged lines with one row saying how many there
// were, keeping context lines on either side of a change. apiVersion and kind
// always stay: they say what the resource is.
func Fold(rows []diff.Row, context int) []diff.Row {
	keep := make([]bool, len(rows))
	for i, r := range rows {
		if r.Kind != "same" || strings.HasPrefix(r.Left, "apiVersion:") || strings.HasPrefix(r.Left, "kind:") {
			keep[i] = true
			if r.Kind != "same" {
				for j := max(0, i-context); j <= min(len(rows)-1, i+context); j++ {
					keep[j] = true
				}
			}
		}
	}
	var out []diff.Row
	for i := 0; i < len(rows); {
		if keep[i] {
			out = append(out, rows[i])
			i++
			continue
		}
		j := i
		for j < len(rows) && !keep[j] {
			j++
		}
		label := fmt.Sprintf("... %d unchanged lines", j-i)
		if j-i == 1 {
			label = "... 1 unchanged line"
		}
		out = append(out, diff.Row{Left: label, Right: label, Kind: "fold"})
		i = j
	}
	return out
}

// YAML writes a resource the way people write them: apiVersion, kind,
// metadata and spec first, the rest of every map in key order.
func YAML(o Object) string {
	var b strings.Builder
	writeMap(&b, map[string]any(o), 0, []string{"apiVersion", "kind", "metadata", "spec", "data"})
	return strings.TrimRight(b.String(), "\n")
}

func writeMap(b *strings.Builder, m map[string]any, indent int, first []string) {
	for _, k := range orderedKeys(m, first) {
		writeEntry(b, k, m[k], indent)
	}
}

func orderedKeys(m map[string]any, first []string) []string {
	var keys, rest []string
	for _, k := range first {
		if _, ok := m[k]; ok {
			keys = append(keys, k)
		}
	}
	for k := range m {
		if !contains(first, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func writeEntry(b *strings.Builder, k string, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			fmt.Fprintf(b, "%s%s: {}\n", pad, scalar(k))
			return
		}
		fmt.Fprintf(b, "%s%s:\n", pad, scalar(k))
		writeMap(b, t, indent+1, []string{"name", "namespace", "labels", "annotations"})
	case []any:
		if len(t) == 0 {
			fmt.Fprintf(b, "%s%s: []\n", pad, scalar(k))
			return
		}
		fmt.Fprintf(b, "%s%s:\n", pad, scalar(k))
		for _, item := range t {
			writeItem(b, item, indent)
		}
	default:
		fmt.Fprintf(b, "%s%s: %s\n", pad, scalar(k), scalar(v))
	}
}

// writeItem writes a list item under a key at indent, in the "- " style.
func writeItem(b *strings.Builder, v any, indent int) {
	pad := strings.Repeat("  ", indent)
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		if _, isList := v.([]any); isList {
			out, _ := json.Marshal(v)
			fmt.Fprintf(b, "%s- %s\n", pad, out)
			return
		}
		fmt.Fprintf(b, "%s- %s\n", pad, scalar(v))
		return
	}
	var item strings.Builder
	writeMap(&item, m, indent+1, []string{"name", "kind", "apiVersion"})
	lines := strings.Split(strings.TrimRight(item.String(), "\n"), "\n")
	inner := strings.Repeat("  ", indent+1)
	for i, l := range lines {
		if i == 0 {
			fmt.Fprintf(b, "%s- %s\n", pad, strings.TrimPrefix(l, inner))
		} else {
			fmt.Fprintf(b, "%s\n", l)
		}
	}
}

var plain = regexp.MustCompile(`^[A-Za-z0-9_./<>()$=+-][A-Za-z0-9_./<>()$=+ ,:@-]*$`)

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		reserved := map[string]bool{"true": true, "false": true, "null": true, "yes": true, "no": true, "": true}
		_, isNumber := strconv.ParseFloat(t, 64)
		if reserved[strings.ToLower(t)] || isNumber == nil || !plain.MatchString(t) || strings.Contains(t, ": ") || strings.HasSuffix(t, ":") {
			out, _ := json.Marshal(t)
			return string(out)
		}
		return t
	}
	out, _ := json.Marshal(v)
	return string(out)
}

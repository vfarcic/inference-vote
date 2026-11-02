package stack

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Created is a resource a controller made from a composed one, reduced to the
// few facts worth reading: a pod's node and state, which GPUs a claim got.
type Created struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Facts      []string `json:"facts,omitempty"`
}

// Summarize describes what controllers created for a deployment, including
// the Secret cert-manager issues for each of its Certificates. The app never
// reads Secrets, which hold credentials, so those come from the Certificates'
// secretName rather than from the API.
func Summarize(composed, created []Object, deployment string) []Created {
	var out []Created
	for _, o := range created {
		out = append(out, Created{APIVersion: o.APIVersion(), Kind: o.Kind(), Key: Key(o.Name(), deployment), Name: o.Name(), Facts: facts(o)})
	}
	for _, o := range composed {
		if o.Kind() != "Certificate" {
			continue
		}
		if name := o.str("spec", "secretName"); name != "" {
			out = append(out, Created{APIVersion: "v1", Kind: "Secret", Key: Key(name, deployment), Name: name, Facts: []string{"issued by cert-manager for the Certificate", "contents never read"}})
		}
	}
	return out
}

func facts(o Object) []string {
	switch o.Kind() {
	case "Pod":
		var f []string
		if n := o.str("spec", "nodeName"); n != "" {
			f = append(f, "on "+n)
		} else {
			f = append(f, "not scheduled yet")
		}
		statuses, _ := dig(o, "status", "containerStatuses").([]any)
		if len(statuses) > 0 {
			cs, _ := statuses[0].(map[string]any)
			if ready, _ := cs["ready"].(bool); ready {
				f = append(f, "ready")
			} else {
				f = append(f, "starting")
			}
			if r, _ := cs["restartCount"].(float64); r > 0 {
				f = append(f, fmt.Sprintf("restarted %d times", int(r)))
			}
		}
		return f
	case "ResourceClaim":
		results, _ := dig(o, "status", "allocation", "devices", "results").([]any)
		if len(results) == 0 {
			return []string{"no GPU allocated yet"}
		}
		var f []string
		for _, r := range results {
			m, _ := r.(map[string]any)
			f = append(f, fmt.Sprintf("%v on %v", m["device"], m["pool"]))
		}
		return f
	case "ReplicaSet":
		ready, _ := dig(o, "status", "readyReplicas").(float64)
		want, _ := dig(o, "spec", "replicas").(float64)
		return []string{fmt.Sprintf("%d of %d pods ready", int(ready), int(want))}
	case "EndpointSlice":
		eps, _ := o["endpoints"].([]any)
		return []string{fmt.Sprintf("%d endpoints", len(eps))}
	case "CertificateRequest":
		conds, _ := dig(o, "status", "conditions").([]any)
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] == "Ready" {
				return []string{fmt.Sprintf("ready: %v", m["status"])}
			}
		}
	}
	return nil
}

func dig(o Object, path ...string) any {
	var v any = map[string]any(o)
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// Engine is what vLLM did inside one engine pod: what it loaded, where, and
// how much room that left for conversations.
type Engine struct {
	Pod            string    `json:"pod"`
	Node           string    `json:"node"`
	GPUs           []string  `json:"gpus"`
	Model          string    `json:"model"`
	TensorParallel int       `json:"tensorParallel"`
	WeightsGiB     []float64 `json:"weightsGiB"`    // per GPU, once loaded
	KVCacheGiB     float64   `json:"kvCacheGiB"`    // per GPU, once sized
	KVCacheTokens  int       `json:"kvCacheTokens"` // the whole engine
	Conversations  float64   `json:"conversations"` // full-length ones that fit at once
	KVCacheDtype   string    `json:"kvCacheDtype"`  // as vLLM reports it: auto (the model's own) or fp8
	Ready          bool      `json:"ready"`
}

var (
	weightsLine = regexp.MustCompile(`Model loading took ([0-9.]+) GiB`)
	kvLine      = regexp.MustCompile(`Available KV cache memory: ([0-9.]+) GiB`)
	tokensLine  = regexp.MustCompile(`GPU KV cache size: ([0-9,]+) tokens`)
	concLine    = regexp.MustCompile(`Maximum concurrency for [0-9,]+ tokens per request: ([0-9.]+)x`)
	dtypeLabel  = regexp.MustCompile(`vllm:cache_config_info\{[^}]*cache_dtype="([^"]*)"`)
)

// Engines describes a deployment's engine pods from what controllers created
// (the pods and their GPU claims), each pod's startup log, and its metrics.
// Logs and metrics are keyed by pod name; a missing one leaves its facts out.
func Engines(created []Object, logs, metrics map[string]string) []Engine {
	gpus := map[string][]string{} // pod name -> allocated devices
	for _, o := range created {
		if o.Kind() != "ResourceClaim" {
			continue
		}
		owner := ""
		if md, ok := o["metadata"].(map[string]any); ok {
			if refs, ok := md["ownerReferences"].([]any); ok && len(refs) > 0 {
				ref, _ := refs[0].(map[string]any)
				owner, _ = ref["name"].(string)
			}
		}
		results, _ := dig(o, "status", "allocation", "devices", "results").([]any)
		for _, r := range results {
			m, _ := r.(map[string]any)
			gpus[owner] = append(gpus[owner], fmt.Sprint(m["device"]))
		}
	}
	var out []Engine
	for _, o := range created {
		if o.Kind() != "Pod" || !isEngine(o) {
			continue
		}
		e := Engine{Pod: o.Name(), Node: o.str("spec", "nodeName"), GPUs: gpus[o.Name()], TensorParallel: 1}
		sort.Strings(e.GPUs)
		containers, _ := dig(o, "spec", "containers").([]any)
		if len(containers) > 0 {
			c, _ := containers[0].(map[string]any)
			args, _ := c["args"].([]any)
			for _, a := range args {
				s, _ := a.(string)
				if v, ok := strings.CutPrefix(s, "--model="); ok {
					e.Model = v
				}
				if v, ok := strings.CutPrefix(s, "--tensor-parallel-size="); ok {
					e.TensorParallel, _ = strconv.Atoi(v)
				}
			}
		}
		if statuses, _ := dig(o, "status", "containerStatuses").([]any); len(statuses) > 0 {
			cs, _ := statuses[0].(map[string]any)
			e.Ready, _ = cs["ready"].(bool)
		}
		log := logs[o.Name()]
		for _, m := range weightsLine.FindAllStringSubmatch(log, -1) {
			v, _ := strconv.ParseFloat(m[1], 64)
			e.WeightsGiB = append(e.WeightsGiB, v)
		}
		if m := kvLine.FindStringSubmatch(log); m != nil {
			e.KVCacheGiB, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := tokensLine.FindStringSubmatch(log); m != nil {
			e.KVCacheTokens, _ = strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		}
		if m := concLine.FindStringSubmatch(log); m != nil {
			e.Conversations, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := dtypeLabel.FindStringSubmatch(metrics[o.Name()]); m != nil {
			e.KVCacheDtype = m[1]
		}
		out = append(out, e)
	}
	return out
}

// Sized reports whether vLLM has logged how big its KV cache is, the last
// fact Engines reads from a log; until then the log is worth reading again.
func Sized(log string) bool { return concLine.MatchString(log) }

// StartupLines keeps only the lines of an engine's log that Engines reads.
func StartupLines(log string) string {
	var keep []string
	for _, l := range strings.Split(log, "\n") {
		if weightsLine.MatchString(l) || kvLine.MatchString(l) || tokensLine.MatchString(l) || concLine.MatchString(l) {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

func isEngine(o Object) bool {
	md, _ := o["metadata"].(map[string]any)
	labels, _ := md["labels"].(map[string]any)
	_, ok := labels["modelplane.ai/engine"]
	return ok
}

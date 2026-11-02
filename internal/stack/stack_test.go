package stack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The testdata comes from a real cluster: B, A, A as optimized (one engine on
// two L4s) against B, B, A (two copies, one L4 each), deployed through the app.
func load(t *testing.T) (optimized, audience []Object) {
	t.Helper()
	b, err := os.ReadFile("testdata/namespace.json")
	if err != nil {
		t.Fatal(err)
	}
	var list struct{ Items []Object }
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	for _, o := range list.Items {
		switch {
		case Belongs(o.Name(), "optimized"):
			optimized = append(optimized, o)
		case Belongs(o.Name(), "audience"):
			audience = append(audience, o)
		}
	}
	return optimized, audience
}

func TestSplit(t *testing.T) {
	opt, aud := load(t)
	oc, ocr := Split(opt)
	ac, acr := Split(aud)
	if len(oc) != 15 || len(ac) != 15 {
		t.Errorf("composed: %d and %d, want 15 each", len(oc), len(ac))
	}
	// One engine pod and its claim against two of each.
	if len(acr)-len(ocr) != 2 {
		t.Errorf("created: %d and %d, want the audience to have two more", len(ocr), len(acr))
	}
}

func TestKey(t *testing.T) {
	for name, want := range map[string]string{
		"audience-4cd7f-qwen3-8b-303a3":                    "<deployment>-qwen3-8b",
		"optimized-a7558-qwen3-8b-3a00f":                   "<deployment>-qwen3-8b",
		"audience-4cd7f-qwen3-8b-standalone-devices-3cc3b": "<deployment>-qwen3-8b-standalone-devices",
		"audience-public-7d752-audience-4cd7f-87886":       "<deployment>-public-<deployment>",
		"audience-4cd7f-qwen3-8b-303a3-5975655f47":         "<deployment>-qwen3-8b",
		"ai-eg-host-rewrite-audience":                      "ai-eg-host-rewrite-<deployment>",
		"audience":                                         "<deployment>",
	} {
		dep := "audience"
		if strings.HasPrefix(name, "optimized") {
			dep = "optimized"
		}
		if got := Key(name, dep); got != want {
			t.Errorf("Key(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCompare(t *testing.T) {
	opt, aud := load(t)
	oc, _ := Split(opt)
	ac, _ := Split(aud)
	states := map[string]Pair{}
	for _, p := range Compare(oc, ac) {
		states[p.Kind+" "+p.Key] = p
	}
	if len(states) != 15 {
		t.Errorf("%d pairs, want the 15 composed resources paired", len(states))
	}
	engine := states["Deployment <deployment>-qwen3-8b"]
	if engine.State != "differs" {
		t.Fatalf("engine Deployment: %q, want differs", engine.State)
	}
	var changed []string
	for _, r := range engine.Rows {
		if r.Kind != "same" && r.Kind != "fold" {
			changed = append(changed, strings.TrimSpace(r.Left)+" | "+strings.TrimSpace(r.Right))
		}
	}
	text := strings.Join(changed, "\n")
	// Two copies, a single GPU each, no tensor parallelism: the answer to
	// question 2, all in the engine Deployment.
	for _, want := range []string{"replicas: 1 | replicas: 2", "--tensor-parallel-size=2"} {
		if !strings.Contains(text, want) {
			t.Errorf("engine diff lacks %q:\n%s", want, text)
		}
	}
	if rows := engine.Rows; len(rows) == 0 || !strings.HasPrefix(rows[0].Left, "apiVersion:") || !strings.HasPrefix(rows[1].Left, "kind:") {
		t.Errorf("apiVersion and kind must stay at the top:\n%v", rows[:min(3, len(rows))])
	}
	if gpus := states["ResourceClaimTemplate <deployment>-qwen3-8b-standalone-devices"]; gpus.State != "differs" {
		t.Errorf("the GPU request should differ, 2 GPUs against 1: %q", gpus.State)
	}
	// Only real differences: the engine and its GPU request. A Service's
	// assigned IP is not one.
	var differ []string
	for k, p := range states {
		if p.State != "same" {
			differ = append(differ, k)
		}
	}
	if len(differ) != 2 {
		t.Errorf("differing: %v, want only the engine Deployment and its ResourceClaimTemplate", differ)
	}
	if sa := states["ServiceAccount <deployment>-epp"]; sa.State != "same" || sa.Rows != nil {
		t.Errorf("the endpoint picker's ServiceAccount should be the same and carry no rows: %+v", sa)
	}
}

func TestFoldKeepsKindAndAPIVersion(t *testing.T) {
	opt, aud := load(t)
	oc, _ := Split(opt)
	ac, _ := Split(aud)
	for _, p := range Compare(oc, ac) {
		for _, r := range p.Rows {
			if r.Kind == "fold" && (strings.Contains(r.Left, "apiVersion") || strings.Contains(r.Left, "kind:")) {
				t.Errorf("%s %s: folded %q", p.Kind, p.Key, r.Left)
			}
		}
	}
}

func TestSummarize(t *testing.T) {
	_, aud := load(t)
	composed, created := Split(aud)
	var claims, pods, secrets int
	for _, c := range Summarize(composed, created, "audience") {
		switch c.Kind {
		case "Secret":
			secrets++
			if c.Name != "audience-public-7d752-client-cfb42" {
				t.Errorf("secret %q, want the Certificate's", c.Name)
			}
		case "ResourceClaim":
			claims++
			if len(c.Facts) != 1 || !strings.HasPrefix(c.Facts[0], "gpu-") {
				t.Errorf("claim %s: %v, want one GPU", c.Name, c.Facts)
			}
		case "Pod":
			pods++
		}
	}
	if claims != 2 || pods != 3 || secrets != 1 { // two engine copies, one endpoint picker, the client certificate
		t.Errorf("claims %d, pods %d, secrets %d", claims, pods, secrets)
	}
}

func TestEngines(t *testing.T) {
	opt, aud := load(t)
	read := func(objs []Object) []Engine {
		_, created := Split(objs)
		logs, metrics := map[string]string{}, map[string]string{}
		for _, o := range created {
			if l, err := os.ReadFile(filepath.Join("testdata/logs", o.Name()+".log")); err == nil {
				logs[o.Name()] = string(l)
			}
			if m, err := os.ReadFile(filepath.Join("testdata/metrics", o.Name()+".txt")); err == nil {
				metrics[o.Name()] = string(m)
			}
		}
		return Engines(created, logs, metrics)
	}
	o := read(opt)
	if len(o) != 1 || o[0].TensorParallel != 2 || len(o[0].GPUs) != 2 || len(o[0].WeightsGiB) != 2 || o[0].WeightsGiB[0] != 4.41 {
		t.Fatalf("optimized: %+v", o)
	}
	if o[0].Conversations != 23.93 || o[0].KVCacheGiB != 13.46 || o[0].KVCacheTokens != 196016 || o[0].KVCacheDtype != "auto" || o[0].Model != "Qwen/Qwen3-8B-FP8" {
		t.Errorf("optimized: %+v", o[0])
	}
	a := read(aud)
	if len(a) != 2 || a[0].TensorParallel != 1 || len(a[0].GPUs) != 1 || a[0].Conversations != 8.51 || a[0].WeightsGiB[0] != 8.8 {
		t.Errorf("audience: %+v", a)
	}
	if !Sized(mustRead(t, "testdata/logs/"+o[0].Pod+".log")) || Sized("Model loading took 4.41 GiB") {
		t.Error("Sized should tell a finished startup from one still loading")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

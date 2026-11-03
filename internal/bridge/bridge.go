// Package bridge runs next to Modelplane on the control plane, which is a kind
// cluster on the presenter's laptop, so nothing on the internet can reach it.
// It reaches out instead: it polls the voting app for the winning config and
// its opposite, applies their ModelDeployments locally, and sends back what the
// control plane reports. Only outbound HTTPS, so it works through any venue network or a
// phone's hotspot.
package bridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/kube"
)

type Bridge struct {
	VoteURL string // the voting app's public address
	Token   string
	// Insecure skips verifying the voting app's certificate. Rehearsals use
	// Let's Encrypt staging, whose certificates nothing trusts; never on stage.
	Insecure bool
	Names    []string // the ModelDeployments to own: audience and opposite
	Options  choice.Options
	Kube     *kube.Client
	Every    time.Duration

	applied map[string]string // name -> config last applied, so an unchanged one is not re-sent
	poked   map[string]string // name -> config whose ModelService was nudged once its replica was ready
	removed map[string]bool   // names deleted because this session doesn't need them
	http    *http.Client
}

func path(kind, name string) string {
	return fmt.Sprintf("/apis/modelplane.ai/v1alpha1/namespaces/%s/%s/%s", choice.Namespace, kind, name)
}

func (b *Bridge) Run(ctx context.Context) {
	b.http = &http.Client{Timeout: 10 * time.Second}
	b.applied, b.poked, b.removed = map[string]string{}, map[string]string{}, map[string]bool{}
	if b.Insecure {
		b.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		log.Print("bridge: not verifying the voting app's certificate (rehearsal)")
	}
	t := time.NewTicker(b.Every)
	defer t.Stop()
	for {
		if err := b.tick(ctx); err != nil {
			log.Printf("bridge: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick applies what the session wants and removes what it doesn't. A session
// whose pick or opposite is the optimized config needs only one of the two:
// the other, left from an earlier session, would hold GPUs and confuse the
// screen, so it goes.
func (b *Bridge) tick(ctx context.Context) error {
	desired, ok, err := b.desired(ctx)
	if err != nil || !ok {
		return err
	}
	reports := map[string]any{}
	for _, name := range b.Names {
		config, want := desired[name]
		if !want {
			if !b.removed[name] {
				if err := b.Kube.Delete(ctx, path("modeldeployments", name)); err != nil {
					return fmt.Errorf("removing %s: %w", name, err)
				}
				log.Printf("bridge: %s/%s isn't needed this session, removed", choice.Namespace, name)
				b.removed[name], b.applied[name], b.poked[name] = true, "", ""
			}
			continue
		}
		b.removed[name] = false
		if config != b.applied[name] {
			ready, err := b.replace(ctx, name, config)
			if err != nil {
				return err
			}
			if !ready {
				// The last session's is still going: report it as not applied yet.
				reports[name] = Report{Conditions: []string{"Replacing the last session's deployment"}}
				continue
			}
		}
		r, err := b.status(ctx, name)
		if err != nil {
			return err
		}
		reports[name] = r
	}
	return b.report(ctx, reports)
}

// replace makes a deployment hold config, and reports whether it does. A
// deployment holding another config, left from an earlier session, is deleted
// first rather than updated: Modelplane would roll its engines, and a new
// engine can't start while the old ones hold the nodes, since every engine
// takes a node to itself. Its status would also say ready, from the old ones.
// Deleting takes a few ticks; until the old one is gone, this reports false.
func (b *Bridge) replace(ctx context.Context, name, config string) (bool, error) {
	var existing struct {
		Metadata struct {
			Labels            map[string]string `json:"labels"`
			DeletionTimestamp *string           `json:"deletionTimestamp"`
		} `json:"metadata"`
	}
	err := b.Kube.Get(ctx, path("modeldeployments", name), &existing)
	switch {
	case kube.NotFound(err):
		c, err := choice.Parse(config)
		if err != nil {
			return false, err
		}
		if err := b.Kube.Apply(ctx, path("modeldeployments", name), choice.Deployment(name, c, b.Options)); err != nil {
			return false, fmt.Errorf("applying %s as %s: %w", config, name, err)
		}
		log.Printf("bridge: applied %s as %s/%s", config, choice.Namespace, name)
		b.applied[name], b.poked[name] = config, ""
		return true, nil
	case err != nil:
		return false, err
	case existing.Metadata.DeletionTimestamp != nil:
		return false, nil // still going
	case existing.Metadata.Labels["inference-vote/config"] == config:
		b.applied[name] = config // already there, from before a restart
		return true, nil
	}
	if err := b.Kube.Delete(ctx, path("modeldeployments", name)); err != nil {
		return false, fmt.Errorf("removing %s's last config: %w", name, err)
	}
	log.Printf("bridge: %s/%s held %s; removing it before applying %s", choice.Namespace, name, existing.Metadata.Labels["inference-vote/config"], config)
	return false, nil
}

func (b *Bridge) desired(ctx context.Context) (map[string]string, bool, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.VoteURL+"/api/bridge/desired", nil)
	req.Header.Set("Authorization", "Bearer "+b.Token)
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, false, nil
	case http.StatusOK:
		var body struct {
			Deployments map[string]string `json:"deployments"`
		}
		return body.Deployments, true, json.NewDecoder(resp.Body).Decode(&body)
	default:
		return nil, false, fmt.Errorf("voting app answered %d", resp.StatusCode)
	}
}

type deployment struct {
	Status struct {
		Replicas struct {
			Total int `json:"total"`
			Ready int `json:"ready"`
		} `json:"replicas"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// Report is what the bridge sends back for one deployment.
type Report struct {
	Applied       bool     `json:"applied"`
	ReplicasReady int      `json:"replicasReady"`
	ReplicasTotal int      `json:"replicasTotal"`
	Conditions    []string `json:"conditions"`
}

func (b *Bridge) status(ctx context.Context, name string) (Report, error) {
	var d deployment
	if err := b.Kube.Get(ctx, path("modeldeployments", name), &d); err != nil {
		return Report{}, err
	}
	if d.Status.Replicas.Total > 0 && d.Status.Replicas.Ready == d.Status.Replicas.Total && b.poked[name] != b.applied[name] {
		b.poke(ctx, name)
	}
	r := Report{Applied: true, ReplicasReady: d.Status.Replicas.Ready, ReplicasTotal: d.Status.Replicas.Total, Conditions: []string{}}
	for _, c := range d.Status.Conditions {
		r.Conditions = append(r.Conditions, fmt.Sprintf("%s=%s %s", c.Type, c.Status, c.Reason))
	}
	return r, nil
}

func (b *Bridge) report(ctx context.Context, reports map[string]any) error {
	body, _ := json.Marshal(map[string]any{"deployments": reports})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.VoteURL+"/api/bridge/report", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// poke touches a deployment's ModelService once its replica is ready. In the
// first rehearsals Modelplane routed the gateway to ready engines only about
// five minutes later, which looks like waiting for a periodic reconcile; a
// change to the ModelService makes it reconcile now. An annotation is the
// smallest change that does it, and it is ours alone, so it never fights
// Modelplane over a field.
func (b *Bridge) poke(ctx context.Context, name string) {
	obj := map[string]any{
		"apiVersion": "modelplane.ai/v1alpha1",
		"kind":       "ModelService",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   choice.Namespace,
			"annotations": map[string]string{"inference-vote/replica-ready": time.Now().UTC().Format(time.RFC3339)},
		},
	}
	if err := b.Kube.Apply(ctx, path("modelservices", name), obj); err != nil {
		log.Printf("bridge: nudging ModelService %s: %v", name, err)
		return
	}
	log.Printf("bridge: replica ready, nudged ModelService %s/%s to route it", choice.Namespace, name)
	b.poked[name] = b.applied[name]
}

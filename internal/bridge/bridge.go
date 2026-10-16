// Package bridge runs next to Modelplane on the control plane, which is a kind
// cluster on the presenter's laptop, so nothing on the internet can reach it.
// It reaches out instead: it polls the voting app for the winning config,
// applies the ModelDeployment locally, and sends back what the control plane
// reports. Only outbound HTTPS, so it works through any venue network or a
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
	Name     string // the ModelDeployment to own, "audience"
	Options  choice.Options
	Kube     *kube.Client
	Every    time.Duration

	applied string // config last applied, so an unchanged one is not re-sent
	http    *http.Client
}

func (b *Bridge) path() string {
	return fmt.Sprintf("/apis/modelplane.ai/v1alpha1/namespaces/%s/modeldeployments/%s", choice.Namespace, b.Name)
}

func (b *Bridge) Run(ctx context.Context) {
	b.http = &http.Client{Timeout: 10 * time.Second}
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

func (b *Bridge) tick(ctx context.Context) error {
	desired, err := b.desired(ctx)
	if err != nil || desired == "" {
		return err
	}
	if desired != b.applied {
		c, err := choice.Parse(desired)
		if err != nil {
			return err
		}
		if err := b.Kube.Apply(ctx, b.path(), choice.Deployment(b.Name, c, b.Options)); err != nil {
			return fmt.Errorf("applying %s: %w", desired, err)
		}
		log.Printf("bridge: applied %s as %s/%s", desired, choice.Namespace, b.Name)
		b.applied = desired
	}
	return b.report(ctx)
}

func (b *Bridge) desired(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.VoteURL+"/api/bridge/desired", nil)
	req.Header.Set("Authorization", "Bearer "+b.Token)
	resp, err := b.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return "", nil
	case http.StatusOK:
		var body struct {
			Config string `json:"config"`
		}
		return body.Config, json.NewDecoder(resp.Body).Decode(&body)
	default:
		return "", fmt.Errorf("voting app answered %d", resp.StatusCode)
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

func (b *Bridge) report(ctx context.Context) error {
	var d deployment
	if err := b.Kube.Get(ctx, b.path(), &d); err != nil {
		return err
	}
	conditions := make([]string, 0, len(d.Status.Conditions))
	for _, c := range d.Status.Conditions {
		conditions = append(conditions, fmt.Sprintf("%s=%s %s", c.Type, c.Status, c.Reason))
	}
	body, _ := json.Marshal(map[string]any{
		"applied":       true,
		"replicasReady": d.Status.Replicas.Ready,
		"replicasTotal": d.Status.Replicas.Total,
		"conditions":    conditions,
	})
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

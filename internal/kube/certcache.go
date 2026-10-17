package kube

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"

	"golang.org/x/crypto/acme/autocert"
)

// SecretCache keeps Let's Encrypt certificates in a Secret, so a restarted or
// rescheduled voting app reuses its certificate instead of requesting a new
// one. Let's Encrypt issues at most five identical certificates a week, and a
// restart right before the talk must not be what uses up the last one.
type SecretCache struct {
	Client    *Client
	Namespace string
	Name      string
}

var _ autocert.Cache = SecretCache{}

func (c SecretCache) path() string {
	return "/api/v1/namespaces/" + c.Namespace + "/secrets/" + c.Name
}

// Secret data keys allow only [-._a-zA-Z0-9]; autocert's keys contain "+".
func secretKey(key string) string { return "k-" + hex.EncodeToString([]byte(key)) }

func (c SecretCache) Get(ctx context.Context, key string) ([]byte, error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := c.Client.Get(ctx, c.path(), &secret); err != nil {
		if NotFound(err) {
			return nil, autocert.ErrCacheMiss
		}
		return nil, err
	}
	v, ok := secret.Data[secretKey(key)]
	if !ok {
		return nil, autocert.ErrCacheMiss
	}
	return base64.StdEncoding.DecodeString(v)
}

// Put merges in only its own key, so the keys already in the Secret stay. A
// server-side apply would not do: it drops whatever the same field manager set
// before and left out this time.
func (c SecretCache) Put(ctx context.Context, key string, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	if err := c.patch(ctx, map[string]any{secretKey(key): encoded}); !NotFound(err) {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": c.Name, "namespace": c.Namespace},
		"data":       map[string]string{secretKey(key): encoded},
	})
	q := url.Values{"fieldManager": {FieldManager}}
	_, err := c.Client.do(ctx, http.MethodPost, "/api/v1/namespaces/"+c.Namespace+"/secrets?"+q.Encode(), "application/json", body)
	return err
}

func (c SecretCache) patch(ctx context.Context, data map[string]any) error {
	body, _ := json.Marshal(map[string]any{"data": data})
	q := url.Values{"fieldManager": {FieldManager}}
	_, err := c.Client.do(ctx, http.MethodPatch, c.path()+"?"+q.Encode(), "application/merge-patch+json", body)
	return err
}

func (c SecretCache) Delete(ctx context.Context, key string) error {
	err := c.patch(ctx, map[string]any{secretKey(key): nil})
	if NotFound(err) {
		return nil
	}
	return err
}

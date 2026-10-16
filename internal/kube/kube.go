// Package kube is the little of the Kubernetes API this app needs: server-side
// apply, get and list, with the pod's own service account. It talks REST
// directly rather than pulling in client-go, because three calls do not justify
// the dependency tree.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// FieldManager is who server-side apply records as owning the fields we set.
const FieldManager = "inference-vote"

type Client struct {
	base      string
	http      *http.Client
	tokenFile string
}

// InCluster builds a client from the pod's service account.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster: KUBERNETES_SERVICE_HOST is not set")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &Client{
		base:      "https://" + host + ":" + port,
		http:      &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
		tokenFile: saDir + "/token",
	}, nil
}

// do reads the token on every call: projected service account tokens rotate.
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return out, &StatusError{Code: resp.StatusCode, Body: string(out)}
	}
	return out, nil
}

type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("kubernetes API: %d: %s", e.Code, e.Body) }

// NotFound reports whether err is a 404 from the API.
func NotFound(err error) bool {
	se, ok := err.(*StatusError)
	return ok && se.Code == http.StatusNotFound
}

// Apply server-side applies obj at path, the object's own URL. JSON is valid
// YAML, so the apply patch content type accepts it as is.
func (c *Client) Apply(ctx context.Context, path string, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	q := url.Values{"fieldManager": {FieldManager}, "force": {"true"}}
	_, err = c.do(ctx, http.MethodPatch, path+"?"+q.Encode(), "application/apply-patch+yaml", body)
	return err
}

// Get decodes the object at path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	b, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// List decodes a collection, filtered by a label selector, into out.
func (c *Client) List(ctx context.Context, path, labelSelector string, out any) error {
	if labelSelector != "" {
		path += "?" + url.Values{"labelSelector": {labelSelector}}.Encode()
	}
	return c.Get(ctx, path, out)
}

package kube

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Type is a kind of namespaced resource the API serves, at its preferred
// version.
type Type struct {
	Group    string // empty for the core group
	Version  string
	Resource string // the plural in URLs, such as deployments
	Kind     string
}

func (t Type) APIVersion() string {
	if t.Group == "" {
		return t.Version
	}
	return t.Group + "/" + t.Version
}

// Types that are never worth showing: Secrets hold credentials, Events are
// history rather than resources, v1 Endpoints duplicate EndpointSlices, and
// metrics.k8s.io mirrors pods.
func skipped(t Type) bool {
	switch {
	case t.Group == "" && slices.Contains([]string{"secrets", "events", "endpoints"}, t.Resource):
		return true
	case t.Group == "events.k8s.io", t.Group == "metrics.k8s.io":
		return true
	}
	return false
}

type resourceList struct {
	GroupVersion string `json:"groupVersion"`
	Resources    []struct {
		Name       string   `json:"name"`
		Kind       string   `json:"kind"`
		Namespaced bool     `json:"namespaced"`
		Verbs      []string `json:"verbs"`
	} `json:"resources"`
}

// NamespacedTypes lists every namespaced resource type the API can list, from
// API discovery, so resources that operators and controllers add are found
// without naming them here.
func (c *Client) NamespacedTypes(ctx context.Context) ([]Type, error) {
	paths := []string{"/api/v1"}
	var groups struct {
		Groups []struct {
			Name             string `json:"name"`
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
			} `json:"preferredVersion"`
		} `json:"groups"`
	}
	if err := c.Get(ctx, "/apis", &groups); err != nil {
		return nil, err
	}
	for _, g := range groups.Groups {
		paths = append(paths, "/apis/"+g.PreferredVersion.GroupVersion)
	}
	var types []Type
	for _, p := range paths {
		var rl resourceList
		if err := c.Get(ctx, p, &rl); err != nil {
			continue // an aggregated API that is down shouldn't hide the rest
		}
		group, version, ok := strings.Cut(rl.GroupVersion, "/")
		if !ok {
			group, version = "", rl.GroupVersion
		}
		for _, r := range rl.Resources {
			t := Type{Group: group, Version: version, Resource: r.Name, Kind: r.Kind}
			if !r.Namespaced || strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") || skipped(t) {
				continue
			}
			types = append(types, t)
		}
	}
	return types, nil
}

// ListType lists one type's resources in a namespace. A collection's items
// don't repeat their apiVersion and kind, so they are filled in.
func (c *Client) ListType(ctx context.Context, namespace string, t Type) ([]map[string]any, error) {
	path := fmt.Sprintf("/apis/%s/namespaces/%s/%s", t.APIVersion(), namespace, t.Resource)
	if t.Group == "" {
		path = fmt.Sprintf("/api/%s/namespaces/%s/%s", t.Version, namespace, t.Resource)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := c.Get(ctx, path, &list); err != nil {
		return nil, err
	}
	for _, it := range list.Items {
		it["apiVersion"], it["kind"] = t.APIVersion(), t.Kind
	}
	return list.Items, nil
}

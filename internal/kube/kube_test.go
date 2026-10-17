package kube

import (
	"encoding/json"
	"strings"
	"testing"
)

var inv = Inventory{Version: "v1.31.4", Nodes: 1,
	StorageClasses: []Class{{Name: "local-path", Default: true}, {Name: "fast"}},
	IngressClasses: []Class{{Name: "traefik", Default: true}}, TLSIssuers: []string{"letsencrypt"}}

func TestValidateDefaultsAndRejects(t *testing.T) {
	c := Config{Namespace: "shop"}
	if err := Validate(&c, inv, 2, 20); err != nil {
		t.Fatal(err)
	}
	if c.StorageClass != "local-path" || c.StorageGB != 20 || c.NetworkPolicy != "isolated" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	bad := []Config{
		{Namespace: "kube-system"}, {Namespace: "default"}, {Namespace: "Bad_NS"},
		{Namespace: "a", StorageClass: "missing"},
		{Namespace: "a", Host: "x.example.ng", IngressClass: "nginx"},
		{Namespace: "a", Host: "x.example.ng", TLSIssuer: "nope"},
		{Namespace: "a", TLSIssuer: "letsencrypt"},
		{Namespace: "a", Host: "not a host"},
		{Namespace: "a", NetworkPolicy: "allow-all"},
	}
	for _, b := range bad {
		if err := Validate(&b, inv, 1, 10); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
	if err := Validate(&Config{Namespace: "a"}, inv, 21, 10); err == nil {
		t.Error("accepted 21 replicas")
	}
}

const kuma = `services:
  app:
    image: louislam/uptime-kuma:1.23.16
    restart: unless-stopped
    ports: ["127.0.0.1:3001:3001"]
    volumes: ["data:/app/data"]
    environment:
      TZ: "${TZ:-Africa/Lagos}"
volumes:
  data: {}
`

func kinds(t *testing.T, b *Bundle) map[string]map[string]any {
	var l struct{ Items []map[string]any }
	if err := json.Unmarshal([]byte(b.Manifests), &l); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, it := range l.Items {
		out[it["kind"].(string)] = it
		if it["kind"] != "Namespace" && it["metadata"].(map[string]any)["namespace"] != "shop" {
			t.Fatalf("object outside namespace: %v", it)
		}
	}
	return out
}

func TestRenderStatefulTemplate(t *testing.T) {
	c := Config{Namespace: "shop", Host: "status.example.ng", TLSIssuer: "letsencrypt"}
	if err := Validate(&c, inv, 1, 10); err != nil {
		t.Fatal(err)
	}
	b, err := Render(kuma, Health{Port: 3001, Path: "/"}, c, map[string]string{"SECRET": "x"}, 1, 2, 4096, "svc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.HealthURL != "http://app.shop.svc.cluster.local:3001/" {
		t.Fatalf("health url %s", b.HealthURL)
	}
	k := kinds(t, b)
	for _, want := range []string{"Namespace", "Secret", "PersistentVolumeClaim", "Deployment", "Service", "Ingress", "NetworkPolicy"} {
		if k[want] == nil {
			t.Fatalf("missing %s in %s", want, b.Manifests)
		}
	}
	if !strings.Contains(b.Manifests, `"value":"Africa/Lagos"`) || !strings.Contains(b.Manifests, `"storageClassName":"local-path"`) ||
		!strings.Contains(b.Manifests, `"cert-manager.io/cluster-issuer":"letsencrypt"`) || !strings.Contains(b.Manifests, `"type":"Recreate"`) {
		t.Fatalf("unexpected manifests: %s", b.Manifests)
	}
	if _, err := Render(kuma, Health{Port: 3001}, c, nil, 2, 2, 4096, "svc-1"); err == nil {
		t.Fatal("RWO volume template accepted 2 replicas")
	}
}

func TestRenderRefusesUnsafeCompose(t *testing.T) {
	c := Config{Namespace: "shop", NetworkPolicy: "open"}
	for _, compose := range []string{
		"services:\n  app:\n    image: x\n    privileged: true\n",
		"services:\n  app:\n    image: x\n    network_mode: host\n",
		"services:\n  app:\n    image: x\n    volumes: [\"/etc:/host-etc\"]\n",
		"services:\n  app:\n    build: .\n",
	} {
		if _, err := Render(compose, Health{}, c, nil, 1, 1, 1024, "s"); err == nil {
			t.Errorf("accepted %q", compose)
		}
	}
	b, err := Render("services:\n  app:\n    image: traefik/whoami:v1.10.3\n    ports: [\"127.0.0.1:18080:80\"]\n", Health{Port: 18080}, c, nil, 3, 1, 1024, "s")
	if err != nil {
		t.Fatal(err)
	}
	if b.HealthURL != "http://app.shop.svc.cluster.local:80/" || strings.Contains(b.Manifests, "NetworkPolicy") || !strings.Contains(b.Manifests, `"replicas":3`) {
		t.Fatalf("whoami render: %s %s", b.HealthURL, b.Manifests)
	}
}

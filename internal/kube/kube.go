// Package kube renders a template's Docker Compose stack into Kubernetes objects for one service and
// validates the customer's configuration against what their cluster reported. Only a safe subset of
// Compose is accepted (images, commands, env, ports, named volumes); anything else is refused rather
// than approximated.
package kube

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Inventory is what the in-cluster agent reports (k8s_inventory) and what the configuration menu offers.
type Inventory struct {
	Version        string   `json:"version"`
	Nodes          int      `json:"nodes"`
	CPUMilli       int64    `json:"allocatable_cpu_milli"`
	MemoryMB       int64    `json:"allocatable_memory_mb"`
	StorageClasses []Class  `json:"storage_classes"`
	IngressClasses []Class  `json:"ingress_classes"`
	TLSIssuers     []string `json:"tls_issuers"`
}

type Class struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// AgentNamespace is where the Helm chart installs the agent; isolated namespaces admit its probes.
const AgentNamespace = "spade-system"

// Config is the per-service Kubernetes configuration chosen from the menu.
type Config struct {
	Namespace        string `json:"namespace"`
	StorageClass     string `json:"storage_class,omitempty"`
	StorageGB        int    `json:"storage_gb,omitempty"`
	IngressClass     string `json:"ingress_class,omitempty"`
	Host             string `json:"host,omitempty"`
	TLSIssuer        string `json:"tls_issuer,omitempty"`
	NetworkPolicy    string `json:"network_policy,omitempty"`
	IngressNamespace string `json:"ingress_namespace,omitempty"`
}

const MaxReplicas = 20

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	interp   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)
)

func reservedNamespace(n string) bool {
	switch n {
	case "default", "spade-system":
		return true
	}
	return strings.HasPrefix(n, "kube-") || strings.HasPrefix(n, "openshift")
}

func pick(cs []Class, want string) (string, bool) {
	for _, c := range cs {
		if (want == "" && c.Default) || (want != "" && c.Name == want) {
			return c.Name, true
		}
	}
	return "", false
}

// Validate fills defaults from the inventory and rejects anything the cluster cannot honour.
func Validate(c *Config, inv Inventory, replicas, sizeDiskGB int) error {
	if !dnsLabel.MatchString(c.Namespace) || reservedNamespace(c.Namespace) {
		return fmt.Errorf("namespace %q must be a DNS label and not default, spade-system, kube-* or openshift*", c.Namespace)
	}
	if replicas < 1 || replicas > MaxReplicas {
		return fmt.Errorf("replicas must be between 1 and %d", MaxReplicas)
	}
	if c.StorageGB == 0 {
		c.StorageGB = sizeDiskGB
	}
	if c.StorageGB < 1 || c.StorageGB > 1000 {
		return fmt.Errorf("storage_gb must be between 1 and 1000")
	}
	sc, ok := pick(inv.StorageClasses, c.StorageClass)
	if !ok && c.StorageClass != "" {
		return fmt.Errorf("storage class %q does not exist on the cluster", c.StorageClass)
	}
	c.StorageClass = sc
	switch c.NetworkPolicy {
	case "":
		c.NetworkPolicy = "isolated"
	case "isolated", "open":
	default:
		return fmt.Errorf("network_policy must be isolated or open")
	}
	if c.Host == "" {
		if c.TLSIssuer != "" || c.IngressClass != "" {
			return fmt.Errorf("ingress_class and tls_issuer need a host")
		}
		return nil
	}
	if !hostname.MatchString(c.Host) {
		return fmt.Errorf("host %q is not a valid DNS name", c.Host)
	}
	ic, ok := pick(inv.IngressClasses, c.IngressClass)
	if !ok {
		return fmt.Errorf("ingress class %q is not available on the cluster", c.IngressClass)
	}
	c.IngressClass = ic
	if c.TLSIssuer != "" && !contains(inv.TLSIssuers, c.TLSIssuer) {
		return fmt.Errorf("TLS issuer %q is not a cert-manager ClusterIssuer on the cluster", c.TLSIssuer)
	}
	if c.IngressNamespace == "" {
		c.IngressNamespace = "kube-system"
	}
	if !dnsLabel.MatchString(c.IngressNamespace) {
		return fmt.Errorf("ingress_namespace must be a DNS label")
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

type composeFile struct {
	Services map[string]map[string]any `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
}

var allowedKeys = map[string]bool{"image": true, "command": true, "entrypoint": true, "environment": true, "ports": true,
	"volumes": true, "restart": true, "depends_on": true, "labels": true, "healthcheck": true}

// Health is the template's health block (spade.yml).
type Health struct {
	Port   int    `json:"port"`
	Path   string `json:"path"`
	Expect []int  `json:"expect_status"`
}

// Bundle is what apply_bundle carries to the cluster agent.
type Bundle struct {
	Manifests string // JSON v1 List
	HealthURL string
}

type obj = map[string]any

// Render turns the Compose stack into namespaced objects for service svcID. vcpu/ramMB is the per-replica size.
func Render(compose string, h Health, c Config, env map[string]string, replicas, vcpu, ramMB int, svcID string) (*Bundle, error) {
	var cf composeFile
	if err := yaml.Unmarshal([]byte(compose), &cf); err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	if len(cf.Services) == 0 {
		return nil, fmt.Errorf("compose has no services")
	}
	names := make([]string, 0, len(cf.Services))
	for n := range cf.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	ns := c.Namespace
	labels := func(app string) obj {
		l := obj{"app.kubernetes.io/managed-by": "spadeai", "spade.ai/service": svcID}
		if app != "" {
			l["app.kubernetes.io/name"] = app
		}
		return l
	}
	meta := func(name, app string) obj { return obj{"name": name, "namespace": ns, "labels": labels(app)} }
	items := []any{obj{"apiVersion": "v1", "kind": "Namespace", "metadata": obj{"name": ns, "labels": obj{
		"app.kubernetes.io/managed-by": "spadeai", "spade.ai/service": svcID, "spade.ai/managed": "true",
		"pod-security.kubernetes.io/enforce": "baseline"}}}}
	items = append(items, obj{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta("spade-env", ""), "stringData": stringMap(env)})
	cpuMilli := int64(vcpu) * 1000 / int64(len(names))
	memMB := int64(ramMB) / int64(len(names))
	var vols []string
	for v := range cf.Volumes {
		if !dnsLabel.MatchString(v) {
			return nil, fmt.Errorf("volume name %q is not a DNS label", v)
		}
		vols = append(vols, v)
	}
	sort.Strings(vols)
	if len(vols) > 0 && replicas > 1 {
		return nil, fmt.Errorf("this template keeps data on persistent volumes, which mount on one node (ReadWriteOnce): use replicas=1")
	}
	for _, v := range vols {
		gb := c.StorageGB / len(vols)
		if gb < 1 {
			gb = 1
		}
		spec := obj{"accessModes": []any{"ReadWriteOnce"}, "resources": obj{"requests": obj{"storage": fmt.Sprintf("%dGi", gb)}}}
		if c.StorageClass != "" {
			spec["storageClassName"] = c.StorageClass
		}
		items = append(items, obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta("data-"+v, ""), "spec": spec})
	}
	healthSvc, healthPort := "", 0
	for _, n := range names {
		s := cf.Services[n]
		if !dnsLabel.MatchString(n) {
			return nil, fmt.Errorf("compose service name %q is not a DNS label", n)
		}
		for k := range s {
			if !allowedKeys[k] {
				return nil, fmt.Errorf("compose key %s.%s is not supported on Kubernetes", n, k)
			}
		}
		image, _ := s["image"].(string)
		if image == "" {
			return nil, fmt.Errorf("compose service %s has no image", n)
		}
		ctr := obj{"name": n, "image": image, "envFrom": []any{obj{"secretRef": obj{"name": "spade-env"}}},
			"securityContext": obj{"allowPrivilegeEscalation": false},
			"resources": obj{
				"requests": obj{"cpu": fmt.Sprintf("%dm", cpuMilli/2), "memory": fmt.Sprintf("%dMi", memMB/2)},
				"limits":   obj{"cpu": fmt.Sprintf("%dm", cpuMilli), "memory": fmt.Sprintf("%dMi", memMB)}}}
		if v, ok := s["command"]; ok {
			a, err := argv(v)
			if err != nil {
				return nil, fmt.Errorf("%s.command: %w", n, err)
			}
			ctr["args"] = a
		}
		if v, ok := s["entrypoint"]; ok {
			a, err := argv(v)
			if err != nil {
				return nil, fmt.Errorf("%s.entrypoint: %w", n, err)
			}
			ctr["command"] = a
		}
		envs, err := composeEnv(s["environment"], env)
		if err != nil {
			return nil, fmt.Errorf("%s.environment: %w", n, err)
		}
		if len(envs) > 0 {
			ctr["env"] = envs
		}
		var ports []any
		for _, p := range list(s["ports"]) {
			hostPort, ctrPort, err := parsePort(p)
			if err != nil {
				return nil, fmt.Errorf("%s.ports: %w", n, err)
			}
			ports = append(ports, obj{"name": "p" + strconv.Itoa(ctrPort), "port": ctrPort, "targetPort": ctrPort})
			if h.Port != 0 && (hostPort == h.Port || (hostPort == 0 && ctrPort == h.Port)) {
				healthSvc, healthPort = n, ctrPort
			}
		}
		var mounts, podVols []any
		for _, v := range list(s["volumes"]) {
			str, _ := v.(string)
			parts := strings.Split(str, ":")
			if len(parts) < 2 || len(parts) > 3 || cf.Volumes == nil || !hasKey(cf.Volumes, parts[0]) || !strings.HasPrefix(parts[1], "/") {
				return nil, fmt.Errorf("%s.volumes: only named volumes (name:/path) are supported, got %q", n, str)
			}
			m := obj{"name": parts[0], "mountPath": parts[1]}
			if len(parts) == 3 && parts[2] == "ro" {
				m["readOnly"] = true
			}
			mounts = append(mounts, m)
			podVols = append(podVols, obj{"name": parts[0], "persistentVolumeClaim": obj{"claimName": "data-" + parts[0]}})
		}
		if len(mounts) > 0 {
			ctr["volumeMounts"] = mounts
		}
		pod := obj{"containers": []any{ctr}, "automountServiceAccountToken": false, "enableServiceLinks": false}
		if len(podVols) > 0 {
			pod["volumes"] = podVols
		}
		dep := obj{"replicas": replicas, "selector": obj{"matchLabels": obj{"app.kubernetes.io/name": n}},
			"template": obj{"metadata": obj{"labels": labels(n)}, "spec": pod}}
		if len(podVols) > 0 {
			dep["strategy"] = obj{"type": "Recreate"}
		}
		items = append(items, obj{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(n, n), "spec": dep})
		if len(ports) > 0 {
			items = append(items, obj{"apiVersion": "v1", "kind": "Service", "metadata": meta(n, n),
				"spec": obj{"selector": obj{"app.kubernetes.io/name": n}, "ports": ports}})
		}
	}
	if h.Port != 0 && healthSvc == "" {
		return nil, fmt.Errorf("no compose service publishes the health port %d", h.Port)
	}
	if c.Host != "" {
		if healthSvc == "" {
			return nil, fmt.Errorf("a host needs a template with a health port to route to")
		}
		ing := obj{"ingressClassName": c.IngressClass, "rules": []any{obj{"host": c.Host, "http": obj{"paths": []any{obj{
			"path": "/", "pathType": "Prefix", "backend": obj{"service": obj{"name": healthSvc, "port": obj{"number": healthPort}}}}}}}}}
		m := meta("web", "")
		if c.TLSIssuer != "" {
			ing["tls"] = []any{obj{"hosts": []any{c.Host}, "secretName": "web-tls"}}
			m["annotations"] = obj{"cert-manager.io/cluster-issuer": c.TLSIssuer}
		}
		items = append(items, obj{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": m, "spec": ing})
	}
	if c.NetworkPolicy == "isolated" {
		from := []any{obj{"podSelector": obj{}},
			// the SpadeAi agent runs the health probe from its own namespace
			obj{"namespaceSelector": obj{"matchLabels": obj{"kubernetes.io/metadata.name": AgentNamespace}},
				"podSelector": obj{"matchLabels": obj{"app.kubernetes.io/name": "spade-agent"}}}}
		if c.Host != "" {
			from = append(from, obj{"namespaceSelector": obj{"matchLabels": obj{"kubernetes.io/metadata.name": c.IngressNamespace}}})
		}
		items = append(items, obj{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta("isolate", ""),
			"spec": obj{"podSelector": obj{}, "policyTypes": []any{"Ingress"}, "ingress": []any{obj{"from": from}}}})
	}
	b, err := json.Marshal(obj{"apiVersion": "v1", "kind": "List", "items": items})
	if err != nil {
		return nil, err
	}
	out := &Bundle{Manifests: string(b)}
	if healthSvc != "" {
		path := h.Path
		if path == "" {
			path = "/"
		}
		out.HealthURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s", healthSvc, ns, healthPort, path)
	}
	return out, nil
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func stringMap(m map[string]string) obj {
	o := obj{}
	for k, v := range m {
		o[k] = v
	}
	return o
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func argv(v any) ([]any, error) {
	switch t := v.(type) {
	case string:
		out := []any{}
		for _, f := range strings.Fields(t) {
			out = append(out, f)
		}
		return out, nil
	case []any:
		for _, x := range t {
			if _, ok := x.(string); !ok {
				return nil, fmt.Errorf("arguments must be strings")
			}
		}
		return t, nil
	}
	return nil, fmt.Errorf("must be a string or list")
}

// parsePort accepts "80", "8080:80", "127.0.0.1:8080:80" (optionally /tcp) and returns host and container ports.
func parsePort(v any) (int, int, error) {
	s := fmt.Sprint(v)
	s = strings.TrimSuffix(s, "/tcp")
	parts := strings.Split(s, ":")
	ctr, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || ctr < 1 || ctr > 65535 {
		return 0, 0, fmt.Errorf("unsupported port %q", s)
	}
	host := 0
	if len(parts) >= 2 {
		if host, err = strconv.Atoi(parts[len(parts)-2]); err != nil {
			return 0, 0, fmt.Errorf("unsupported port %q", s)
		}
	}
	return host, ctr, nil
}

// composeEnv resolves ${NAME} and ${NAME:-default} from the service env, as Compose would.
func composeEnv(v any, svcEnv map[string]string) ([]any, error) {
	kv := map[string]string{}
	switch t := v.(type) {
	case nil:
	case map[string]any:
		for k, x := range t {
			if x == nil {
				x = ""
			}
			kv[k] = fmt.Sprint(x)
		}
	case []any:
		for _, x := range t {
			k, val, _ := strings.Cut(fmt.Sprint(x), "=")
			kv[k] = val
		}
	default:
		return nil, fmt.Errorf("must be a map or list")
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []any
	for _, k := range keys {
		val := interp.ReplaceAllStringFunc(kv[k], func(m string) string {
			g := interp.FindStringSubmatch(m)
			if x, ok := svcEnv[g[1]]; ok {
				return x
			}
			return g[3]
		})
		if strings.Contains(val, "${") {
			return nil, fmt.Errorf("%s uses unsupported interpolation", k)
		}
		out = append(out, obj{"name": k, "value": val})
	}
	return out, nil
}

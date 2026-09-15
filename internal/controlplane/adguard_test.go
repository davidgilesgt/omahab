package controlplane

import (
	"strings"
	"testing"
)

// Fresh seeds carry Quad9 upstream, DHCP off, LAN binds and web bind.
func TestRenderAdGuardFresh(t *testing.T) {
	doc, fresh, err := renderAdGuardDoc(nil, true, []string{"192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("want fresh=true for missing file")
	}
	if doc["schema_version"] != 34 || doc["bind_host"] != "127.0.0.1" || doc["bind_port"] != adguardWebPort {
		t.Fatalf("managed web fields = %v", doc)
	}
	dns, _ := doc["dns"].(map[string]any)
	if dns == nil {
		t.Fatal("fresh dns section missing")
	}
	binds, _ := dns["bind_hosts"].([]string)
	if len(binds) != 1 || binds[0] != "192.168.1.10" {
		t.Fatalf("bind_hosts = %v, want explicit LAN (never 0.0.0.0)", dns["bind_hosts"])
	}
	if dns["port"] != 53 {
		t.Fatalf("dns port = %v", dns["port"])
	}
	up, _ := dns["upstream_dns"].([]string)
	if len(up) != 1 || !strings.Contains(up[0], "quad9") {
		t.Fatalf("upstream = %v", dns["upstream_dns"])
	}
	dhcp, _ := doc["dhcp"].(map[string]any)
	if dhcp["enabled"] != false {
		t.Fatalf("dhcp = %v", dhcp)
	}
}

// Refresh preserves user filtering/upstream and only manages binds/security.
func TestRenderAdGuardRefreshPreservesUser(t *testing.T) {
	raw := []byte(`schema_version: 30
bind_host: 0.0.0.0
bind_port: 3000
dns:
  bind_hosts: ["10.0.0.5"]
  port: 5353
  upstream_dns: ["https://example.com/dns-query"]
  filtering_enabled: true
dhcp:
  enabled: true
filters:
- {name: mine}
users:
- {name: admin, password: $2a$10$kept}
`)
	doc, fresh, err := renderAdGuardDoc(raw, false, []string{"192.168.1.10", "192.168.1.11"})
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("want fresh=false for existing file")
	}
	dns, _ := doc["dns"].(map[string]any)
	if dns["port"] != 53 {
		t.Fatalf("managed port = %v", dns["port"])
	}
	binds := toStrings(dns["bind_hosts"])
	if len(binds) != 2 {
		t.Fatalf("bind_hosts = %v, want refreshed LAN set", dns["bind_hosts"])
	}
	up := toStrings(dns["upstream_dns"])
	if len(up) != 1 || up[0] != "https://example.com/dns-query" {
		t.Fatalf("user upstream must be preserved, got %v", up)
	}
	dhcp, _ := doc["dhcp"].(map[string]any)
	if dhcp["enabled"] != false {
		t.Fatalf("managed dhcp = %v", dhcp)
	}
	users, _ := doc["users"].([]any)
	if len(users) != 1 {
		t.Fatalf("existing admin users must be preserved: %v", doc["users"])
	}
	if doc["schema_version"] != 34 {
		t.Fatalf("schema = %v", doc["schema_version"])
	}
	if _, ok := doc["filters"]; !ok {
		t.Fatal("user filters must survive refresh")
	}
}

// No LAN address: loopback-only DNS, never 0.0.0.0.
func TestRenderAdGuardNoLAN(t *testing.T) {
	doc, fresh, err := renderAdGuardDoc(nil, true, nil)
	if err != nil || !fresh {
		t.Fatalf("fresh=%v err=%v", fresh, err)
	}
	dns, _ := doc["dns"].(map[string]any)
	binds, _ := dns["bind_hosts"].([]string)
	if len(binds) != 1 || binds[0] != "127.0.0.1" {
		t.Fatalf("bind_hosts = %v, want loopback-only (never 0.0.0.0)", dns["bind_hosts"])
	}
}

func TestManagedBindsNeverWildcard(t *testing.T) {
	for _, b := range managedBinds(nil) {
		if b == "0.0.0.0" || b == "::" {
			t.Fatalf("wildcard bind %q", b)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		for _, e := range t {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
	}
	return out
}

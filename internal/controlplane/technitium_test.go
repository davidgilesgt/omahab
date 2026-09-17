package controlplane

import (
	"strings"
	"testing"
)

func TestTechnitiumDNSEndpointsLoopbackWhenNoLAN(t *testing.T) {
	got := technitiumDNSEndpoints(nil)
	if len(got) != 1 || got[0] != "127.0.0.1:53" {
		t.Fatalf("endpoints = %v, want [127.0.0.1:53]", got)
	}
}

func TestTechnitiumDNSEndpointsLANPlusLoopback(t *testing.T) {
	got := technitiumDNSEndpoints([]string{"192.168.8.106", "192.168.8.106"})
	want := []string{"127.0.0.1:53", "192.168.8.106:53"}
	if len(got) != len(want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoints = %v, want %v", got, want)
		}
	}
}

func TestTechnitiumUpstreamIsQuad9DoH(t *testing.T) {
	if technitiumUpstream != "https://dns.quad9.net/dns-query" {
		t.Fatalf("upstream = %q", technitiumUpstream)
	}
	if technitiumWebPort != 5380 {
		t.Fatalf("web port = %d, want 5380", technitiumWebPort)
	}
}

func TestTechnitiumBlocklistsDefault(t *testing.T) {
	lists := technitiumBlocklists()
	if len(lists) == 0 {
		t.Fatal("blocklists empty")
	}
	joined := strings.Join(lists, ",")
	if !strings.Contains(joined, "StevenBlack") || !strings.Contains(joined, "hagezi") {
		t.Fatalf("blocklists = %v, want StevenBlack + HaGeZi Pro hosts", lists)
	}
}

func TestTechnitiumSSOParamsOpenSignupWithRoleSync(t *testing.T) {
	p := technitiumSSOParams("omahab.com", "cid", "csecret")
	if p.Get("ssoAuthority") != "https://id.omahab.com" {
		t.Fatalf("authority = %q", p.Get("ssoAuthority"))
	}
	if p.Get("ssoAllowSignup") != "true" {
		t.Fatalf("ssoAllowSignup = %q, want true", p.Get("ssoAllowSignup"))
	}
	if p.Get("ssoAllowSignupOnlyForMappedUsers") != "false" {
		t.Fatalf("ssoAllowSignupOnlyForMappedUsers = %q, want false (no mapped-only dead-end)", p.Get("ssoAllowSignupOnlyForMappedUsers"))
	}
	got := p.Get("ssoGroupMap")
	if !strings.Contains(got, "admins:Administrators") || !strings.Contains(got, "members:DNS Administrators") {
		t.Fatalf("ssoGroupMap = %q, want admins+members role sync", got)
	}
	scopes := strings.Split(p.Get("ssoScopes"), "|")
	for _, want := range []string{"openid", "profile", "email", "groups"} {
		found := false
		for _, s := range scopes {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ssoScopes = %q, missing %q", p.Get("ssoScopes"), want)
		}
	}
}

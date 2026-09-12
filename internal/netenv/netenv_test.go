package netenv

import (
	"context"
	"net"
	"testing"
	"time"
)

func ipnet(s string) *net.IPNet {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}
func TestPickLANIPv4(t *testing.T) {
	up := net.FlagUp
	upLoop := net.FlagUp | net.FlagLoopback
	cases := []struct {
		name   string
		ifaces []IfaceAddrs
		want   string
	}{
		{
			name: "private picked",
			ifaces: []IfaceAddrs{
				{Name: "eth0", Flags: up, Addrs: []net.Addr{ipnet("192.168.1.12/24")}},
			},
			want: "192.168.1.12",
		},
		{
			name: "down skipped",
			ifaces: []IfaceAddrs{
				{Name: "eth0", Flags: 0, Addrs: []net.Addr{ipnet("192.168.1.12/24")}},
			},
			want: "",
		},
		{
			name: "loopback skipped",
			ifaces: []IfaceAddrs{
				{Name: "lo", Flags: upLoop, Addrs: []net.Addr{ipnet("127.0.0.1/8")}},
			},
			want: "",
		},
		{
			name: "docker skipped",
			ifaces: []IfaceAddrs{
				{Name: "docker0", Flags: up, Addrs: []net.Addr{ipnet("172.17.0.1/16")}},
			},
			want: "",
		},
		{
			name: "tailscale skipped",
			ifaces: []IfaceAddrs{
				{Name: "tailscale0", Flags: up, Addrs: []net.Addr{ipnet("100.64.0.5/10")}},
			},
			want: "",
		},
		{
			name: "veth and br skipped",
			ifaces: []IfaceAddrs{
				{Name: "vethabc", Flags: up, Addrs: []net.Addr{ipnet("10.0.0.2/24")}},
				{Name: "br-def", Flags: up, Addrs: []net.Addr{ipnet("10.0.1.2/24")}},
			},
			want: "",
		},
		{
			name: "public skipped private kept",
			ifaces: []IfaceAddrs{
				{Name: "eth0", Flags: up, Addrs: []net.Addr{ipnet("203.0.113.7/24")}},
				{Name: "eth1", Flags: up, Addrs: []net.Addr{ipnet("10.3.0.9/16")}},
			},
			want: "10.3.0.9",
		},
		{
			name: "public only",
			ifaces: []IfaceAddrs{
				{Name: "eth0", Flags: up, Addrs: []net.Addr{ipnet("203.0.113.7/24")}},
			},
			want: "",
		},
		{
			name: "ipv6 only",
			ifaces: []IfaceAddrs{
				{Name: "eth0", Flags: up, Addrs: []net.Addr{ipnet("fd00::5/64")}},
			},
			want: "",
		},
	}
	for _, c := range cases {
		if got := PickLANIPv4(c.ifaces); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestCloudMetadataPresentURL(t *testing.T) {
	ctx := context.Background()
	serve := func(status int) string {
		s := newTestServer(status)
		t.Cleanup(s.Close)
		return s.URL
	}
	if !CloudMetadataPresentURL(ctx, serve(200)) {
		t.Error("200 should be present")
	}
	if !CloudMetadataPresentURL(ctx, serve(404)) {
		t.Error("404 should be present")
	}
	if CloudMetadataPresentURL(ctx, serve(500)) {
		t.Error("500 should be absent")
	}
	if CloudMetadataPresentURL(ctx, "http://127.0.0.1:1/") {
		t.Error("refused connection should be absent")
	}
	// Timeout honored: server sleeps past MetadataTimeout.
	old := MetadataTimeout
	MetadataTimeout = 300 * time.Millisecond
	defer func() { MetadataTimeout = old }()
	if CloudMetadataPresentURL(ctx, slowServer(t)) {
		t.Error("slow server should be absent after timeout")
	}
}

func TestIsLANAddr(t *testing.T) {
	lan := []string{
		"192.168.1.5", "10.0.0.1", "172.16.9.9", "172.31.255.1",
		"127.0.0.1", "::1", "169.254.10.20", "fe80::1", "fd00::5", "fc00::1",
		"192.168.1.5:8484", "[fd00::5]:8484",
	}
	for _, s := range lan {
		if !IsLANAddr(s) {
			t.Errorf("%s should be LAN", s)
		}
	}
	wan := []string{
		"", "8.8.8.8", "203.0.113.7", "100.64.0.5", "not-an-ip", "1.2.3.4.5",
	}
	for _, s := range wan {
		if IsLANAddr(s) {
			t.Errorf("%q should not be LAN", s)
		}
	}
}

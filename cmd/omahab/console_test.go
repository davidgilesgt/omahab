package main

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/netenv"
	"github.com/omahab/omahab/internal/tui"
)

func mustIPNet(ipStr string, maskBits int) *net.IPNet {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		panic("invalid ip: " + ipStr)
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(maskBits, 32)}
}

func TestPickLANIPv4_WLANPrivate(t *testing.T) {
	ifaces := []netenv.IfaceAddrs{
		{Name: "wlan0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("192.168.1.5", 24)}},
	}
	if got := netenv.PickLANIPv4(ifaces); got != "192.168.1.5" {
		t.Fatalf("wlan0 private: got %q want %q", got, "192.168.1.5")
	}
}

func TestPickLANIPv4_DockerSkipped(t *testing.T) {
	ifaces := []netenv.IfaceAddrs{
		{Name: "docker0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("172.17.0.1", 16)}},
		{Name: "eth0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("192.168.1.10", 24)}},
	}
	// docker0 should be skipped, eth0 returned
	if got := netenv.PickLANIPv4(ifaces); got != "192.168.1.10" {
		t.Fatalf("docker0 should be skipped, got %q want 192.168.1.10", got)
	}
	// docker alone returns empty
	ifaces2 := []netenv.IfaceAddrs{
		{Name: "docker0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("172.17.0.1", 16)}},
	}
	if got := netenv.PickLANIPv4(ifaces2); got != "" {
		t.Fatalf("docker0 alone: got %q want empty", got)
	}
}

func TestPickLANIPv4_TailscaleSkipped(t *testing.T) {
	ifaces := []netenv.IfaceAddrs{
		{Name: "tailscale0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("100.64.0.1", 10)}},
		{Name: "wlan0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("192.168.1.5", 24)}},
	}
	if got := netenv.PickLANIPv4(ifaces); got != "192.168.1.5" {
		t.Fatalf("tailscale should be skipped, got %q", got)
	}
	ifaces2 := []netenv.IfaceAddrs{
		{Name: "tailscale0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("100.64.0.1", 10)}},
	}
	if got := netenv.PickLANIPv4(ifaces2); got != "" {
		t.Fatalf("tailscale alone: got %q want empty", got)
	}
}

func TestPickLANIPv4_PublicOnly(t *testing.T) {
	ifaces := []netenv.IfaceAddrs{
		{Name: "eth0", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("8.8.8.8", 24)}},
	}
	if got := netenv.PickLANIPv4(ifaces); got != "" {
		t.Fatalf("public only: got %q want empty", got)
	}
}

func TestPickLANIPv4_FlagFiltering(t *testing.T) {
	// Down interface skipped
	ifaces := []netenv.IfaceAddrs{
		{Name: "wlan0", Flags: 0, Addrs: []net.Addr{mustIPNet("192.168.1.5", 24)}},
	}
	if got := netenv.PickLANIPv4(ifaces); got != "" {
		t.Fatalf("down iface should be skipped, got %q", got)
	}
	// Loopback skipped
	ifaces2 := []netenv.IfaceAddrs{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: []net.Addr{mustIPNet("192.168.1.5", 24)}},
	}
	if got := netenv.PickLANIPv4(ifaces2); got != "" {
		t.Fatalf("loopback should be skipped, got %q", got)
	}
	// br- skipped
	ifaces3 := []netenv.IfaceAddrs{
		{Name: "br-abc", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("192.168.1.5", 24)}},
	}
	if got := netenv.PickLANIPv4(ifaces3); got != "" {
		t.Fatalf("br- should be skipped, got %q", got)
	}
	// veth skipped
	ifaces4 := []netenv.IfaceAddrs{
		{Name: "veth123", Flags: net.FlagUp, Addrs: []net.Addr{mustIPNet("192.168.1.5", 24)}},
	}
	if got := netenv.PickLANIPv4(ifaces4); got != "" {
		t.Fatalf("veth should be skipped, got %q", got)
	}
}

func TestRenderFirstBoot_NoColorContainsURLAndToken(t *testing.T) {
	var buf bytes.Buffer
	caps := tui.Caps{IsTTY: false, ColorEnabled: false}
	renderFirstBoot(&buf, caps, "192.168.1.42", "ABCD1234")
	out := buf.String()
	if !strings.Contains(out, "http://192.168.1.42:8484") {
		t.Fatalf("expected URL, got %q", out)
	}
	if !strings.Contains(out, "ABCD1234") {
		t.Fatalf("expected token, got %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("NO_COLOR output should not contain escapes, got %q", out)
	}
}

func TestRenderFirstBoot_ColorContainsEscape(t *testing.T) {
	var buf bytes.Buffer
	caps := tui.Caps{IsTTY: true, ColorEnabled: true}
	renderFirstBoot(&buf, caps, "192.168.1.42", "ABCD1234")
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("color output should contain escape, got %q", out)
	}
	if !strings.Contains(out, "http://192.168.1.42:8484") {
		t.Fatalf("expected URL even in color, got %q", out)
	}
}

func TestRenderFirstBoot_VPSHasNoLANURL(t *testing.T) {
	t.Setenv("OMAHAB_PLACEMENT", "vps")
	var buf bytes.Buffer
	caps := tui.Caps{IsTTY: false, ColorEnabled: false}
	// Even with a LAN IP present, vps placement shows no LAN URL.
	renderFirstBoot(&buf, caps, "192.168.1.42", "ABCD1234")
	out := buf.String()
	if strings.Contains(out, "http://192.168.1.42:8484") {
		t.Fatalf("vps must not show LAN URL, got %q", out)
	}
	if !strings.Contains(out, "tailscale up") {
		t.Fatalf("vps should point at tailscale up, got %q", out)
	}
	if !strings.Contains(out, "ABCD1234") {
		t.Fatalf("vps should still show the token, got %q", out)
	}
}

func TestRenderFirstBoot_LANDefaultUnset(t *testing.T) {
	// Empty placement defaults to lan: LAN URL shown.
	var buf bytes.Buffer
	caps := tui.Caps{IsTTY: false, ColorEnabled: false}
	renderFirstBoot(&buf, caps, "192.168.1.42", "ABCD1234")
	out := buf.String()
	if !strings.Contains(out, "http://192.168.1.42:8484") {
		t.Fatalf("default lan should show LAN URL, got %q", out)
	}
}

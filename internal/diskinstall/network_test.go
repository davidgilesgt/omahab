package diskinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndValidateNetworkFile_WiredDHCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "network.json")
	nf := NetworkFile{Mode: "wired-dhcp", ConnectionUUID: "", Interface: "", Keyfile: ""}
	if err := WriteNetworkFile(path, nf); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadNetworkFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Mode != "wired-dhcp" {
		t.Fatalf("mode %q", got.Mode)
	}
	if err := ValidateNetworkFileForInstall(*got); err != nil {
		t.Fatalf("validate wired-dhcp: %v", err)
	}
}

func TestValidateNetworkFile_ProfileMissingKeyfile(t *testing.T) {
	nf := NetworkFile{Mode: "profile", ConnectionUUID: "uuid", Interface: "eth0", Keyfile: "/nonexistent"}
	if err := ValidateNetworkFileForInstall(nf); err == nil {
		t.Fatal("should fail for missing keyfile")
	}
}

func TestValidateNetworkFile_ProfileValid(t *testing.T) {
	dir := t.TempDir()
	keyfile := filepath.Join(dir, "eth0.nmconnection")
	// Minimal keyfile with PSK for wifi check - but for ethernet, no wifi, just non-empty
	if err := os.WriteFile(keyfile, []byte("[connection]\nid=eth0\n[ipv4]\nmethod=auto\n"), 0600); err != nil {
		t.Fatalf("write keyfile: %v", err)
	}
	nf := NetworkFile{Mode: "profile", ConnectionUUID: "uuid-123", Interface: "eth0", Keyfile: keyfile}
	if err := ValidateNetworkFileForInstall(nf); err != nil {
		t.Fatalf("validate profile: %v", err)
	}
}

func TestValidateNetworkFile_WiFiLacksPSK(t *testing.T) {
	dir := t.TempDir()
	keyfile := filepath.Join(dir, "wifi.nmconnection")
	content := "[connection]\nid=wifi\ntype=wifi\n[wifi]\nssid=test\n[802-11-wireless]\nssid=test\n"
	if err := os.WriteFile(keyfile, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	nf := NetworkFile{Mode: "profile", ConnectionUUID: "uuid-wifi", Interface: "wlan0", Keyfile: keyfile}
	if err := ValidateNetworkFileForInstall(nf); err == nil {
		t.Fatal("wifi without psk should fail")
	}
}

func TestWriteNetworkFile_InvalidMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "network.json")
	nf := NetworkFile{Mode: "invalid"}
	if err := WriteNetworkFile(path, nf); err == nil {
		t.Fatal("invalid mode should fail")
	}
}

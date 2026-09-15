package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// backupSSHPort is the Hetzner Storage Box SSH/SFTP port. The current SFTP
// protocol is preserved; there is no SSH-exec fallback.
const backupSSHPort = "23"

// knownHostPatterns lists every known_hosts first-field form that refers to
// host: the legacy bare form, the JoinHostPort form, and the bracketed
// OpenSSH form written for hosts with a port.
func knownHostPatterns(host string) []string {
	return []string{host, net.JoinHostPort(host, backupSSHPort), "[" + host + "]:" + backupSSHPort}
}

// knownHostLine formats one known_hosts line in OpenSSH form:
// [host]:port <keytype> <base64>. Brackets disambiguate the port separator
// for hostnames, IPv4, and IPv6 alike.
func knownHostLine(host string, key ssh.PublicKey) string {
	return "[" + host + "]:" + backupSSHPort + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func matchesBackupHost(pattern, host string) bool {
	for _, p := range knownHostPatterns(host) {
		if pattern == p {
			return true
		}
	}
	return false
}

// sshDialFunc opens the TCP connection for the backup key upload.
// Production passes a net.Dialer's DialContext; tests inject a fixture so
// dial, handshake, SFTP, and file failures are exercised without placeholder
// host special cases.
type sshDialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ensureBackupSSHKey returns the ed25519 key pair at
// <stateDir>/backup_ssh/id_ed25519 (0600) and the authorized_keys line.
// A key is generated only when absent. An existing key that cannot be
// read or parsed is reported, never silently regenerated: overwriting it
// would orphan the key already enrolled on the storage server.
func ensureBackupSSHKey(stateDir string) (string, string, error) {
	dir := filepath.Join(stateDir, "backup_ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir backup_ssh: %w", err)
	}
	privPath := filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(privPath); err == nil {
		privData, err := os.ReadFile(privPath)
		if err != nil {
			return "", "", fmt.Errorf("read existing backup key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(privData)
		if err != nil {
			return "", "", fmt.Errorf("parse existing backup key %s: %w", privPath, err)
		}
		pub := ssh.MarshalAuthorizedKey(signer.PublicKey())
		return privPath, strings.TrimSpace(string(pub)), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("stat backup key: %w", err)
	}
	// Generate new ed25519 key.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ed25519: %w", err)
	}
	// Marshal private key in OpenSSH format using ssh helper.
	privBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	pemData := pem.EncodeToMemory(privBlock)
	if err := os.WriteFile(privPath, pemData, 0o600); err != nil {
		return "", "", fmt.Errorf("write private key: %w", err)
	}
	if err := os.Chmod(privPath, 0o600); err != nil {
		return "", "", fmt.Errorf("chmod private key: %w", err)
	}
	// Derive public
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return "", "", fmt.Errorf("signer: %w", err)
	}
	pubLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return privPath, pubLine, nil
}

// uploadHetznerKey connects via SFTP with password auth on port 23, appends
// the public key to .ssh/authorized_keys exactly once, and pins the real
// server host key into backup_ssh/known_hosts. Every dial, handshake, SFTP,
// and file failure is returned; nothing is fabricated on failure. The
// password never appears in errors.
func uploadHetznerKey(ctx context.Context, stateDir, host, username, password, pubKeyLine string) error {
	var d net.Dialer
	return uploadHetznerKeyDial(ctx, stateDir, host, username, password, pubKeyLine, d.DialContext)
}

func uploadHetznerKeyDial(ctx context.Context, stateDir, host, username, password, pubKeyLine string, dial sshDialFunc) error {
	pubKeyLine = strings.TrimSpace(pubKeyLine)
	if pubKeyLine == "" {
		return errors.New("backup ssh public key is empty")
	}
	wantKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubKeyLine))
	if err != nil {
		return fmt.Errorf("parse backup ssh public key: %w", err)
	}

	dir := filepath.Join(stateDir, "backup_ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir backup_ssh: %w", err)
	}
	knownHostsPath := filepath.Join(dir, "known_hosts")

	pinned, err := loadPinnedHostKey(knownHostsPath, host)
	if err != nil {
		return err
	}

	var presented ssh.PublicKey
	callback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		presented = key
		if pinned != nil && !samePublicKey(pinned, key) {
			return fmt.Errorf("backup host key for %s changed: pinned key no longer matches", host)
		}
		return nil
	}
	cfg := &ssh.ClientConfig{
		User:            username,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: callback,
	}
	addr := net.JoinHostPort(host, backupSSHPort)

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := dial(dialCtx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial backup host %s: %w", addr, err)
	}
	// Bound the handshake by the dial deadline, then restore the connection
	// to no deadline for the SFTP session.
	if deadline, ok := dialCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return fmt.Errorf("set backup handshake deadline: %w", err)
		}
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("backup ssh handshake: %w", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = client.Close() }()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("open backup sftp: %w", err)
	}
	defer func() { _ = sftpClient.Close() }()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear backup session deadline: %w", err)
	}

	if err := sftpClient.MkdirAll(".ssh"); err != nil {
		return fmt.Errorf("mkdir remote .ssh: %w", err)
	}
	primary, err := readRemoteFile(sftpClient, ".ssh/authorized_keys")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read remote authorized_keys: %w", err)
		}
		primary = ""
	}
	if authorizedKeyPresent(primary, wantKey) {
		return pinHostKey(knownHostsPath, host, presented)
	}
	// The legacy alternate file counts for presence only when the primary
	// is absent, and never for the primary's newline accounting below.
	if primary == "" {
		secondary, err := readRemoteFile(sftpClient, ".ssh/authorized_keys2")
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read remote authorized_keys2: %w", err)
			}
		} else if authorizedKeyPresent(secondary, wantKey) {
			return pinHostKey(knownHostsPath, host, presented)
		}
	}
	if err := appendRemoteKey(sftpClient, ".ssh/authorized_keys", primary, pubKeyLine); err != nil {
		return err
	}
	if err := sftpClient.Chmod(".ssh", 0o700); err != nil {
		return fmt.Errorf("chmod remote .ssh: %w", err)
	}
	if err := sftpClient.Chmod(".ssh/authorized_keys", 0o600); err != nil {
		return fmt.Errorf("chmod remote authorized_keys: %w", err)
	}
	return pinHostKey(knownHostsPath, host, presented)
}

// readRemoteFile reads a remote file fully. Absence surfaces as
// os.ErrNotExist (via the SFTP status normalization); other failures,
// including close failures, are errors.
func readRemoteFile(client *sftp.Client, path string) (string, error) {
	f, err := client.Open(path)
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil {
		return "", fmt.Errorf("read %s: %w", path, readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close %s: %w", path, closeErr)
	}
	return string(data), nil
}

// authorizedKeyPresent reports whether want already occurs in content,
// comparing parsed keys rather than substrings so key comments or prefixes
// cannot alias unrelated entries. Unparseable lines are preserved by the
// caller and ignored here.
func authorizedKeyPresent(content string, want ssh.PublicKey) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		got, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		if samePublicKey(got, want) {
			return true
		}
	}
	return false
}

func samePublicKey(a, b ssh.PublicKey) bool {
	return a.Type() == b.Type() && bytes.Equal(a.Marshal(), b.Marshal())
}

// appendRemoteKey appends the key line exactly once, preserving unrelated
// keys and the file's trailing-newline state. The file is opened for append,
// never truncated or recreated.
func appendRemoteKey(client *sftp.Client, path, existing, pubKeyLine string) error {
	f, err := client.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("open remote authorized_keys for append: %w", err)
	}
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		if _, err := f.Write([]byte("\n")); err != nil {
			_ = f.Close()
			return fmt.Errorf("write remote authorized_keys separator: %w", err)
		}
	}
	if _, err := f.Write([]byte(pubKeyLine + "\n")); err != nil {
		_ = f.Close()
		return fmt.Errorf("write remote authorized_keys: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close remote authorized_keys: %w", err)
	}
	return nil
}

// loadPinnedHostKey returns the pinned key for host, matching both the
// current port-qualified form and bare-host entries written by earlier
// versions. A malformed pin fails closed: it is never accepted or silently
// replaced.
func loadPinnedHostKey(path, host string) (ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read known_hosts: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		matched := false
		for _, pattern := range strings.Split(fields[0], ",") {
			if matchesBackupHost(pattern, host) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ")))
		if err != nil {
			return nil, fmt.Errorf("parse pinned backup host key: %w", err)
		}
		return key, nil
	}
	return nil, nil
}

// pinHostKey stores the presented server key in port-qualified form,
// preserving pins for other hosts and normalizing this host's legacy
// bare-host entries. Trust is first-use only: an existing mismatching pin
// is rejected by the handshake callback before this runs.
func pinHostKey(path, host string, key ssh.PublicKey) error {
	if key == nil {
		return errors.New("no backup host key presented")
	}
	var keep []string
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			drop := false
			if fields := strings.Fields(line); len(fields) > 0 {
				for _, pattern := range strings.Split(fields[0], ",") {
					if matchesBackupHost(pattern, host) {
						drop = true
						break
					}
				}
			}
			if !drop {
				keep = append(keep, line)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read known_hosts: %w", err)
	}
	keep = append(keep, knownHostLine(host, key))
	if err := os.WriteFile(path, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write known_hosts: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod known_hosts: %w", err)
	}
	return nil
}

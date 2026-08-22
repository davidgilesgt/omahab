package installer

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// --- identity ---

func liveOSRelease() (OSInfo, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return OSInfo{}, fmt.Errorf("read /etc/os-release: %w", err)
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		m[k] = v
	}
	return OSInfo{
		ID:        m["ID"],
		VersionID: m["VERSION_ID"],
		Codename:  m["VERSION_CODENAME"],
		Pretty:    m["PRETTY_NAME"],
	}, nil
}

func liveArch() (string, error) {
	return runtime.GOARCH, nil
}

// --- filesystem ---

func liveFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func liveDirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func liveDirNotEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return false, err
	}
	return len(names) > 0, nil
}

func liveReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func liveWriteFile(path string, data []byte, perm uint32) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, os.FileMode(perm))
}

func liveRemoveFile(path string) error { return os.Remove(path) }

func liveMkdirAll(path string, perm uint32) error { return os.MkdirAll(path, os.FileMode(perm)) }

func liveStatFile(path string) (bool, uint32, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, 0, err
	}
	return fi.IsDir(), uint32(fi.Mode().Perm()), nil
}

func liveFileOwner(path string) (int, int, error) {
	// Best-effort: use stat via /proc or syscall; fall back to 0,0 if unavailable.
	// Implemented via "stat -c %u:%g" to avoid cgo.
	out, err := exec.Command("stat", "-c", "%u:%g", path).Output()
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected stat output: %q", string(out))
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// --- processes ---

func liveCommandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func liveCommandOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func liveRunningPids() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func liveProcessCmdline(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(data), "\x00", " "), nil
}

func liveListeningPorts() ([]int, error) {
	// Parse /proc/net/tcp and tcp6 for listening sockets.
	var ports []int
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		first := true
		for scanner.Scan() {
			if first {
				first = false
				continue
			}
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 {
				continue
			}
			state := fields[3]
			if state != "0A" { // LISTEN
				continue
			}
			local := fields[1]
			_, portHex, ok := strings.Cut(local, ":")
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			ports = append(ports, int(n))
		}
		f.Close()
	}
	return ports, nil
}

func liveServiceActive(name string) (bool, error) {
	cmd := exec.Command("systemctl", "is-active", "--quiet", name)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 3 {
		return false, nil
	}
	return false, err
}

func liveServiceEnabled(name string) (bool, error) {
	cmd := exec.Command("systemctl", "is-enabled", "--quiet", name)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// --- APT ---

func liveAPTSources() ([]AptSource, error) {
	var sources []AptSource
	// Main sources.list
	if data, err := os.ReadFile("/etc/apt/sources.list"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			sources = append(sources, AptSource{File: "/etc/apt/sources.list", Line: line})
		}
	}
	entries, err := os.ReadDir("/etc/apt/sources.list.d")
	if err != nil {
		return sources, nil
	}
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".list") || strings.HasSuffix(e.Name(), ".sources")) {
			continue
		}
		path := filepath.Join("/etc/apt/sources.list.d", e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			sources = append(sources, AptSource{File: path, Line: line})
		}
	}
	return sources, nil
}

// --- resources ---

func liveMemInfo() (MemInfo, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return MemInfo{}, err
	}
	m := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		// values are in kB
		m[key] = val * 1024
	}
	return MemInfo{
		Total:     m["MemTotal"],
		Available: m["MemAvailable"],
	}, nil
}

func liveDiskInfo(path string) (DiskInfo, error) {
	// Use statfs via "df" to avoid cgo/syscall variance.
	out, err := exec.Command("df", "--block-size=1", "--output=size,avail", path).Output()
	if err != nil {
		return DiskInfo{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return DiskInfo{}, fmt.Errorf("unexpected df output: %q", string(out))
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		return DiskInfo{}, fmt.Errorf("unexpected df output: %q", lines[1])
	}
	total, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return DiskInfo{}, err
	}
	avail, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return DiskInfo{}, err
	}
	return DiskInfo{Total: total, Free: avail}, nil
}

// --- time / network ---

func liveNow() time.Time { return time.Now().UTC() }

func liveDNSLookup(ctx context.Context, host string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	return addrs, nil
}

func liveHTTPSGet(ctx context.Context, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func liveDialTCP(ctx context.Context, address string) (net.Conn, error) {
	var d net.Dialer
	d.Timeout = 5 * time.Second
	return d.DialContext(ctx, "tcp", address)
}

// --- SSH ---

func liveSSHDConfigTest(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "sshd", "-t")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sshd -t failed: %w: %s", err, string(out))
	}
	return nil
}

func liveSSHDReload(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "systemctl", "reload", "sshd")
	if out, err := cmd.CombinedOutput(); err != nil {
		// Try "ssh" service name as fallback
		cmd2 := exec.CommandContext(ctx, "systemctl", "reload", "ssh")
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("sshd reload failed: %w: %s / %s", err, string(out), string(out2))
		}
	}
	return nil
}

func liveAuthorizedKeys(userName string) (string, []string, error) {
	u, err := user.Lookup(userName)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(u.HomeDir, ".ssh", "authorized_keys")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil, nil
		}
		return path, nil, err
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return path, keys, nil
}

func liveWriteAuthorizedKeys(_ string, path string, keys []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	content := strings.Join(keys, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	return nil
}

func liveActiveSSHSession() (bool, string, error) {
	// Fast path: an installer started directly from an SSH login shell (no
	// sudo) inherits SSH_CONNECTION ("client-addr client-port server-addr
	// server-port").
	if sshConn := os.Getenv("SSH_CONNECTION"); sshConn != "" {
		if fields := strings.Fields(sshConn); len(fields) > 0 {
			return true, fields[0], nil
		}
		return true, sshConn, nil
	}
	// sudo's env_reset strips SSH_* variables, so an installer elevated from
	// an SSH session cannot see them. Fall back to harder evidence.
	if sshdAncestor() {
		return true, "", nil
	}
	return sshdRemoteLoginSession()
}

// sshdAncestor reports whether a live sshd process is an ancestor of this
// process. sudo and exec preserve the parent chain, so this works even when
// the environment has been reset. Only live ancestors are visited: once the
// session's sshd exits it disappears from the chain.
func sshdAncestor() bool {
	pid := os.Getpid()
	for i := 0; i < 128 && pid > 1; i++ {
		name, ppid, err := procNameAndParent(pid)
		if err != nil {
			return false
		}
		if name == "sshd" || name == "sshd-session" {
			return true
		}
		pid = ppid
	}
	return false
}

// procNameAndParent reads the process name and parent PID from /proc.
// It returns an error on non-Linux systems or for unreachable processes.
func procNameAndParent(pid int) (string, int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", 0, err
	}
	name, ppid := "", -1
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "Name:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		case strings.HasPrefix(line, "PPid:"):
			if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:"))); err == nil {
				ppid = v
			}
		}
		if name != "" && ppid >= 0 {
			break
		}
	}
	if name == "" || ppid < 0 {
		return "", 0, fmt.Errorf("unreadable /proc/%d/status", pid)
	}
	return name, ppid, nil
}

// sshdRemoteLoginSession consults logind for an active remote login session.
// This covers multiplexer panes (tmux/screen) whose parent chain leads to the
// multiplexer server rather than the sshd the client is currently attached
// through. Unavailable loginctl is not an error: detection simply reports no
// session.
func sshdRemoteLoginSession() (bool, string, error) {
	out, err := exec.Command("loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return false, "", nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		props, err := exec.Command("loginctl", "show-session", fields[0],
			"-p", "State", "-p", "Remote", "-p", "RemoteHost").Output()
		if err != nil {
			continue
		}
		state, remote, host := "", "", ""
		for _, pl := range strings.Split(string(props), "\n") {
			key, value, _ := strings.Cut(pl, "=")
			switch strings.TrimSpace(key) {
			case "State":
				state = strings.TrimSpace(value)
			case "Remote":
				remote = strings.TrimSpace(value)
			case "RemoteHost":
				host = strings.TrimSpace(value)
			}
		}
		if remote == "yes" && (state == "active" || state == "online") {
			return true, host, nil
		}
	}
	return false, "", nil
}

func liveSecondSessionProbe(ctx context.Context) (bool, error) {
	// Check if there are at least 2 sshd sessions for the current user.
	// Modern Ubuntu 26.04 uses systemd sshd-session with no pts/ in who; fallback to ss and ps.
	out, err := exec.CommandContext(ctx, "who").Output()
	if err == nil {
		count := 0
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.Contains(line, "pts/") || strings.Contains(line, "sshd-session") || strings.Contains(line, "192.168.") {
				count++
			}
		}
		if count >= 2 {
			return true, nil
		}
	}
	// Fallback: count established SSH connections via ss
	if out2, err2 := exec.CommandContext(ctx, "ss", "-tn", "state", "established").Output(); err2 == nil {
		lines := strings.Split(string(out2), "\n")
		cnt := 0
		for _, l := range lines {
			if strings.Contains(l, ":22") {
				cnt++
			}
		}
		if cnt >= 2 {
			return true, nil
		}
	}
	// Fallback: count sshd processes for user via ps
	if out3, err3 := exec.CommandContext(ctx, "ps", "-o", "cmd=").Output(); err3 == nil {
		cnt := strings.Count(string(out3), "sshd-session")
		if cnt == 0 {
			cnt = strings.Count(string(out3), "sshd: omahab")
		}
		if cnt >= 2 {
			return true, nil
		}
	}
	return false, nil
}

// --- systemd rollback timer ---

func liveScheduleRollback(ctx context.Context, after time.Duration, restorePath string) error {
	// Create a transient timer that restores sshd_config.d/99-omahab.conf from backup.
	// Implementation: write a systemd service+timer in /run/systemd/system.
	service := `[Unit]
Description=Omahab SSH rollback
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'if [ -f ` + restorePath + ` ]; then cp ` + restorePath + ` /etc/ssh/sshd_config.d/99-omahab.conf; else rm -f /etc/ssh/sshd_config.d/99-omahab.conf; fi; sshd -t && systemctl reload sshd || true'
`
	timer := `[Unit]
Description=Omahab SSH rollback timer
[Timer]
OnActiveSec=` + fmt.Sprintf("%d", int(after.Seconds())) + `
Unit=omahab-ssh-rollback.service
[Install]
WantedBy=timers.target
`
	if err := os.WriteFile("/run/systemd/system/omahab-ssh-rollback.service", []byte(service), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/run/systemd/system/omahab-ssh-rollback.timer", []byte(timer), 0o644); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %w: %s", err, string(out))
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "omahab-ssh-rollback.timer").CombinedOutput(); err != nil {
		return fmt.Errorf("enable timer: %w: %s", err, string(out))
	}
	return nil
}

func liveCancelRollback(ctx context.Context) error {
	_, _ = exec.CommandContext(ctx, "systemctl", "stop", "omahab-ssh-rollback.timer").CombinedOutput()
	_, _ = exec.CommandContext(ctx, "systemctl", "disable", "omahab-ssh-rollback.timer").CombinedOutput()
	_ = os.Remove("/run/systemd/system/omahab-ssh-rollback.timer")
	_ = os.Remove("/run/systemd/system/omahab-ssh-rollback.service")
	_, _ = exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput()
	return nil
}

func liveRollbackActive(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "omahab-ssh-rollback.timer")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 3 {
		return false, nil
	}
	return false, err
}

func liveFetchGitHubKeys(ctx context.Context, username string) ([]string, error) {
	url := fmt.Sprintf("https://github.com/%s.keys", username)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github keys for %q: HTTP %d", username, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no keys found for github user %q", username)
	}
	return keys, nil
}

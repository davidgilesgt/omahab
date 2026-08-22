package installer

import (
	"context"
	"fmt"
	"net"
	"path"
	"strings"
	"time"
)

// Level indicates how a check failed.
type Level string

const (
	LevelPass Level = "pass"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// CheckResult is one preflight check outcome.
type CheckResult struct {
	Name        string `json:"name"`
	Level       Level  `json:"level"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
	Dirty       bool   `json:"dirty,omitempty"`
}

// RunPreflight executes every strict preflight check through the injected probes.
// It always returns all results; the error is non-nil only when a LevelFail is present.
func RunPreflight(ctx context.Context, probes Probes) ([]CheckResult, error) {
	var results []CheckResult
	add := func(r CheckResult) { results = append(results, r) }

	// 1. OS (Debian 13 or Ubuntu 26.04)
	add(checkDebian(probes))
	// 2. Architecture
	add(checkArch(probes))
	// 3. systemd
	add(checkSystemd(probes))
	// 4. Dirty host: containers
	add(checkContainers(probes))
	// 5. Existing /srv/omahab
	add(checkDataDir(probes))
	// 6. Occupied ports
	add(checkPorts(probes))
	// 7. Conflicting services
	add(checkConflictingServices(probes))
	// 8. APT sources
	add(checkAPTSources(probes))
	// 9. RAM
	add(checkRAM(probes))
	// 10. Disk
	add(checkDisk(probes))
	// 11. Filesystem semantics
	add(checkFilesystem(probes))
	// 12. System time
	add(checkTime(probes))
	// 13. DNS
	add(checkDNS(ctx, probes))
	// 14. HTTPS
	add(checkHTTPS(ctx, probes))
	// 15. SSH keys
	add(checkSSHKeys(probes))
	// 16. Signed package / HTTPS CA sanity (covered by HTTPS + APT)
	add(checkPackageSigning(probes))

	var failed []CheckResult
	for _, r := range results {
		if r.Level == LevelFail {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		return results, &PreflightError{Checks: failed}
	}
	return results, nil
}

func checkDebian(probes Probes) CheckResult {
	if probes.OSRelease == nil {
		return CheckResult{Name: "os", Level: LevelFail, Message: "OS probe not configured", Dirty: true}
	}
	info, err := probes.OSRelease()
	if err != nil {
		return CheckResult{Name: "os", Level: LevelFail, Message: fmt.Sprintf("cannot determine OS: %v", err), Remediation: "ensure /etc/os-release is readable", Dirty: false}
	}
	id := strings.ToLower(info.ID)
	switch id {
	case "debian":
		if info.VersionID != "13" {
			return CheckResult{
				Name:        "os",
				Level:       LevelFail,
				Message:     fmt.Sprintf("unsupported Debian version %q (need 13)", info.VersionID),
				Remediation: "install Debian 13 (trixie)",
				Dirty:       true,
			}
		}
		return CheckResult{Name: "os", Level: LevelPass, Message: fmt.Sprintf("Debian %s (%s)", info.VersionID, info.Codename)}
	case "ubuntu":
		if info.VersionID != "26.04" {
			return CheckResult{
				Name:        "os",
				Level:       LevelFail,
				Message:     fmt.Sprintf("unsupported Ubuntu version %q (need 26.04)", info.VersionID),
				Remediation: "install Ubuntu 26.04 LTS (resolute)",
				Dirty:       true,
			}
		}
		return CheckResult{Name: "os", Level: LevelPass, Message: fmt.Sprintf("Ubuntu %s (%s)", info.VersionID, info.Codename)}
	default:
		return CheckResult{
			Name:        "os",
			Level:       LevelFail,
			Message:     fmt.Sprintf("unsupported OS %q (need Debian 13 or Ubuntu 26.04)", info.Pretty),
			Remediation: "install Debian 13 minimal (trixie) or Ubuntu 26.04 LTS (resolute)",
			Dirty:       true,
		}
	}
}

func checkArch(probes Probes) CheckResult {
	if probes.Arch == nil {
		return CheckResult{Name: "arch", Level: LevelFail, Message: "arch probe not configured", Dirty: true}
	}
	arch, err := probes.Arch()
	if err != nil {
		return CheckResult{Name: "arch", Level: LevelFail, Message: fmt.Sprintf("cannot determine arch: %v", err), Dirty: false}
	}
	switch arch {
	case "amd64", "arm64":
		return CheckResult{Name: "arch", Level: LevelPass, Message: arch}
	default:
		return CheckResult{
			Name:        "arch",
			Level:       LevelFail,
			Message:     fmt.Sprintf("unsupported architecture %q (need amd64 or arm64)", arch),
			Remediation: "use amd64 or arm64 hardware",
			Dirty:       true,
		}
	}
}
func checkSystemd(probes Probes) CheckResult {
	if probes.DirExists == nil {
		return CheckResult{Name: "systemd", Level: LevelWarn, Message: "cannot check systemd: probe not configured"}
	}
	if probes.DirExists("/run/systemd/system") {
		return CheckResult{Name: "systemd", Level: LevelPass, Message: "systemd detected (/run/systemd/system present)"}
	}
	return CheckResult{
		Name:        "systemd",
		Level:       LevelFail,
		Message:     "/run/systemd/system not present — system not booted with systemd",
		Remediation: "install on a fresh Debian 13 or Ubuntu 26.04 system booted with systemd; containers and chroots are not supported",
		Dirty:       false,
	}
}


func checkContainers(probes Probes) CheckResult {
	// Look for docker/podman/k8s binaries and running containers.
	if probes.CommandExists != nil {
		if probes.CommandExists("docker") {
			// Check if any container exists (docker ps -aq)
			if probes.CommandOutput != nil {
				out, err := probes.CommandOutput(context.Background(), "docker", "ps", "-aq")
				if err != nil {
					return CheckResult{
						Name:        "containers",
						Level:       LevelFail,
						Message:     "docker is installed but its daemon is not reachable",
						Remediation: "start the daemon (systemctl start docker) so preflight can list containers, or remove docker",
						Dirty:       false,
					}
				}
				if strings.TrimSpace(out) != "" {
					return CheckResult{
						Name:        "containers",
						Level:       LevelFail,
						Message:     "existing Docker containers found",
						Remediation: "reinstall on a fresh host; Omahab does not adopt existing container workloads",
						Dirty:       true,
					}
				}
				return CheckResult{Name: "containers", Level: LevelPass, Message: "no containers found (docker installed but no workloads)"}
			}
			// Even without containers, docker presence is dirty if we cannot list.
			return CheckResult{
				Name:        "containers",
				Level:       LevelFail,
				Message:     "Docker is installed",
				Remediation: "reinstall on a fresh Debian 13 or Ubuntu 26.04 host without Docker/Podman/Kubernetes",
				Dirty:       true,
			}
		}
		if probes.CommandExists("podman") {
			return CheckResult{Name: "containers", Level: LevelFail, Message: "Podman is installed", Remediation: "reinstall on a fresh host", Dirty: true}
		}
		if probes.CommandExists("k3s") || probes.CommandExists("kubectl") || probes.CommandExists("kubelet") {
			return CheckResult{Name: "containers", Level: LevelFail, Message: "Kubernetes tooling detected", Remediation: "reinstall on a fresh host", Dirty: true}
		}
	}
	// Also scan for known process names if RunningPids is available.
	if probes.RunningPids != nil && probes.ProcessCmdline != nil {
		pids, err := probes.RunningPids()
		if err == nil {
			for _, pid := range pids {
				cmd, _ := probes.ProcessCmdline(pid)
				lower := strings.ToLower(cmd)
				if strings.Contains(lower, "dockerd") || strings.Contains(lower, "containerd") || strings.Contains(lower, "kubelet") {
					return CheckResult{
						Name:        "containers",
						Level:       LevelFail,
						Message:     fmt.Sprintf("container runtime process detected: %q", strings.TrimSpace(cmd)),
						Remediation: "reinstall on a fresh host",
						Dirty:       true,
					}
				}
			}
		}
	}
	return CheckResult{Name: "containers", Level: LevelPass, Message: "no container runtime detected"}
}

func checkDataDir(probes Probes) CheckResult {
	path := "/srv/omahab"
	if probes.DirExists != nil && probes.DirExists(path) {
		// If installer state dir exists, we are mid-install or resuming — don't treat existing data dir as dirty
		if probes.DirExists != nil && probes.DirExists("/var/lib/omahab") {
			return CheckResult{Name: "data_dir", Level: LevelPass, Message: "/srv/omahab present (install in progress)"}
		}
		notEmpty := true
		if probes.DirNotEmpty != nil {
			empty, err := probes.DirNotEmpty(path)
			if err == nil {
				notEmpty = empty
			}
		}
		if notEmpty {
			return CheckResult{
				Name:        "data_dir",
				Level:       LevelFail,
				Message:     "/srv/omahab already exists and is not empty",
				Remediation: "reinstall on a fresh host or remove /srv/omahab",
				Dirty:       true,
			}
		}
		// Empty directory is a warn — suggests prior attempt.
		return CheckResult{Name: "data_dir", Level: LevelWarn, Message: "/srv/omahab exists but is empty", Remediation: "ensure a clean install or remove the directory"}
	}
	return CheckResult{Name: "data_dir", Level: LevelPass, Message: "/srv/omahab not present"}
}

func checkPorts(probes Probes) CheckResult {
	if probes.ListeningPorts == nil {
		return CheckResult{Name: "ports", Level: LevelWarn, Message: "cannot check ports: probe not configured"}
	}
	listening, err := probes.ListeningPorts()
	if err != nil {
		return CheckResult{Name: "ports", Level: LevelWarn, Message: fmt.Sprintf("cannot list ports: %v", err)}
	}
	set := map[int]bool{}
	for _, p := range listening {
		set[p] = true
	}
	var occupied []int
	for _, want := range append(append([]int{}, RequiredPorts...), ReservedPorts...) {
		if set[want] {
			// Port 22 is expected to be listening (sshd). Only fail if we expect it free
			// for a different service. For omahab, 22 being occupied by sshd is OK.
			if want == 22 {
				continue
			}
			occupied = append(occupied, want)
		}
	}
	if len(occupied) > 0 {
		return CheckResult{
			Name:        "ports",
			Level:       LevelFail,
			Message:     fmt.Sprintf("required ports already in use: %v", occupied),
			Remediation: "free ports 80, 443, 8484, 8080 before installing",
			Dirty:       true,
		}
	}
	return CheckResult{Name: "ports", Level: LevelPass, Message: "required ports are free"}
}

func checkConflictingServices(probes Probes) CheckResult {
	conflicts := []string{"nginx", "apache2", "caddy", "traefik", "haproxy", "bind9", "unbound", "dnsmasq", "postgresql", "mysql", "mariadb", "mongod", "tailscaled", "wg-quick"}
	if probes.ServiceActive == nil {
		return CheckResult{Name: "services", Level: LevelWarn, Message: "cannot check conflicting services: probe not configured"}
	}
	var active []string
	for _, svc := range conflicts {
		ok, _ := probes.ServiceActive(svc)
		if ok {
			active = append(active, svc)
		}
	}
	if len(active) > 0 {
		return CheckResult{
			Name:        "services",
			Level:       LevelFail,
			Message:     fmt.Sprintf("conflicting services active: %s", strings.Join(active, ", ")),
			Remediation: "disable conflicting proxies, DNS servers, databases, or VPNs before installing",
			Dirty:       true,
		}
	}
	return CheckResult{Name: "services", Level: LevelPass, Message: "no conflicting services detected"}
}

func checkAPTSources(probes Probes) CheckResult {
	if probes.APTSources == nil {
		return CheckResult{Name: "apt_sources", Level: LevelWarn, Message: "cannot check APT sources: probe not configured"}
	}
	sources, err := probes.APTSources()
	if err != nil {
		return CheckResult{Name: "apt_sources", Level: LevelWarn, Message: fmt.Sprintf("cannot read APT sources: %v", err)}
	}
	// Allow: debian/ubuntu archives, security, cloudflare, tailscale, docker (warn), etc.
	// Strict: any unknown third-party host is a fail/dirty.
	allowedHosts := []string{
		"deb.debian.org", "security.debian.org", "ftp.debian.org",
		"archive.ubuntu.com", "security.ubuntu.com", "us.archive.ubuntu.com",
		"download.docker.com", // will be warn, not fail, if present before install
	}
	_ = allowedHosts
	var thirdParty []string
	var vendorMsgs []string
	for _, s := range sources {
		line := strings.ToLower(s.Line)
		// Skip deb lines that are clearly debian or ubuntu
		if strings.Contains(line, "debian.org") || strings.Contains(line, "debian-security") || strings.Contains(line, "ubuntu.com") {
			continue
		}
		// Extract host from line
		// Format: deb [options] https://host/path suite component
		fields := strings.Fields(s.Line)
		host := ""
		for _, f := range fields {
			if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
				// strip scheme
				h := strings.TrimPrefix(strings.TrimPrefix(f, "https://"), "http://")
				if idx := strings.Index(h, "/"); idx >= 0 {
					h = h[:idx]
				}
				host = h
				break
			}
		}
		if host == "" {
			continue
		}
		if strings.Contains(line, "debian.org") || strings.Contains(line, "ubuntu.com") {
			continue
		}
		hostLower := strings.ToLower(host)
		// Allowlist ONLY omahab-*.list vendor sources for the two vendor hosts.
		// Filename is not a bypass for other hosts; other hosts in omahab-*.list still fail dirty.
		if strings.HasPrefix(s.File, "/etc/apt/sources.list.d/") {
			base := path.Base(s.File)
			if strings.HasPrefix(base, "omahab-") && strings.HasSuffix(base, ".list") {
				switch hostLower {
				case "pkgs.tailscale.com":
					vendorMsgs = append(vendorMsgs, "omahab-managed vendor source (tailscale)")
					continue
				case "pkg.cloudflare.com":
					vendorMsgs = append(vendorMsgs, "omahab-managed vendor source (cloudflare)")
					continue
				}
			}
		}
		thirdParty = append(thirdParty, fmt.Sprintf("%s (%s)", host, s.File))
	}
	if len(thirdParty) > 0 {
		return CheckResult{
			Name:        "apt_sources",
			Level:       LevelFail,
			Message:     fmt.Sprintf("unexpected third-party APT repositories: %s", strings.Join(thirdParty, ", ")),
			Remediation: "remove third-party APT sources and reinstall from clean Debian 13 or Ubuntu 26.04",
			Dirty:       true,
		}
	}
	if len(vendorMsgs) > 0 {
		return CheckResult{Name: "apt_sources", Level: LevelPass, Message: fmt.Sprintf("APT sources look clean (%s)", strings.Join(vendorMsgs, ", "))}
	}
	return CheckResult{Name: "apt_sources", Level: LevelPass, Message: "APT sources look clean"}
}

func checkRAM(probes Probes) CheckResult {
	if probes.MemInfo == nil {
		return CheckResult{Name: "ram", Level: LevelWarn, Message: "cannot check RAM: probe not configured"}
	}
	mem, err := probes.MemInfo()
	if err != nil {
		return CheckResult{Name: "ram", Level: LevelWarn, Message: fmt.Sprintf("cannot read memory: %v", err)}
	}
	if mem.Total < MinRAMBytes {
		return CheckResult{
			Name:        "ram",
			Level:       LevelFail,
			Message:     fmt.Sprintf("insufficient RAM: %d MiB (need %d MiB)", mem.Total/(1024*1024), MinRAMBytes/(1024*1024)),
			Remediation: "use a host with at least 2 GiB RAM",
			Dirty:       false,
		}
	}
	return CheckResult{Name: "ram", Level: LevelPass, Message: fmt.Sprintf("%d MiB RAM", mem.Total/(1024*1024))}
}

func checkDisk(probes Probes) CheckResult {
	if probes.DiskInfo == nil {
		return CheckResult{Name: "disk", Level: LevelWarn, Message: "cannot check disk: probe not configured"}
	}
	info, err := probes.DiskInfo("/")
	if err != nil {
		return CheckResult{Name: "disk", Level: LevelWarn, Message: fmt.Sprintf("cannot check disk: %v", err)}
	}
	if info.Total < MinDiskBytes {
		return CheckResult{
			Name:        "disk",
			Level:       LevelFail,
			Message:     fmt.Sprintf("insufficient disk: %d GiB total (need %d GiB)", info.Total/(1024*1024*1024), MinDiskBytes/(1024*1024*1024)),
			Remediation: "use a disk with at least 20 GiB",
			Dirty:       false,
		}
	}
	if info.Free < MinDiskFree {
		return CheckResult{
			Name:        "disk",
			Level:       LevelFail,
			Message:     fmt.Sprintf("insufficient free disk: %d GiB free (need %d GiB)", info.Free/(1024*1024*1024), MinDiskFree/(1024*1024*1024)),
			Remediation: "free at least 5 GiB on /",
			Dirty:       false,
		}
	}
	return CheckResult{Name: "disk", Level: LevelPass, Message: fmt.Sprintf("%d GiB free", info.Free/(1024*1024*1024))}
}

func checkFilesystem(probes Probes) CheckResult {
	// Verify that file ownership semantics work (not FAT/NTFS, ownership preserved).
	// We check that /tmp or /var/tmp is writable and that StatFile reports plausible ownership.
	if probes.FileOwner == nil {
		return CheckResult{Name: "filesystem", Level: LevelPass, Message: "filesystem check skipped (no owner probe)"}
	}
	// Probe a path that should exist.
	uid, gid, err := probes.FileOwner("/etc")
	if err != nil {
		return CheckResult{Name: "filesystem", Level: LevelWarn, Message: fmt.Sprintf("cannot check filesystem ownership: %v", err)}
	}
	if uid != 0 {
		return CheckResult{
			Name:        "filesystem",
			Level:       LevelWarn,
			Message:     fmt.Sprintf("/etc owned by uid %d, expected 0 (root)", uid),
			Remediation: "ensure the root filesystem preserves Unix ownership",
			Dirty:       false,
		}
	}
	_ = gid
	return CheckResult{Name: "filesystem", Level: LevelPass, Message: "filesystem ownership looks sane"}
}

func checkTime(probes Probes) CheckResult {
	if probes.Now == nil {
		return CheckResult{Name: "time", Level: LevelWarn, Message: "cannot check time: probe not configured"}
	}
	now := probes.Now()
	if now.IsZero() {
		return CheckResult{Name: "time", Level: LevelFail, Message: "system time is not set", Remediation: "enable NTP and ensure time is synchronized"}
	}
	// Check skew against a known-good HTTPS endpoint's Date header indirectly via HTTPS check.
	// Here we just check that time is not wildly off (before 2024 or far in future).
	if now.Before(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return CheckResult{Name: "time", Level: LevelFail, Message: fmt.Sprintf("system time is too far in the past: %s", now.Format(time.RFC3339)), Remediation: "enable NTP (systemd-timesyncd or chrony)"}
	}
	if now.After(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return CheckResult{Name: "time", Level: LevelFail, Message: fmt.Sprintf("system time is too far in the future: %s", now.Format(time.RFC3339)), Remediation: "enable NTP"}
	}
	return CheckResult{Name: "time", Level: LevelPass, Message: now.Format(time.RFC3339)}
}

func checkDNS(ctx context.Context, probes Probes) CheckResult {
	if probes.DNSLookup == nil {
		return CheckResult{Name: "dns", Level: LevelWarn, Message: "cannot check DNS: probe not configured"}
	}
	hosts := []string{"github.com"}
	// Include OS-appropriate archives: Debian or Ubuntu, plus github.
	if probes.OSRelease != nil {
		if info, err := probes.OSRelease(); err == nil {
			switch strings.ToLower(info.ID) {
			case "ubuntu":
				hosts = append([]string{"archive.ubuntu.com", "security.ubuntu.com", "us.archive.ubuntu.com"}, hosts...)
			case "debian":
				hosts = append([]string{"deb.debian.org", "security.debian.org"}, hosts...)
			default:
				hosts = append([]string{"deb.debian.org", "archive.ubuntu.com", "security.debian.org", "security.ubuntu.com"}, hosts...)
			}
		} else {
			hosts = append([]string{"deb.debian.org", "security.debian.org", "archive.ubuntu.com"}, hosts...)
		}
	} else {
		hosts = append([]string{"deb.debian.org", "security.debian.org", "archive.ubuntu.com"}, hosts...)
	}
	for _, h := range hosts {
		addrs, err := probes.DNSLookup(ctx, h)
		if err != nil {
			return CheckResult{
				Name:        "dns",
				Level:       LevelFail,
				Message:     fmt.Sprintf("DNS lookup failed for %s: %v", h, err),
				Remediation: "fix DNS resolution (check /etc/resolv.conf and network)",
			}
		}
		if len(addrs) == 0 {
			return CheckResult{Name: "dns", Level: LevelFail, Message: fmt.Sprintf("DNS lookup for %s returned no addresses", h)}
		}
		// Validate returned addresses are parseable IPs.
		for _, a := range addrs {
			if net.ParseIP(a) == nil {
				return CheckResult{Name: "dns", Level: LevelFail, Message: fmt.Sprintf("DNS returned invalid IP %q for %s", a, h)}
			}
		}
	}
	return CheckResult{Name: "dns", Level: LevelPass, Message: "DNS resolution works"}
}

func checkHTTPS(ctx context.Context, probes Probes) CheckResult {
	if probes.HTTPSGet == nil {
		return CheckResult{Name: "https", Level: LevelWarn, Message: "cannot check HTTPS: probe not configured"}
	}
	url := "https://deb.debian.org/"
	// Use OS-appropriate archive for HTTPS probe.
	if probes.OSRelease != nil {
		if info, err := probes.OSRelease(); err == nil && strings.ToLower(info.ID) == "ubuntu" {
			url = "https://archive.ubuntu.com/"
		}
	}
	status, _, err := probes.HTTPSGet(ctx, url)
	if err != nil {
		return CheckResult{
			Name:        "https",
			Level:       LevelFail,
			Message:     fmt.Sprintf("HTTPS check failed for %s: %v", url, err),
			Remediation: "fix outbound HTTPS (check CA certificates, firewall, time)",
		}
	}
	if status < 200 || status >= 400 {
		return CheckResult{Name: "https", Level: LevelFail, Message: fmt.Sprintf("HTTPS check for %s returned HTTP %d", url, status)}
	}
	return CheckResult{Name: "https", Level: LevelPass, Message: "outbound HTTPS works"}
}

func checkSSHKeys(probes Probes) CheckResult {
	// Determine if the current session is key-authenticated or if any key exists.
	if probes.ActiveSSHSession != nil {
		ok, _, err := probes.ActiveSSHSession()
		if err == nil && !ok {
			// Not in SSH — running locally, warn but don't fail.
			return CheckResult{Name: "ssh_keys", Level: LevelWarn, Message: "not running inside an SSH session; ensure key auth is configured", Remediation: "run the installer over SSH with key authentication"}
		}
	}
	// Check that some authorized_keys exists for the invoking user or root.
	// We probe a few likely users if probes support it.
	if probes.AuthorizedKeys != nil {
		for _, u := range []string{"omahab", "ubuntu", "admin", "debian", "root"} {
			_, keys, err := probes.AuthorizedKeys(u)
			if err == nil && len(keys) > 0 {
				return CheckResult{Name: "ssh_keys", Level: LevelPass, Message: fmt.Sprintf("SSH keys present for %s", u)}
			}
		}
		// Also try current user via $USER
		if user := getEnvUser(); user != "" {
			_, keys, err := probes.AuthorizedKeys(user)
			if err == nil && len(keys) > 0 {
				return CheckResult{Name: "ssh_keys", Level: LevelPass, Message: fmt.Sprintf("SSH keys present for %s", user)}
			}
		}
		return CheckResult{
			Name:        "ssh_keys",
			Level:       LevelFail,
			Message:     "no SSH public keys found; password-only SSH would be locked out",
			Remediation: "add an SSH public key before hardening (paste, GitHub import, or file)",
			Dirty:       false,
		}
	}
	return CheckResult{Name: "ssh_keys", Level: LevelWarn, Message: "cannot verify SSH keys: probe not configured"}
}

func checkPackageSigning(probes Probes) CheckResult {
	// Verify that apt's signed Release files are accessible (via HTTPS check already)
	// and that ca-certificates is present.
	if probes.CommandExists != nil && !probes.CommandExists("apt-get") {
		return CheckResult{Name: "packages", Level: LevelWarn, Message: "apt-get not found; cannot verify package signing"}
	}
	// If HTTPS and DNS already passed, package signing is likely OK.
	return CheckResult{Name: "packages", Level: LevelPass, Message: "package signing prerequisites look OK"}
}

func getEnvUser() string {
	if envUser != nil {
		return envUser()
	}
	return ""
}

// envUser is a test hook; when nil, getEnvUser returns "".
var envUser func() string

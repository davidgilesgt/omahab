package health

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// CommandRunner abstracts command execution for testability.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (e execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// DiskProbe

type diskProbe struct {
	paths []string
	stat  func(string) (DiskStatus, error)
}

type DiskProbeOption func(*diskProbe)

func WithDiskStatFS(fn func(string) (DiskStatus, error)) DiskProbeOption {
	return func(p *diskProbe) { p.stat = fn }
}

func NewDiskProbe(paths []string, opts ...DiskProbeOption) DiskProbe {
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	p := &diskProbe{paths: paths, stat: defaultStatFS}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *diskProbe) CheckDisk(ctx context.Context) (DiskStatus, error) {
	if p.stat == nil {
		p.stat = defaultStatFS
	}
	var (
		worst DiskStatus
		found bool
	)
	for _, path := range p.paths {
		select {
		case <-ctx.Done():
			return DiskStatus{}, ctx.Err()
		default:
		}
		st, err := p.stat(path)
		if err != nil {
			return DiskStatus{}, err
		}
		if !found || st.UsePercent > worst.UsePercent {
			worst = st
			found = true
		}
	}
	if !found {
		return DiskStatus{}, fmt.Errorf("no paths checked")
	}
	return worst, nil
}

func defaultStatFS(path string) (DiskStatus, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskStatus{}, err
	}
	bsize := uint64(st.Bsize)
	// Prefer Bavail when available for user-visible free space.
	total := uint64(st.Blocks) * bsize
	var freeBlocks uint64
	// syscall.Statfs_t on both Darwin and Linux has Bavail.
	// Use Bavail if non-zero, else Bfree.
	if st.Bavail != 0 {
		freeBlocks = uint64(st.Bavail)
	} else {
		freeBlocks = uint64(st.Bfree)
	}
	free := freeBlocks * bsize
	if total == 0 {
		return DiskStatus{Path: path}, nil
	}
	var used uint64
	if free > total {
		used = 0
		free = total
	} else {
		used = total - free
	}
	pct := float64(used) / float64(total) * 100
	return DiskStatus{Path: path, TotalBytes: total, FreeBytes: free, UsedBytes: used, UsePercent: pct}, nil
}

// ServiceProbe

type serviceProbe struct {
	runner       CommandRunner
	dial         func(network, address string) (net.Conn, error)
	dockerSocket string
	systemdUnit  string
}

type ServiceProbeOption func(*serviceProbe)

func WithServiceRunner(r CommandRunner) ServiceProbeOption {
	return func(p *serviceProbe) { p.runner = r }
}
func WithServiceDial(fn func(network, address string) (net.Conn, error)) ServiceProbeOption {
	return func(p *serviceProbe) { p.dial = fn }
}
func WithServiceDockerSocket(path string) ServiceProbeOption {
	return func(p *serviceProbe) { p.dockerSocket = path }
}
func WithServiceSystemdUnit(unit string) ServiceProbeOption {
	return func(p *serviceProbe) { p.systemdUnit = unit }
}

func NewServiceProbe(opts ...ServiceProbeOption) ServiceProbe {
	p := &serviceProbe{
		runner:       execRunner{},
		dial:         func(n, a string) (net.Conn, error) { return net.DialTimeout(n, a, 2*time.Second) },
		dockerSocket: "/var/run/docker.sock",
		systemdUnit:  "omahabd",
	}
	for _, o := range opts {
		o(p)
	}
	if p.runner == nil {
		p.runner = execRunner{}
	}
	if p.dial == nil {
		p.dial = func(n, a string) (net.Conn, error) { return net.DialTimeout(n, a, 2*time.Second) }
	}
	if p.dockerSocket == "" {
		p.dockerSocket = "/var/run/docker.sock"
	}
	if p.systemdUnit == "" {
		p.systemdUnit = "omahabd"
	}
	return p
}

func (p *serviceProbe) CheckServices(ctx context.Context) ([]ServiceStatus, error) {
	var out []ServiceStatus
	// Docker socket ping
	dockerHealth := p.checkDocker(ctx)
	out = append(out, dockerHealth)
	// omahabd systemd
	sysHealth := p.checkSystemd(ctx)
	out = append(out, sysHealth)
	return out, nil
}

func (p *serviceProbe) checkDocker(ctx context.Context) ServiceStatus {
	conn, err := p.dial("unix", p.dockerSocket)
	if err != nil {
		return ServiceStatus{Name: "docker", Health: "degraded", Detail: "docker socket not reachable: " + redactDetail(err.Error())}
	}
	defer conn.Close()
	// Try HTTP _ping if possible
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte("GET /_ping HTTP/1.0\r\n\r\n"))
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	if n > 0 && strings.Contains(string(buf[:n]), "OK") {
		return ServiceStatus{Name: "docker", Health: "healthy", Detail: "docker socket ping ok"}
	}
	// If we could dial, consider healthy even without HTTP OK (some docker versions)
	return ServiceStatus{Name: "docker", Health: "healthy", Detail: "docker socket reachable"}
}

func (p *serviceProbe) checkSystemd(ctx context.Context) ServiceStatus {
	if p.runner == nil {
		p.runner = execRunner{}
	}
	out, err := p.runner.Run(ctx, "systemctl", "is-active", p.systemdUnit)
	s := strings.TrimSpace(string(out))
	if err != nil {
		// systemctl returns non-zero when not active; check output
		if s == "active" {
			return ServiceStatus{Name: p.systemdUnit, Health: "healthy", Detail: "systemd active"}
		}
		if s == "" {
			s = err.Error()
		}
		return ServiceStatus{Name: p.systemdUnit, Health: "degraded", Detail: "systemd not active: " + redactDetail(s)}
	}
	if s == "active" {
		return ServiceStatus{Name: p.systemdUnit, Health: "healthy", Detail: "systemd active"}
	}
	return ServiceStatus{Name: p.systemdUnit, Health: "degraded", Detail: "systemd status: " + redactDetail(s)}
}

// BackupProbe

type backupProbe struct {
	db *sql.DB
}

func NewBackupProbe(db *sql.DB) BackupProbe {
	return &backupProbe{db: db}
}

func (p *backupProbe) LastBackup(ctx context.Context) (*BackupInfo, error) {
	if p.db == nil {
		return nil, nil
	}
	var (
		id       sql.NullString
		finished sql.NullString
		status   sql.NullString
	)
	err := p.db.QueryRowContext(ctx, `SELECT id, finished_at, status FROM backup_runs WHERE kind='backup' AND status='completed' AND finished_at IS NOT NULL ORDER BY finished_at DESC LIMIT 1`).Scan(&id, &finished, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if isNoSuchTable(err) {
			return nil, nil
		}
		return nil, err
	}
	if !id.Valid || !finished.Valid {
		return nil, nil
	}
	t, err := parseBackupTime(finished.String)
	if err != nil {
		return nil, err
	}
	return &BackupInfo{ID: id.String, FinishedAt: t, Status: status.String}, nil
}

func (p *backupProbe) LastVerified(ctx context.Context) (*BackupInfo, error) {
	if p.db == nil {
		return nil, nil
	}
	var latest sql.NullString
	// Prefer passed verifications, fallback to completed restores (mirrors backups.lastVerifiedRestoreAt)
	err := p.db.QueryRowContext(ctx, `SELECT MAX(f) FROM (SELECT finished_at AS f FROM backup_verifications WHERE status='passed' AND finished_at IS NOT NULL UNION ALL SELECT finished_at AS f FROM backup_runs WHERE kind='restore' AND status='completed' AND finished_at IS NOT NULL)`).Scan(&latest)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if isNoSuchTable(err) {
			return nil, nil
		}
		return nil, err
	}
	if !latest.Valid || latest.String == "" {
		return nil, nil
	}
	t, err := parseBackupTime(latest.String)
	if err != nil {
		return nil, err
	}
	return &BackupInfo{ID: "verified", FinishedAt: t, VerifiedAt: &t, Status: "passed"}, nil
}

func parseBackupTime(s string) (time.Time, error) {
	layouts := []string{
		"2006-01-02T15:04:05.000000000Z07:00",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
	}
	var lastErr error
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}

func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table")
}

// TailscaleProbe

type tailscaleProbe struct {
	runner CommandRunner
}

type TailscaleProbeOption func(*tailscaleProbe)

func WithTailscaleRunner(r CommandRunner) TailscaleProbeOption {
	return func(p *tailscaleProbe) { p.runner = r }
}

func NewTailscaleProbe(opts ...TailscaleProbeOption) TailscaleProbe {
	p := &tailscaleProbe{runner: execRunner{}}
	for _, o := range opts {
		o(p)
	}
	if p.runner == nil {
		p.runner = execRunner{}
	}
	return p
}

func (p *tailscaleProbe) IsInstalled(ctx context.Context) (bool, string) {
	if p.runner == nil {
		p.runner = execRunner{}
	}
	out, err := p.runner.Run(ctx, "tailscale", "version")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(strings.ToLower(msg), "not found") || strings.Contains(strings.ToLower(msg), "executable file not found") {
			return false, "tailscale not installed: " + redactDetail(msg)
		}
		return false, redactDetail(msg)
	}
	return true, strings.TrimSpace(string(out))
}

type tailscaleStatusJSON struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		ID string `json:"ID"`
	} `json:"Self"`
	Peer map[string]json.RawMessage `json:"Peer"`
}

func (p *tailscaleProbe) IsLoggedIn(ctx context.Context) (bool, string) {
	if p.runner == nil {
		p.runner = execRunner{}
	}
	out, err := p.runner.Run(ctx, "tailscale", "status", "--json")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, redactDetail(msg)
	}
	var st tailscaleStatusJSON
	if err := json.Unmarshal(out, &st); err != nil {
		return false, "tailscale status parse error: " + redactDetail(err.Error())
	}
	if strings.EqualFold(st.BackendState, "Running") {
		return true, "tailscale running"
	}
	return false, "tailscale state: " + redactDetail(st.BackendState)
}

func (p *tailscaleProbe) ServerNodeVisible(ctx context.Context) (bool, string) {
	if p.runner == nil {
		p.runner = execRunner{}
	}
	out, err := p.runner.Run(ctx, "tailscale", "status", "--json")
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, redactDetail(msg)
	}
	var st tailscaleStatusJSON
	if err := json.Unmarshal(out, &st); err != nil {
		return false, "tailscale status parse error: " + redactDetail(err.Error())
	}
	if st.Self.ID == "" && len(st.Peer) == 0 {
		return false, "server node not visible (no peers)"
	}
	return true, "server node visible"
}

// DNSProbe

type dnsProbe struct {
	lookup func(ctx context.Context, host string) ([]string, error)
}

type DNSProbeOption func(*dnsProbe)

func WithDNSLookup(fn func(ctx context.Context, host string) ([]string, error)) DNSProbeOption {
	return func(p *dnsProbe) { p.lookup = fn }
}

func NewDNSProbe(opts ...DNSProbeOption) DNSProbe {
	p := &dnsProbe{
		lookup: func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		},
	}
	for _, o := range opts {
		o(p)
	}
	if p.lookup == nil {
		p.lookup = func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		}
	}
	return p
}

func (p *dnsProbe) Lookup(ctx context.Context, host string) ([]string, error) {
	return p.lookup(ctx, host)
}

// TLSProbe

type tlsProbe struct {
	dialTLS func(ctx context.Context, network, addr string) (net.Conn, error)
	timeout time.Duration
}

type TLSProbeOption func(*tlsProbe)

func WithTLSDial(fn func(ctx context.Context, network, addr string) (net.Conn, error)) TLSProbeOption {
	return func(p *tlsProbe) { p.dialTLS = fn }
}
func WithTLSTimeout(d time.Duration) TLSProbeOption {
	return func(p *tlsProbe) { p.timeout = d }
}

func NewTLSProbe(opts ...TLSProbeOption) TLSProbe {
	p := &tlsProbe{
		timeout: 5 * time.Second,
	}
	for _, o := range opts {
		o(p)
	}
	if p.dialTLS == nil {
		p.dialTLS = func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			return tls.DialWithDialer(d, network, addr, &tls.Config{})
		}
	}
	if p.timeout == 0 {
		p.timeout = 5 * time.Second
	}
	return p
}

func (p *tlsProbe) CheckTLS(ctx context.Context, urlStr string) (bool, string) {
	host := strings.TrimPrefix(strings.TrimPrefix(urlStr, "https://"), "http://")
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	if !strings.Contains(host, ":") {
		host = host + ":443"
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	conn, err := p.dialTLS(ctx, "tcp", host)
	if err != nil {
		return false, redactDetail(err.Error())
	}
	defer conn.Close()
	if tc, ok := conn.(*tls.Conn); ok {
		// Ensure handshake done
		_ = tc.HandshakeContext(ctx)
		state := tc.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			cert := state.PeerCertificates[0]
			if time.Now().After(cert.NotAfter) {
				return false, fmt.Sprintf("certificate expired %s", cert.NotAfter.Format(time.RFC3339))
			}
			if time.Until(cert.NotAfter) < 14*24*time.Hour {
				return false, fmt.Sprintf("certificate expires soon %s", cert.NotAfter.Format(time.RFC3339))
			}
		}
	}
	return true, "ok"
}

// PocketIDProbe

type pocketIDProbe struct {
	baseURL string
	client  *http.Client
}

type PocketIDProbeOption func(*pocketIDProbe)

func WithPocketIDClient(c *http.Client) PocketIDProbeOption {
	return func(p *pocketIDProbe) { p.client = c }
}

func NewPocketIDProbe(baseURL string, opts ...PocketIDProbeOption) PocketIDProbe {
	p := &pocketIDProbe{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), client: &http.Client{Timeout: 5 * time.Second}}
	for _, o := range opts {
		o(p)
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: 5 * time.Second}
	}
	return p
}

func (p *pocketIDProbe) CheckPocketID(ctx context.Context) error {
	if strings.TrimSpace(p.baseURL) == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", p.baseURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("pocketid returned %d", resp.StatusCode)
}

// InstanceProbe

type instanceProbe struct {
	db      *sql.DB
	queryFn func(ctx context.Context) (string, error)
}

type InstanceProbeOption func(*instanceProbe)

func WithInstanceQuery(fn func(ctx context.Context) (string, error)) InstanceProbeOption {
	return func(p *instanceProbe) { p.queryFn = fn }
}

func NewInstanceProbe(db *sql.DB, opts ...InstanceProbeOption) InstanceProbe {
	p := &instanceProbe{db: db}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *instanceProbe) GetInstanceID(ctx context.Context) (string, error) {
	if p.queryFn != nil {
		return p.queryFn(ctx)
	}
	if p.db == nil {
		return "", fmt.Errorf("no db configured")
	}
	var id string
	err := p.db.QueryRowContext(ctx, `SELECT id FROM instance WHERE singleton=1`).Scan(&id)
	if err != nil {
		if isNoSuchTable(err) {
			return "", fmt.Errorf("instance not initialized")
		}
		return "", err
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("no instance id")
	}
	return id, nil
}

// EncryptionProbe (LUKS)

type EncryptionProbe interface {
	CheckEncryption(ctx context.Context) (bool, string, error)
}

type NoopEncryptionProbe struct{}

func (NoopEncryptionProbe) CheckEncryption(ctx context.Context) (bool, string, error) {
	return true, "ok", nil
}

type encryptionProbe struct {
	runner   CommandRunner
	lsblkBin string
}

type EncryptionProbeOption func(*encryptionProbe)

func WithEncryptionRunner(r CommandRunner) EncryptionProbeOption {
	return func(p *encryptionProbe) { p.runner = r }
}
func WithEncryptionLsblkBin(bin string) EncryptionProbeOption {
	return func(p *encryptionProbe) { p.lsblkBin = bin }
}

func NewEncryptionProbe(opts ...EncryptionProbeOption) EncryptionProbe {
	p := &encryptionProbe{runner: execRunner{}, lsblkBin: "lsblk"}
	for _, o := range opts {
		o(p)
	}
	if p.runner == nil {
		p.runner = execRunner{}
	}
	if p.lsblkBin == "" {
		p.lsblkBin = "lsblk"
	}
	return p
}

const encryptionRecommendation = "Recommend LUKS on bare metal and encrypted Proxmox storage for VMs (DESIGN §9)."

func (p *encryptionProbe) CheckEncryption(ctx context.Context) (bool, string, error) {
	if p.runner == nil {
		p.runner = execRunner{}
	}
	out, err := p.runner.Run(ctx, p.lsblkBin, "-J", "-o", "NAME,MOUNTPOINT,FSTYPE,TYPE")
	if err != nil {
		// Try fallback without JSON
		out2, err2 := p.runner.Run(ctx, p.lsblkBin, "-o", "MOUNTPOINT,FSTYPE")
		if err2 != nil {
			// Cannot determine; return unknown as error, service will map to unknown/degraded
			return false, "", fmt.Errorf("lsblk failed: %w", err)
		}
		out = out2
		if containsCrypto(out) {
			return true, "encrypted (lsblk)", nil
		}
		return false, "unencrypted filesystem detected: " + encryptionRecommendation, nil
	}
	// JSON path: check for crypto_LUKS anywhere
	if containsCrypto(out) {
		return true, "encrypted (lsblk)", nil
	}
	// Heuristic: parse JSON for fstype
	var data struct {
		Blockdevices []struct {
			Name       string `json:"name"`
			Mountpoint *string `json:"mountpoint"`
			Fstype     *string `json:"fstype"`
			Type       string `json:"type"`
			Children   []struct {
				Name       string `json:"name"`
				Mountpoint *string `json:"mountpoint"`
				Fstype     *string `json:"fstype"`
				Type       string `json:"type"`
			} `json:"children"`
		} `json:"blockdevices"`
	}
	if jsonErr := json.Unmarshal(out, &data); jsonErr == nil {
		// Check any device has fstype crypto_LUKS
		for _, dev := range data.Blockdevices {
			if dev.Fstype != nil && strings.EqualFold(*dev.Fstype, "crypto_LUKS") {
				return true, "encrypted (lsblk)", nil
			}
			for _, ch := range dev.Children {
				if ch.Fstype != nil && strings.EqualFold(*ch.Fstype, "crypto_LUKS") {
					return true, "encrypted (lsblk)", nil
				}
			}
		}
		// Also check mountpoint "/" type?
		// If no crypto found, report unencrypted
		return false, "unencrypted filesystem detected: " + encryptionRecommendation, nil
	}
	return false, "unencrypted filesystem detected: " + encryptionRecommendation, nil
}

func containsCrypto(b []byte) bool {
	return strings.Contains(strings.ToLower(string(b)), "crypto_luks")
}

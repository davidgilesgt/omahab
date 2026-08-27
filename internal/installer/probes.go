package installer

import (
	"context"
	"io"
	"net"
	"time"
)

// OSInfo describes the host operating system.
type OSInfo struct {
	ID        string // e.g. "debian"
	VersionID string // e.g. "13"
	Codename  string // e.g. "trixie"
	Pretty    string
}

// MemInfo holds memory figures in bytes.
type MemInfo struct {
	Total     uint64
	Available uint64
}

// DiskInfo holds disk figures in bytes.
type DiskInfo struct {
	Total uint64
	Free  uint64
}

// Probes bundles every external interaction the installer needs.
// Each field is a function so tests can inject fakes without interfaces.
// A nil field means "use the default live implementation" when NewProbes is used,
// but Service never calls a nil probe — it fills defaults on construction.
type Probes struct {
	// Identity
	OSRelease func() (OSInfo, error)
	Arch      func() (string, error)

	// Filesystem
	FileExists  func(path string) bool
	DirExists   func(path string) bool
	DirNotEmpty func(path string) (bool, error)
	ReadFile    func(path string) ([]byte, error)
	WriteFile   func(path string, data []byte, perm uint32) error
	RemoveFile  func(path string) error
	MkdirAll    func(path string, perm uint32) error
	StatFile    func(path string) (isDir bool, perm uint32, err error)
	FileOwner   func(path string) (uid, gid int, err error)
	LookupUser  func(name string) (uid, gid int, homeDir string, err error)
	Chown       func(path string, uid, gid int) error
	Chmod       func(path string, perm uint32) error
	// Processes / services
	CommandExists  func(name string) bool
	CommandOutput  func(ctx context.Context, name string, args ...string) (string, error)
	CommandStream  func(ctx context.Context, combined io.Writer, name string, args ...string) error
	RunningPids    func() ([]int, error)
	ProcessCmdline func(pid int) (string, error)
	ListeningPorts func() ([]int, error)
	ServiceActive  func(name string) (bool, error)
	ServiceEnabled func(name string) (bool, error)

	// APT
	APTSources func() ([]AptSource, error)

	// Resources
	MemInfo  func() (MemInfo, error)
	DiskInfo func(path string) (DiskInfo, error)

	// Time / network
	Now       func() time.Time
	DNSLookup func(ctx context.Context, host string) ([]string, error)
	HTTPSGet  func(ctx context.Context, url string) (status int, body []byte, err error)
	DialTCP   func(ctx context.Context, address string) (net.Conn, error)

	// SSH
	SSHDConfigTest      func(ctx context.Context) error
	SSHDReload          func(ctx context.Context) error
	AuthorizedKeys      func(user string) (path string, keys []string, err error)
	WriteAuthorizedKeys func(user, path string, keys []string) error
	ActiveSSHSession    func() (bool, string, error) // ok, remoteAddr, err
	SecondSessionProbe  func(ctx context.Context) (bool, error)

	// systemd rollback timer
	ScheduleRollback func(ctx context.Context, after time.Duration, restorePath string) error
	CancelRollback   func(ctx context.Context) error
	RollbackActive   func(ctx context.Context) (bool, error)

	// GitHub keys
	FetchGitHubKeys func(ctx context.Context, username string) ([]string, error)

	// APT / package management (packages step)
	APTRefresh func(ctx context.Context) error
	APTInstall func(ctx context.Context, packages ...string) error

	// systemd management (firewall/services/daemon steps). Non-zero exit
	// returns an error carrying the combined output.
	Systemctl func(ctx context.Context, args ...string) (string, error)

	// Downloads (vendor keyrings). HTTPS only, TLS >= 1.2.
	DownloadFile func(ctx context.Context, url, destPath string) error

	// SHA256File returns the hex sha256 of a file (asset/binary records).
	SHA256File func(path string) (string, error)
}

// AptSource describes one APT source entry.
type AptSource struct {
	File    string
	Line    string
	Trusted bool
}

// Default thresholds.
const (
	MinRAMBytes  = 2 * 1024 * 1024 * 1024  // 2 GiB
	MinDiskBytes = 20 * 1024 * 1024 * 1024 // 20 GiB
	MinDiskFree  = 5 * 1024 * 1024 * 1024  // 5 GiB free
	MaxClockSkew = 5 * time.Minute
)

// RequiredPorts are checked for availability.
var RequiredPorts = []int{22, 80, 443}

// ReservedProjectPorts may be used as internal defaults but must be free at install time
// if the design reserves them (e.g. 8484 for omahabd).
var ReservedPorts = []int{8484, 8080}

// LiveProbes returns Probes wired to the real host. Used when Service is
// constructed without explicit probes (e.g. production binary).
func LiveProbes() Probes {
	return Probes{
		OSRelease:           liveOSRelease,
		Arch:                liveArch,
		FileExists:          liveFileExists,
		DirExists:           liveDirExists,
		DirNotEmpty:         liveDirNotEmpty,
		ReadFile:            liveReadFile,
		WriteFile:           liveWriteFile,
		RemoveFile:          liveRemoveFile,
		MkdirAll:            liveMkdirAll,
		StatFile:            liveStatFile,
		FileOwner:           liveFileOwner,
		LookupUser:          liveLookupUser,
		Chown:               liveChown,
		Chmod:               liveChmod,
		CommandExists:       liveCommandExists,
		CommandOutput:       liveCommandOutput,
		CommandStream:       liveCommandStream,
		RunningPids:         liveRunningPids,
		ProcessCmdline:      liveProcessCmdline,
		ListeningPorts:      liveListeningPorts,
		ServiceActive:       liveServiceActive,
		ServiceEnabled:      liveServiceEnabled,
		APTSources:          liveAPTSources,
		MemInfo:             liveMemInfo,
		DiskInfo:            liveDiskInfo,
		Now:                 liveNow,
		DNSLookup:           liveDNSLookup,
		HTTPSGet:            liveHTTPSGet,
		DialTCP:             liveDialTCP,
		SSHDConfigTest:      liveSSHDConfigTest,
		SSHDReload:          liveSSHDReload,
		AuthorizedKeys:      liveAuthorizedKeys,
		WriteAuthorizedKeys: liveWriteAuthorizedKeys,
		ActiveSSHSession:    liveActiveSSHSession,
		SecondSessionProbe:  liveSecondSessionProbe,
		ScheduleRollback:    liveScheduleRollback,
		CancelRollback:      liveCancelRollback,
		RollbackActive:      liveRollbackActive,
		FetchGitHubKeys:     liveFetchGitHubKeys,
		APTRefresh:          liveAPTRefresh,
		APTInstall:          liveAPTInstall,
		Systemctl:           liveSystemctl,
		DownloadFile:        liveDownloadFile,
		SHA256File:          liveSHA256File,
	}
}

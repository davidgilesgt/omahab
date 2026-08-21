package backups

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner performs restic repository operations. It is an explicit interface
// so orchestration behavior is testable without the restic binary.
type Runner interface {
	// Backup snapshots req.Paths in repo and returns the created snapshot.
	Backup(ctx context.Context, repo Repository, creds Credentials, req BackupRequest) (Snapshot, error)
	// Restore unpacks snapshotID into targetDir, which must exist.
	Restore(ctx context.Context, repo Repository, creds Credentials, snapshotID, targetDir string) error
}

// BackupRequest describes one restic backup invocation.
type BackupRequest struct {
	Paths []string
	Host  string
	Tags  []string
}

// backupTags are applied to every snapshot Omahab creates.
var backupTags = []string{"omahab"}

// CommandRunner runs the restic binary. Repository credentials are passed
// exclusively through the child process environment; command arguments
// never contain secret values, and stderr is redacted before it surfaces in
// errors.
type CommandRunner struct {
	// Bin is the restic binary path; empty means "restic" from PATH.
	Bin string
	// CacheDir overrides restic's cache location.
	CacheDir string
}

var _ Runner = (*CommandRunner)(nil)

func (c *CommandRunner) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "restic"
}

// env builds the child environment. Secret values appear here and nowhere
// else in a restic invocation.
func (c *CommandRunner) env(repo Repository, creds Credentials) []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if c.CacheDir != "" {
		env = append(env, "RESTIC_CACHE_DIR="+c.CacheDir)
	}
	env = append(env,
		"RESTIC_REPOSITORY="+repo.Location,
		"RESTIC_PASSWORD="+creds.Password,
	)
	if creds.AccessKey != "" {
		if creds.Username != "" {
			env = append(env, "AWS_ACCESS_KEY_ID="+creds.Username)
		}
		env = append(env, "AWS_SECRET_ACCESS_KEY="+creds.AccessKey)
	}
	return env
}

// backupPlan returns the argument vector and environment for a backup. It
// is kept separate from Backup so callers and tests can assert that secret
// values stay out of the argument vector.
func (c *CommandRunner) backupPlan(repo Repository, creds Credentials, req BackupRequest) ([]string, []string) {
	args := []string{"backup", "--json"}
	if req.Host != "" {
		args = append(args, "--host", req.Host)
	}
	for _, tag := range req.Tags {
		args = append(args, "--tag", tag)
	}
	args = append(args, req.Paths...)
	return args, c.env(repo, creds)
}

func (c *CommandRunner) Backup(ctx context.Context, repo Repository, creds Credentials, req BackupRequest) (Snapshot, error) {
	if len(req.Paths) == 0 {
		return Snapshot{}, fmt.Errorf("%w: backup request has no paths", ErrInvalid)
	}
	args, env := c.backupPlan(repo, creds, req)
	stdout, err := c.exec(ctx, args, env, creds)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := parseBackupOutput(stdout)
	if err != nil {
		return Snapshot{}, fmt.Errorf("parse restic backup output: %w", err)
	}
	snap.Paths = req.Paths
	return snap, nil
}

func (c *CommandRunner) Restore(ctx context.Context, repo Repository, creds Credentials, snapshotID, targetDir string) error {
	if snapshotID == "" || targetDir == "" {
		return fmt.Errorf("%w: snapshot id and target directory are required", ErrInvalid)
	}
	args := []string{"restore", snapshotID, "--target", targetDir}
	_, err := c.exec(ctx, args, c.env(repo, creds), creds)
	return err
}

// exec runs the restic binary and returns stdout. On failure it returns an
// error carrying redacted stderr.
func (c *CommandRunner) exec(ctx context.Context, args, env []string, creds Credentials) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("restic %s canceled: %w", args[0], ctx.Err())
	}
	if runErr != nil {
		detail := redact(strings.TrimSpace(stderr.String()), creds.Password, creds.AccessKey)
		if detail == "" {
			detail = runErr.Error()
		}
		return nil, fmt.Errorf("restic %s failed: %s", args[0], truncate(detail, maxErrorLength))
	}
	return stdout.Bytes(), nil
}

// resticBackupMessage is the subset of restic's --json backup output used
// here.
type resticBackupMessage struct {
	MessageType         string `json:"message_type"`
	SnapshotID          string `json:"snapshot_id"`
	TotalFilesProcessed int64  `json:"total_files_processed"`
	TotalBytesProcessed int64  `json:"total_bytes_processed"`
}

// parseBackupOutput extracts the final summary line from `restic backup
// --json`, which prints newline-delimited JSON status messages.
func parseBackupOutput(out []byte) (Snapshot, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var snap Snapshot
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var m resticBackupMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue // tolerate non-JSON progress lines
		}
		if m.MessageType == "summary" && m.SnapshotID != "" {
			snap = Snapshot{
				ID:        m.SnapshotID,
				FileCount: m.TotalFilesProcessed,
				SizeBytes: m.TotalBytesProcessed,
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Snapshot{}, err
	}
	if snap.ID == "" {
		return Snapshot{}, fmt.Errorf("restic reported no summary with a snapshot id")
	}
	return snap, nil
}

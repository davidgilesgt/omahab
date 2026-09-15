package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// --- local SSH/SFTP fixture ---

// fixtureFS serves a temp dir over SFTP with injectable failures.
type fixtureFS struct {
	root     string
	failMode string // "", "mkdir", "read", "open", "write", "close", "chmod"
}

func (f *fixtureFS) resolve(p string) string {
	return filepath.Join(f.root, filepath.Clean("/"+p))
}

func (f *fixtureFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if f.failMode == "read" {
		return nil, errors.New("fixture read failure")
	}
	return os.Open(f.resolve(r.Filepath))
}

func (f *fixtureFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	if f.failMode == "open" {
		return nil, errors.New("fixture open failure")
	}
	flags := r.Pflags()
	file, err := os.OpenFile(f.resolve(r.Filepath), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if flags.Append {
		return &appendWriter{File: file, failWrite: f.failMode == "write", failClose: f.failMode == "close"}, nil
	}
	return &failWriter{File: file, failWrite: f.failMode == "write", failClose: f.failMode == "close"}, nil
}

type failWriter struct {
	*os.File
	failWrite bool
	failClose bool
}

func (w *failWriter) WriteAt(p []byte, off int64) (int, error) {
	if w.failWrite {
		return 0, errors.New("fixture write failure")
	}
	return w.File.WriteAt(p, off)
}

func (w *failWriter) Close() error {
	err := w.File.Close()
	if w.failClose {
		return errors.New("fixture close failure")
	}
	return err
}

// appendWriter emulates server-side O_APPEND like OpenSSH: every write lands
// at the current end of file regardless of the requested offset.
type appendWriter struct {
	*os.File
	mu        sync.Mutex
	failWrite bool
	failClose bool
}

func (w *appendWriter) WriteAt(p []byte, _ int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failWrite {
		return 0, errors.New("fixture write failure")
	}
	if _, err := w.File.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	return w.File.Write(p)
}

func (w *appendWriter) Close() error {
	err := w.File.Close()
	if w.failClose {
		return errors.New("fixture close failure")
	}
	return err
}

func (f *fixtureFS) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Mkdir":
		if f.failMode == "mkdir" {
			return errors.New("fixture mkdir failure")
		}
		return os.Mkdir(f.resolve(r.Filepath), 0o700)
	case "Setstat":
		if f.failMode == "chmod" {
			return errors.New("fixture chmod failure")
		}
		if r.Attributes().Mode != 0 {
			return os.Chmod(f.resolve(r.Filepath), os.FileMode(r.Attributes().Mode)&os.ModePerm)
		}
		return nil
	default:
		return fmt.Errorf("fixture unsupported cmd %s", r.Method)
	}
}

type sliceLister []os.FileInfo

func (l sliceLister) ListAt(f []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(f, l[off:])
	if int64(off)+int64(n) >= int64(len(l)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fixtureFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "Stat", "Lstat":
		fi, err := os.Stat(f.resolve(r.Filepath))
		if err != nil {
			return nil, err
		}
		return sliceLister{fi}, nil
	case "List":
		entries, err := os.ReadDir(f.resolve(r.Filepath))
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil {
				return nil, err
			}
			infos = append(infos, fi)
		}
		return sliceLister(infos), nil
	default:
		return nil, fmt.Errorf("fixture unsupported list %s", r.Method)
	}
}

type sshFixture struct {
	t        *testing.T
	listener net.Listener
	dir      string
	signer   ssh.Signer
	username string
	password string

	rejectSubsystem bool
	fs              *fixtureFS

	wg sync.WaitGroup
}

func newSSHPot(t *testing.T, username, password string) *sshFixture {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fx := &sshFixture{
		t:        t,
		listener: ln,
		dir:      dir,
		signer:   signer,
		username: username,
		password: password,
		fs:       &fixtureFS{root: dir},
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if conn.User() == username && string(pass) == password {
				return nil, nil
			}
			return nil, errors.New("access denied")
		},
	}
	config.AddHostKey(signer)
	fx.wg.Add(1)
	go func() {
		defer fx.wg.Done()
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			fx.wg.Add(1)
			go func() {
				defer fx.wg.Done()
				fx.serve(raw, config)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		fx.wg.Wait()
	})
	return fx
}

func (fx *sshFixture) serve(raw net.Conn, config *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(raw, config)
	if err != nil {
		_ = raw.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, reqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range reqs {
				if req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp" {
					if fx.rejectSubsystem {
						_ = req.Reply(false, nil)
						continue
					}
					_ = req.Reply(true, nil)
					srv := sftp.NewRequestServer(ch, sftp.Handlers{
						FileGet:  fx.fs,
						FilePut:  fx.fs,
						FileCmd:  fx.fs,
						FileList: fx.fs,
					})
					_ = srv.Serve()
					// Complete the SSH channel close handshake so the
					// client's teardown does not wait on our end.
					_ = ch.Close()
					return
				}
				_ = req.Reply(false, nil)
			}
		}()
	}
}

// dialRedirect dials the fixture listener regardless of the requested
// address; the host string still drives known_hosts matching.
func (fx *sshFixture) dialRedirect(ctx context.Context, _ string, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", fx.listener.Addr().String())
}

// --- ensureBackupSSHKey ---

func TestEnsureBackupSSHKeyRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	privPath, pub1, err := ensureBackupSSHKey(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pub1, "ssh-ed25519 ") {
		t.Fatalf("pub = %q", pub1)
	}
	privPath2, pub2, err := ensureBackupSSHKey(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if privPath != privPath2 || pub1 != pub2 {
		t.Fatal("existing key was not reused")
	}
}

func TestEnsureBackupSSHKeyCorruptIsUntouched(t *testing.T) {
	stateDir := t.TempDir()
	privPath, _, err := ensureBackupSSHKey(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privPath, []byte("corrupt-not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureBackupSSHKey(stateDir); err == nil {
		t.Fatal("expected error for malformed existing key")
	}
	after, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "corrupt-not-a-key" {
		t.Fatal("malformed key was modified")
	}
}

// --- uploadHetznerKey ---

func TestUploadAppendsOncePreservesOthers(t *testing.T) {
	fx := newSSHPot(t, "u12345", "secret-pass")
	stateDir := t.TempDir()
	_, pubLine, err := ensureBackupSSHKey(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-seed an unrelated key without trailing newline.
	sshDir := filepath.Join(fx.dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2u5GK other@example"
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := uploadHetznerKeyDial(ctx, stateDir, "testbox", "u12345", "secret-pass", pubLine, fx.dialRedirect); err != nil {
		t.Fatal(err)
	}
	if err := uploadHetznerKeyDial(ctx, stateDir, "testbox", "u12345", "secret-pass", pubLine, fx.dialRedirect); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), pubLine); got != 1 {
		t.Fatalf("key occurs %d times, want 1:\n%s", got, data)
	}
	if !strings.Contains(string(data), other) {
		t.Fatalf("unrelated key lost:\n%s", data)
	}
	kh, err := os.ReadFile(filepath.Join(stateDir, "backup_ssh", "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	wantKH := "[testbox]:23 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(fx.signer.PublicKey())))
	if strings.TrimSpace(string(kh)) != wantKH {
		t.Fatalf("known_hosts = %q want %q", kh, wantKH)
	}
}

func TestUploadFindsKeyInAlternateFile(t *testing.T) {
	fx := newSSHPot(t, "u12345", "secret-pass")
	stateDir := t.TempDir()
	_, pubLine, err := ensureBackupSSHKey(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sshDir := filepath.Join(fx.dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys2"), []byte(pubLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := uploadHetznerKeyDial(context.Background(), stateDir, "testbox", "u12345", "secret-pass", pubLine, fx.dialRedirect); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sshDir, "authorized_keys")); !os.IsNotExist(err) {
		t.Fatal("primary file must not be created when the key already lives in the alternate file")
	}
}

func TestUploadRejectsChangedHostKey(t *testing.T) {
	fx := newSSHPot(t, "u12345", "secret-pass")
	stateDir := t.TempDir()
	_, pubLine, err := ensureBackupSSHKey(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := uploadHetznerKeyDial(context.Background(), stateDir, "testbox", "u12345", "secret-pass", pubLine, fx.dialRedirect); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(stateDir, "backup_ssh", "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	// Same host string, different server key.
	fx2 := newSSHPot(t, "u12345", "secret-pass")
	err = uploadHetznerKeyDial(context.Background(), stateDir, "testbox", "u12345", "secret-pass", pubLine, fx2.dialRedirect)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected changed-key error, got %v", err)
	}
	after, err := os.ReadFile(filepath.Join(stateDir, "backup_ssh", "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("pin changed on rejected key:\n%s", after)
	}
}

func TestUploadFailuresLeaveNoPin(t *testing.T) {
	stateDir := t.TempDir()
	_, pubLine, err := ensureBackupSSHKey(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	refused := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, "127.0.0.1:1")
	}
	// Raw TCP listener that closes immediately: handshake failure.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	handshakeFails := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}
	fx := newSSHPot(t, "u12345", "secret-pass")
	fx.rejectSubsystem = true

	cases := map[string]sshDialFunc{
		"refused": refused,
		"closing": handshakeFails,
		"nosftp":  fx.dialRedirect,
	}
	for name, dial := range cases {
		if err := uploadHetznerKeyDial(context.Background(), stateDir, "testbox", "u12345", "secret-pass", pubLine, dial); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	for _, mode := range []string{"mkdir", "read", "open", "write", "close", "chmod"} {
		fx2 := newSSHPot(t, "u12345", "secret-pass")
		fx2.fs.failMode = mode
		if err := uploadHetznerKeyDial(context.Background(), stateDir, "testbox", "u12345", "secret-pass", pubLine, fx2.dialRedirect); err == nil {
			t.Fatalf("%s: expected error", mode)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "backup_ssh", "known_hosts")); !os.IsNotExist(err) {
		t.Fatalf("known_hosts must not exist after failures: %v", err)
	}
}

// --- wizard propagation ---

type panicRunner struct{}

func (panicRunner) Init(_ context.Context, _ backups.Repository, _ backups.Credentials) error {
	panic("must not initialize after failed upload")
}
func (panicRunner) Backup(_ context.Context, _ backups.Repository, _ backups.Credentials, _ backups.BackupRequest) (backups.Snapshot, error) {
	panic("unreachable")
}
func (panicRunner) Restore(_ context.Context, _ backups.Repository, _ backups.Credentials, _, _ string) error {
	panic("unreachable")
}
func (panicRunner) Snapshots(_ context.Context, _ backups.Repository, _ backups.Credentials, _ int) ([]backups.SnapshotListEntry, error) {
	panic("unreachable")
}

func TestCreateBackupRepositoryFailsClosedOnUploadError(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	if err := b.store.Migrate(ctx, backups.Migrations()...); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	b.cfg.StateDir = stateDir
	b.backups = backups.New(b.store, backups.Config{}, backups.Deps{
		Runner:  panicRunner{},
		Secrets: backupSecretSource{service: b.secrets},
		Events:  newBackupsSink(b.events),
	})

	_, err := b.CreateBackupRepository(ctx, apitypes.CreateBackupRepositoryRequest{
		Kind:               "hetzner_storagebox",
		Label:              "hetzner",
		Username:           "u12345",
		Host:               "127.0.0.1",
		SubAccountPassword: "secret-pass",
	})
	if err == nil || !strings.Contains(err.Error(), "upload backup ssh key") {
		t.Fatalf("expected upload failure, got %v", err)
	}
	if strings.Contains(err.Error(), "secret-pass") {
		t.Fatal("password leaked into error")
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "backup_ssh", "known_hosts")); !os.IsNotExist(statErr) {
		t.Fatal("no pin may be recorded on failure")
	}
	repos, err := b.backups.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("no repository may be configured on failure: %v", repos)
	}
}

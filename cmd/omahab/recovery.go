package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/omahab/omahab/internal/secrets"
)

func newRecoveryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recovery",
		Short: "Recovery phrase and kit",
		Long: `Manage the 24-word recovery phrase and recovery.kit.

The phrase is shown once during setup and never stored. The kit at
/var/lib/omahab/recovery.kit contains the master key wrapped with a key
derived from the phrase. Keep the phrase offline (Bitwarden, 1Password, paper).`,
	}
	cmd.AddCommand(newRecoveryShowFingerprintCmd())
	cmd.AddCommand(newRecoveryUnlockCmd())
	cmd.AddCommand(newRecoveryRotateCmd())
	return cmd
}

func newRecoveryShowFingerprintCmd() *cobra.Command {
	var kitPath string
	c := &cobra.Command{
		Use:   "show-fingerprint",
		Short: "Show the recovery kit fingerprint",
		RunE: func(cmd *cobra.Command, args []string) error {
			if kitPath == "" {
				kitPath = defaultKitPath()
			}
			data, err := os.ReadFile(kitPath)
			if err != nil {
				return fmt.Errorf("read kit %s: %w", kitPath, err)
			}
			var kit struct {
				Fingerprint string `json:"fingerprint"`
			}
			if err := json.Unmarshal(data, &kit); err != nil {
				return fmt.Errorf("parse kit: %w", err)
			}
			if kit.Fingerprint == "" {
				return fmt.Errorf("kit has no fingerprint")
			}
			if flagJSON {
				return printJSON(map[string]string{"fingerprint": kit.Fingerprint})
			}
			fmt.Println(kit.Fingerprint)
			return nil
		},
	}
	c.Flags().StringVar(&kitPath, "kit", "", "path to recovery.kit (default /var/lib/omahab/recovery.kit or $OMAHAB_STATE_DIR/recovery.kit)")
	return c
}

func newRecoveryUnlockCmd() *cobra.Command {
	var kitPath string
	var masterPath string
	c := &cobra.Command{
		Use:   "unlock --kit <path>",
		Short: "Unlock master.key from a recovery kit (prompts for phrase)",
		Long: `Prompt for the 24-word recovery phrase, unwrap the master key from the kit, and write /var/lib/omahab/master.key when absent.

Used on a fresh machine to restore from backup: boot fresh image, copy recovery.kit, run unlock, then restore.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if kitPath == "" {
				kitPath = defaultKitPath()
			}
			if masterPath == "" {
				masterPath = defaultMasterKeyPath()
			}
			if _, err := os.Stat(masterPath); err == nil {
				return fmt.Errorf("master.key already exists at %s; refusing to overwrite", masterPath)
			}
			data, err := os.ReadFile(kitPath)
			if err != nil {
				return fmt.Errorf("read kit %s: %w", kitPath, err)
			}
			var kit struct {
				Version       int    `json:"version"`
				Fingerprint   string `json:"fingerprint"`
				MasterWrapped string `json:"master_wrapped"`
				CreatedAt     string `json:"created_at"`
			}
			if err := json.Unmarshal(data, &kit); err != nil {
				return fmt.Errorf("parse kit: %w", err)
			}
			wrapped, err := base64.StdEncoding.DecodeString(kit.MasterWrapped)
			if err != nil {
				return fmt.Errorf("decode master_wrapped: %w", err)
			}
			phrase, err := promptPhrase()
			if err != nil {
				return err
			}
			words := strings.Fields(phrase)
			if len(words) != 24 {
				return fmt.Errorf("phrase must be 24 words, got %d", len(words))
			}
			seed, err := secrets.PhraseToSeed(words)
			if err != nil {
				return fmt.Errorf("invalid phrase: %w", err)
			}
			// Verify fingerprint matches kit
			sum := sha256.Sum256(seed[:])
			fp := hex.EncodeToString(sum[:4])
			if strings.ToLower(fp) != strings.ToLower(kit.Fingerprint) {
				return fmt.Errorf("phrase fingerprint %s does not match kit fingerprint %s", fp, kit.Fingerprint)
			}
			recoveryKey := secrets.DeriveRecoveryKey(seed)
			master, err := secrets.UnwrapMasterKey(wrapped, recoveryKey)
			if err != nil {
				return fmt.Errorf("unwrap failed: %w", err)
			}
			if err := os.MkdirAll(filepath.Dir(masterPath), 0700); err != nil {
				return err
			}
			tmp := masterPath + ".tmp"
			if err := os.WriteFile(tmp, master[:], 0600); err != nil {
				return fmt.Errorf("write master.key: %w", err)
			}
			if err := os.Rename(tmp, masterPath); err != nil {
				_ = os.Remove(tmp)
				return fmt.Errorf("persist master.key: %w", err)
			}
			_ = os.Chmod(masterPath, 0600)
			// zero master
			for i := range master {
				master[i] = 0
			}
			if flagJSON {
				return printJSON(map[string]string{"master_key": masterPath, "fingerprint": fp})
			}
			fmt.Printf("master.key restored to %s (fingerprint %s)\n", masterPath, fp)
			return nil
		},
	}
	c.Flags().StringVar(&kitPath, "kit", "", "path to recovery.kit")
	c.Flags().StringVar(&masterPath, "master-key", "", "path to master.key to write")
	return c
}

func newRecoveryRotateCmd() *cobra.Command {
	var kitPath string
	var masterPath string
	c := &cobra.Command{
		Use:   "rotate",
		Short: "Generate a new recovery phrase and rewrap the kit",
		Long:  `Generate a new 24-word phrase, rewrap the current master.key, and overwrite recovery.kit. Shows the new phrase once — store it offline.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if kitPath == "" {
				kitPath = defaultKitPath()
			}
			if masterPath == "" {
				masterPath = defaultMasterKeyPath()
			}
			// Load master key
			masterRaw, err := os.ReadFile(masterPath)
			if err != nil {
				return fmt.Errorf("read master.key %s: %w", masterPath, err)
			}
			if len(masterRaw) != 32 {
				return fmt.Errorf("master.key has invalid length %d, expected 32", len(masterRaw))
			}
			var master [32]byte
			copy(master[:], masterRaw)
			for i := range masterRaw {
				masterRaw[i] = 0
			}
			words, seed, err := secrets.GenerateRecoveryPhrase()
			if err != nil {
				return fmt.Errorf("generate phrase: %w", err)
			}
			sum := sha256.Sum256(seed[:])
			fp := hex.EncodeToString(sum[:4])
			recoveryKey := secrets.DeriveRecoveryKey(seed)
			wrapped := secrets.WrapMasterKey(master, recoveryKey)
			kit := struct {
				Version       int    `json:"version"`
				Fingerprint   string `json:"fingerprint"`
				MasterWrapped string `json:"master_wrapped"`
				CreatedAt     string `json:"created_at"`
			}{
				Version:       1,
				Fingerprint:   fp,
				MasterWrapped: base64.StdEncoding.EncodeToString(wrapped),
				CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			}
			data, err := json.MarshalIndent(kit, "", "  ")
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(kitPath), 0700); err != nil {
				return err
			}
			tmp := kitPath + ".tmp"
			if err := os.WriteFile(tmp, data, 0600); err != nil {
				return fmt.Errorf("write kit: %w", err)
			}
			if err := os.Rename(tmp, kitPath); err != nil {
				_ = os.Remove(tmp)
				return fmt.Errorf("persist kit: %w", err)
			}
			_ = os.Chmod(kitPath, 0600)
			// Also update secret store via API if possible? Best-effort via local secrets directly not possible from CLI; user can re-run setup or the daemon will pick up fingerprint from kit.
			// Zero master
			for i := range master {
				master[i] = 0
			}
			if flagJSON {
				return printJSON(map[string]any{"phrase": words, "fingerprint": fp, "kit": kitPath})
			}
			fmt.Println("New recovery phrase (store offline; shown once):")
			for i, w := range words {
				fmt.Printf("%2d. %-12s", i+1, w)
				if (i+1)%4 == 0 {
					fmt.Println()
				} else {
					fmt.Print("  ")
				}
			}
			if len(words)%4 != 0 {
				fmt.Println()
			}
			fmt.Printf("\nFingerprint: %s\n", fp)
			fmt.Printf("Kit written to %s (0600)\n", kitPath)
			fmt.Println("Store the phrase in your password manager (Bitwarden, 1Password, paper). Omahab cannot show it again.")
			return nil
		},
	}
	c.Flags().StringVar(&kitPath, "kit", "", "path to recovery.kit")
	c.Flags().StringVar(&masterPath, "master-key", "", "path to master.key to read")
	return c
}

func defaultKitPath() string {
	if v := os.Getenv("OMAHAB_STATE_DIR"); v != "" {
		return filepath.Join(v, "recovery.kit")
	}
	return "/var/lib/omahab/recovery.kit"
}

func defaultMasterKeyPath() string {
	if v := os.Getenv("OMAHAB_MASTER_KEY"); v != "" {
		return v
	}
	if v := os.Getenv("OMAHAB_STATE_DIR"); v != "" {
		return filepath.Join(v, "master.key")
	}
	return "/var/lib/omahab/master.key"
}

func promptPhrase() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Print("Enter recovery phrase (24 words): ")
		// Use term.ReadPassword to hide input; phrase is sensitive.
		if pw, err := term.ReadPassword(int(os.Stdin.Fd())); err == nil {
			fmt.Println()
			return strings.TrimSpace(string(pw)), nil
		}
	}
	// Not a tty or ReadPassword failed: read from stdin (may be piped)
	reader := bufio.NewReader(os.Stdin)
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

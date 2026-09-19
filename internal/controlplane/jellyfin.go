package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Jellyfin converges like Technitium: Nix owns the service, the plugin, and
// the directories; omahabd owns the runtime state (Pocket ID OIDC client,
// Flowfin provider document, first-run admin, media library) via API + files.
//
// Upstream constraints (pinned Jellyfin 10.11.11, Flowfin SSO stable 4.3.0):
//   - services.jellyfin has no admin/library/plugin options, so Nix cannot
//     declare them; the Startup + Library HTTP APIs converge them instead.
//   - Jellyfin core has no native OIDC; the Flowfin plugin adds it and reads
//     its provider document once at server start from
//     JELLYFIN_SSO_CONFIG_FILE (config-as-code). A missing file means "not
//     configured", never a failure.
//   - The Startup wizard requires creating one local user; that account stays
//     as the plugin-mandated break-glass admin (password in secrets, used for
//     library convergence). Daily logins use Pocket ID; admins/members groups
//     map to Jellyfin roles on every login.

// jellyfinBaseURL is a var so tests can point it at a fake server.
var jellyfinBaseURL = "http://127.0.0.1:8096"

const jellyfinSSOProvider = "pocketid"

// jellyfinEmbyAuth identifies omahabd to the Jellyfin API. Jellyfin 10.x
// requires the X-Emby-Authorization header on auth calls; later calls add
// Token="..." to the same header.
const jellyfinEmbyAuth = `MediaBrowser Client="omahabd", Device="omahab", DeviceId="omahab-setup", Version="1"`

func jellyfinDataDir(b *Backend) string {
	if strings.TrimSpace(b.cfg.DataDir) == "" {
		return "/srv/omahab"
	}
	return strings.TrimSpace(b.cfg.DataDir)
}

func jellyfinLibraryPath(b *Backend) string {
	return filepath.Join(jellyfinDataDir(b), "apps", "jellyfin", "library")
}

func jellyfinSSODir(b *Backend) string {
	return filepath.Join(jellyfinDataDir(b), "apps", "jellyfin", "sso")
}

// jellyfinOIDCCallback is the redirect URI registered in Pocket ID. The
// trailing segment must equal the provider key in the Flowfin document.
func jellyfinOIDCCallback(domainName string) string {
	return "https://media." + strings.TrimSpace(domainName) + "/sso/OID/redirect/" + jellyfinSSOProvider
}

// jellyfinSSOProviderConfig is one Flowfin OpenID provider. The secret is a
// file reference, never an inline value (the document lives in versioned
// state and backups; the secret lives in the platform vault + one file).
type jellyfinSSOProviderConfig struct {
	OidEndpoint         string   `json:"OidEndpoint"`
	OidClientId         string   `json:"OidClientId"`
	OidSecretFile       string   `json:"OidSecretFile"`
	Enabled             bool     `json:"Enabled"`
	EnableAuthorization bool     `json:"EnableAuthorization"`
	RoleClaim           string   `json:"RoleClaim"`
	Roles               []string `json:"Roles"`
	AdminRoles          []string `json:"AdminRoles"`
	EnableAllFolders    bool     `json:"EnableAllFolders"`
}

// jellyfinProviderDoc renders the Flowfin config-as-code document. Pocket ID
// groups are admins/members (SeedDefaultGroups); both may log in, only
// admins receive the Jellyfin administrator role.
func jellyfinProviderDoc(domainName, clientID, secretPath string) ([]byte, error) {
	doc := map[string]any{
		"FormatVersion": 1,
		"Configuration": map[string]any{
			"OidConfigs": map[string]any{
				jellyfinSSOProvider: jellyfinSSOProviderConfig{
					OidEndpoint:         "https://id." + strings.TrimSpace(domainName) + "/.well-known/openid-configuration",
					OidClientId:         strings.TrimSpace(clientID),
					OidSecretFile:       secretPath,
					Enabled:             true,
					EnableAuthorization: true,
					RoleClaim:           "groups",
					Roles:               []string{"admins", "members"},
					AdminRoles:          []string{"admins"},
					EnableAllFolders:    true,
				},
			},
			"SamlConfigs": map[string]any{},
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// writeJellyfinSSOFile converges one file under the SSO dir (root-rendered,
// jellyfin-readable). It reports whether the file changed.
func writeJellyfinSSOFile(path string, data []byte, mode os.FileMode) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("mkdir sso dir: %w", err)
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return false, fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	// A missing jellyfin user (unit tests outside the NixOS closure) is not
	// an error: the caller keeps root-owned files rather than failing setup.
	_ = chownToUser(path, "jellyfin")
	return true, nil
}

// ensureJellyfinOIDC registers the Pocket ID client and renders the Flowfin
// provider document + secret file, restarting Jellyfin once when either
// changed (the plugin reads the document only at server start).
func (b *Backend) ensureJellyfinOIDC(ctx context.Context, domainName string) error {
	domainName = strings.TrimSpace(domainName)
	if domainName == "" {
		return fmt.Errorf("domain required")
	}
	callback := jellyfinOIDCCallback(domainName)
	clientID, clientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "jellyfin", []string{callback})
	if err != nil {
		return fmt.Errorf("ensure oidc client jellyfin: %w", err)
	}
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("oidc client jellyfin returned empty clientID")
	}
	clientSecret, err = reuseStoredOIDCSecret(ctx, b.secrets, "jellyfin_oidc_client_secret", clientSecret)
	if err != nil {
		clientSecret, err = b.pocketClient.CreateOIDCClientSecret(ctx, clientID)
		if err != nil {
			return fmt.Errorf("jellyfin oidc client secret: %w", err)
		}
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "jellyfin_oidc_client_id", clientID); err != nil {
		return fmt.Errorf("store jellyfin_oidc_client_id: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "jellyfin_oidc_client_secret", clientSecret); err != nil {
		return fmt.Errorf("store jellyfin_oidc_client_secret: %w", err)
	}
	if err := b.pocketClient.EnsureOIDCClientGroupAccess(ctx, clientID, []string{"admins", "members"}); err != nil {
		return fmt.Errorf("ensure jellyfin group access: %w", err)
	}

	secretPath := filepath.Join(jellyfinSSODir(b), "pocketid-secret")
	docPath := filepath.Join(jellyfinSSODir(b), "providers.json")
	doc, err := jellyfinProviderDoc(domainName, clientID, secretPath)
	if err != nil {
		return err
	}
	secretChanged, err := writeJellyfinSSOFile(secretPath, []byte(strings.TrimSpace(clientSecret)+"\n"), 0o640)
	if err != nil {
		return err
	}
	docChanged, err := writeJellyfinSSOFile(docPath, doc, 0o640)
	if err != nil {
		return err
	}
	if secretChanged || docChanged {
		log.Printf("jellyfin: provider document changed, restarting to pick it up")
		if err := b.redeployBundle(ctx, "jellyfin"); err != nil {
			return fmt.Errorf("jellyfin restart after sso render: %w", err)
		}
	}
	log.Printf("jellyfin: pocket id sso ensured (groups admins/members)")
	return nil
}

// jellyfinCall performs one JSON Jellyfin API call. A nonempty token adds it
// to the Emby authorization header. 2xx is success; anything else is an error
// carrying the status and the first bytes of the body.
func jellyfinCall(ctx context.Context, method, path string, query url.Values, body any, token string) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	u := jellyfinBaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	auth := jellyfinEmbyAuth
	if strings.TrimSpace(token) != "" {
		auth += `, Token="` + strings.TrimSpace(token) + `"`
	}
	req.Header.Set("X-Emby-Authorization", auth)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jellyfin %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type jellyfinPublicInfo struct {
	StartupWizardCompleted bool `json:"StartupWizardCompleted"`
}

type jellyfinVirtualFolder struct {
	Name           string   `json:"Name"`
	CollectionType string   `json:"CollectionType"`
	Locations      []string `json:"Locations"`
}

// ensureJellyfinSetup converges first-run state: the wizard's mandatory local
// admin (kept as break-glass; password in secrets) and the Media library over
// the drops target dir. Fresh systems fail closed; previously configured
// systems with an unknown admin password warn and continue instead of failing
// setup — the household never loses a working server to a forgotten local
// password.
func (b *Backend) ensureJellyfinSetup(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	var lastErr error
	for {
		body, err := jellyfinCall(waitCtx, http.MethodGet, "/System/Info/Public", nil, nil, "")
		if err == nil {
			var info jellyfinPublicInfo
			if jerr := json.Unmarshal(body, &info); jerr == nil {
				lastErr = nil
				break
			} else {
				lastErr = jerr
			}
		} else {
			lastErr = err
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("jellyfin not ready on :8096: %w", lastErr)
		case <-time.After(2 * time.Second):
		}
	}

	body, err := jellyfinCall(ctx, http.MethodGet, "/System/Info/Public", nil, nil, "")
	if err != nil {
		return err
	}
	var info jellyfinPublicInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return err
	}
	fresh := !info.StartupWizardCompleted

	var password string
	if fresh {
		if b.secrets == nil {
			return fmt.Errorf("jellyfin breakglass storage not configured")
		}
		if pw, err := b.secrets.RevealByName(ctx, "platform-app", "jellyfin_breakglass_password"); err == nil && strings.TrimSpace(pw) != "" {
			password = strings.TrimSpace(pw)
		} else {
			generated, err := randomAdminPassword()
			if err != nil {
				return err
			}
			password = generated
			if err := upsertSecret(ctx, b.secrets, "platform-app", "jellyfin_breakglass_password", password); err != nil {
				return fmt.Errorf("store jellyfin breakglass password: %w", err)
			}
		}
		// Upstream initializes the first user during GET /Startup/User.
		// POST returns 404 until that initialization succeeds.
		if _, err := jellyfinCall(ctx, http.MethodGet, "/Startup/User", nil, nil, ""); err != nil {
			return fmt.Errorf("jellyfin init first user: %w", err)
		}
		if _, err := jellyfinCall(ctx, http.MethodPost, "/Startup/User", nil, map[string]string{
			"Name": "admin", "Password": password,
		}, ""); err != nil {
			return fmt.Errorf("jellyfin first user: %w", err)
		}
		authBody, err := jellyfinCall(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, map[string]string{
			"Username": "admin", "Pw": password,
		}, "")
		if err != nil {
			return fmt.Errorf("jellyfin breakglass auth: %w", err)
		}
		var freshAuth struct {
			AccessToken string `json:"AccessToken"`
		}
		if err := json.Unmarshal(authBody, &freshAuth); err != nil || strings.TrimSpace(freshAuth.AccessToken) == "" {
			return fmt.Errorf("jellyfin auth returned no token")
		}
		freshToken := strings.TrimSpace(freshAuth.AccessToken)
		if _, err := jellyfinCall(ctx, http.MethodPost, "/Startup/Complete", nil, map[string]any{}, ""); err != nil {
			return fmt.Errorf("jellyfin complete wizard: %w", err)
		}
		log.Printf("jellyfin: first-run admin created (break-glass, kept in secrets)")
		token := freshToken
		foldersBody, err := jellyfinCall(ctx, http.MethodGet, "/Library/VirtualFolders", nil, nil, token)
		if err != nil {
			return fmt.Errorf("jellyfin list libraries: %w", err)
		}
		var folders []jellyfinVirtualFolder
		if err := json.Unmarshal(foldersBody, &folders); err != nil {
			return fmt.Errorf("jellyfin parse libraries: %w", err)
		}
		library := jellyfinLibraryPath(b)
		for _, f := range folders {
			for _, loc := range f.Locations {
				if filepath.Clean(loc) == filepath.Clean(library) {
					log.Printf("jellyfin: library already covers %s", library)
					return nil
				}
			}
		}
		q := url.Values{
			"name":           {"Media"},
			"collectionType": {"mixed"},
			"paths":          {library},
			"refreshLibrary": {"true"},
		}
		if _, err := jellyfinCall(ctx, http.MethodPost, "/Library/VirtualFolders", q, nil, token); err != nil {
			return fmt.Errorf("jellyfin create library: %w", err)
		}
		log.Printf("jellyfin: library Media created over %s", library)
		return nil
	} else if b.secrets == nil {
		log.Printf("jellyfin: local admin password unknown, skipping library converge")
		return nil
	} else if pw, err := b.secrets.RevealByName(ctx, "platform-app", "jellyfin_breakglass_password"); err == nil && strings.TrimSpace(pw) != "" {
		password = strings.TrimSpace(pw)
	}
	if password == "" {
		// Configured before automation (or secret lost): the local admin
		// password is unknown. Library convergence needs auth; skip it
		// loudly rather than failing a healthy server.
		log.Printf("jellyfin: local admin password unknown, skipping library converge")
		return nil
	}

	authBody, err := jellyfinCall(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, map[string]string{
		"Username": "admin", "Pw": password,
	}, "")
	if err != nil {
		log.Printf("jellyfin: local admin auth failed, skipping library converge")
		return nil
	}
	var auth struct {
		AccessToken string `json:"AccessToken"`
	}
	if err := json.Unmarshal(authBody, &auth); err != nil || strings.TrimSpace(auth.AccessToken) == "" {
		log.Printf("jellyfin: local admin auth failed, skipping library converge")
		return nil
	}
	token := strings.TrimSpace(auth.AccessToken)

	foldersBody, err := jellyfinCall(ctx, http.MethodGet, "/Library/VirtualFolders", nil, nil, token)
	if err != nil {
		return fmt.Errorf("jellyfin list libraries: %w", err)
	}
	var folders []jellyfinVirtualFolder
	if err := json.Unmarshal(foldersBody, &folders); err != nil {
		return fmt.Errorf("jellyfin parse libraries: %w", err)
	}
	library := jellyfinLibraryPath(b)
	for _, f := range folders {
		for _, loc := range f.Locations {
			if filepath.Clean(loc) == filepath.Clean(library) {
				log.Printf("jellyfin: library already covers %s", library)
				return nil
			}
		}
	}
	q := url.Values{
		"name":           {"Media"},
		"collectionType": {"mixed"},
		"paths":          {library},
		"refreshLibrary": {"true"},
	}
	if _, err := jellyfinCall(ctx, http.MethodPost, "/Library/VirtualFolders", q, nil, token); err != nil {
		return fmt.Errorf("jellyfin create library: %w", err)
	}
	log.Printf("jellyfin: library Media created over %s", library)
	return nil
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// End-to-end owner-consent flow for companion tool-environment distribution:
// enroll (no auto-grant) -> fetch empty -> grant -> set v1 -> fetch v1 ->
// rotate v2 -> fetch v2 -> revoke -> fetch empty. Diagnostics (device list,
// grant responses) carry names/consent/revision only, never secret values.
func TestCompanionEnvironmentGrantFlow(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv, err := New(Config{
		Backend:      backend,
		Environments: backend.EnvironmentsForTest(),
		BearerToken:  "test-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	do := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
		t.Helper()
		if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
			t.Fatalf("decode %s: %v (body %q)", rec.Result().Status, err, rec.Body.String())
		}
	}

	// Anonymous grant attempt must not succeed.
	if rec := do(http.MethodPost, "/api/v1/companion/devices/nope/environment-grant", "", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous grant = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}

	// 1. Enroll a device.
	rec := do(http.MethodPost, "/api/v1/companion-enrollments", "test-token", `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create enrollment = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	var enr struct {
		Code string `json:"code"`
	}
	decode(t, rec, &enr)
	rec = do(http.MethodPost, "/api/v1/companion/enroll", "", `{"code":`+strconv.Quote(enr.Code)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("enroll = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	var enrolled struct {
		Token string `json:"token"`
	}
	decode(t, rec, &enrolled)
	if !strings.HasPrefix(enrolled.Token, "oma_dev_") {
		t.Fatalf("bad token prefix %q", enrolled.Token)
	}

	// Device ID comes from the admin list (enroll response carries no ID).
	rec = do(http.MethodGet, "/api/v1/companion/devices", "test-token", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list devices = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Items []struct {
			ID      string `json:"id"`
			Granted bool   `json:"granted"`
		} `json:"items"`
	}
	decode(t, rec, &listed)
	if len(listed.Items) != 1 {
		t.Fatalf("devices = %d, want 1", len(listed.Items))
	}
	deviceID := listed.Items[0].ID
	if listed.Items[0].Granted {
		t.Fatalf("freshly enrolled device granted=true, want false (no automatic grant)")
	}

	envGet := func() map[string]string {
		t.Helper()
		rec := do(http.MethodGet, "/api/v1/companion/environment", enrolled.Token, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("device environment = %d, want 200, body = %s", rec.Code, rec.Body.String())
		}
		var bundle map[string]string
		decode(t, rec, &bundle)
		return bundle
	}

	// 2. Fresh enrollment: empty bundle (probe absent).
	if bundle := envGet(); len(bundle) != 0 {
		t.Fatalf("ungranted bundle has %d keys, want empty", len(bundle))
	}

	// 3. Grant unknown device -> 404.
	if rec := do(http.MethodPost, "/api/v1/companion/devices/does-not-exist/environment-grant", "test-token", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("grant unknown = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}

	// 4. Explicit grant.
	rec = do(http.MethodPost, "/api/v1/companion/devices/"+deviceID+"/environment-grant", "test-token", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var grant struct {
		DeviceID string `json:"device_id"`
		Granted  bool   `json:"granted"`
		Revision int    `json:"revision"`
	}
	decode(t, rec, &grant)
	if !grant.Granted || grant.DeviceID != deviceID {
		t.Fatalf("grant response = %+v, want granted=true for %s", grant, deviceID)
	}
	if strings.Contains(rec.Body.String(), "probe-secret") {
		t.Fatalf("grant response leaks secret value: %s", rec.Body.String())
	}

	const varName = "FINAL6C32E73_PROBE"

	// 5. Set v1 -> device fetches current value.
	rec = do(http.MethodPut, "/api/v1/tool-environment/"+varName, "test-token", `{"value":"probe-secret-v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put v1 = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if bundle := envGet(); bundle[varName] != "probe-secret-v1" {
		t.Fatalf("granted bundle[%s] = %q, want v1", varName, bundle[varName])
	}

	// 6. Rotate v2 -> visible without re-grant.
	rec = do(http.MethodPut, "/api/v1/tool-environment/"+varName, "test-token", `{"value":"probe-secret-v2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put v2 = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var meta struct {
		Version int `json:"version"`
	}
	decode(t, rec, &meta)
	if meta.Version != 2 {
		t.Fatalf("rotated version = %d, want 2", meta.Version)
	}
	if bundle := envGet(); bundle[varName] != "probe-secret-v2" {
		t.Fatalf("granted bundle[%s] = %q, want v2", varName, bundle[varName])
	}

	// 7. Revoke -> bundle empty again.
	rec = do(http.MethodDelete, "/api/v1/companion/devices/"+deviceID+"/environment-grant", "test-token", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var revoked struct {
		Granted bool `json:"granted"`
	}
	decode(t, rec, &revoked)
	if revoked.Granted {
		t.Fatalf("revoke response granted=true, want false")
	}
	if bundle := envGet(); len(bundle) != 0 {
		t.Fatalf("revoked bundle has %d keys, want empty", len(bundle))
	}

	// 8. List reflects consent withdrawal; metadata endpoint never returns values.
	rec = do(http.MethodGet, "/api/v1/companion/devices", "test-token", "")
	decode(t, rec, &listed)
	if listed.Items[0].Granted {
		t.Fatalf("device granted=true after revoke, want false")
	}
	rec = do(http.MethodGet, "/api/v1/tool-environment", "test-token", "")
	if strings.Contains(rec.Body.String(), "probe-secret") {
		t.Fatalf("tool-environment metadata leaks secret value: %s", rec.Body.String())
	}

	// 9. Revoke unknown device -> 404 (typos are visible).
	if rec := do(http.MethodDelete, "/api/v1/companion/devices/does-not-exist/environment-grant", "test-token", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}

}

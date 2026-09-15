package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyProxyBind(t *testing.T) {
	t.Setenv("OMAHAB_PROXY_BIND", "")
	require.NoError(t, applyProxyBind(""))
	assert.Empty(t, os.Getenv("OMAHAB_PROXY_BIND"))

	require.NoError(t, applyProxyBind("127.0.0.1:8080"))
	assert.Equal(t, "127.0.0.1:8080", os.Getenv("OMAHAB_PROXY_BIND"))

	require.NoError(t, applyProxyBind(":8080"))
	assert.Equal(t, ":8080", os.Getenv("OMAHAB_PROXY_BIND"))

	for _, bad := range []string{"8080", "example.com:8080", "0.0.0.0:8080", "127.0.0.1:", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:notaport"} {
		assert.Error(t, applyProxyBind(bad), "bind %q", bad)
	}
}

func TestParseSecretsFileRejectsBadLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.env")
	content := "# comment\n\nVALID=value\nOTHER = spaced\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	got, err := parseSecretsFile(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"VALID": "value", "OTHER": "spaced"}, got)

	bad := filepath.Join(dir, "bad.env")
	require.NoError(t, os.WriteFile(bad, []byte("OK=1\nMISSING_EQUALS\n"), 0o600))
	_, err = parseSecretsFile(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad.env")
	assert.Contains(t, err.Error(), "line 2")
	assert.NotContains(t, err.Error(), "MISSING_EQUALS")

	emptyKey := filepath.Join(dir, "emptykey.env")
	require.NoError(t, os.WriteFile(emptyKey, []byte("=nokey\n"), 0o600))
	_, err = parseSecretsFile(emptyKey)
	require.Error(t, err)
}

func TestMergeSecretsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.env")
	require.NoError(t, os.WriteFile(path, []byte("A=1\nB=2\n"), 0o600))
	f := &settingsFlags{env: []string{"C=3"}}
	require.NoError(t, mergeSecretsFile(f, path))
	assert.Len(t, f.env, 3)

	require.Error(t, mergeSecretsFile(f, filepath.Join(dir, "missing.env")))
}

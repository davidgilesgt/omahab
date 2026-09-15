package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxyLoopbackPort(t *testing.T) {
	t.Setenv("OMAHAB_PROXY_BIND", "")
	port, err := proxyLoopbackPort()
	require.NoError(t, err)
	assert.Zero(t, port)

	for value, want := range map[string]int{
		"127.0.0.1:8080": 8080,
		":8080":          8080,
		"  :443  ":       443,
	} {
		t.Setenv("OMAHAB_PROXY_BIND", value)
		port, err := proxyLoopbackPort()
		require.NoError(t, err, "value %q", value)
		assert.Equal(t, want, port, "value %q", value)
	}

	for _, bad := range []string{"8080", "example.com:8080", "0.0.0.0:8080", "[::1]:8080", "127.0.0.1:", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:http"} {
		t.Setenv("OMAHAB_PROXY_BIND", bad)
		_, err := proxyLoopbackPort()
		assert.Error(t, err, "value %q", bad)
	}
}

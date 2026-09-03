package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsPortConflict(t *testing.T) {
	assert.True(t, isPortConflict(errors.New("Ports are not available: listen tcp :80: bind: address already in use")))
	assert.True(t, isPortConflict(errors.New("driver failed programming external connectivity: port is already allocated")))
	assert.False(t, isPortConflict(errors.New("something else went wrong")))
	assert.False(t, isPortConflict(nil))
}

func TestErrorMessage(t *testing.T) {
	t.Run("returns description for described error", func(t *testing.T) {
		assert.Equal(t, ErrProxyPortInUse.Description(), ErrorMessage(ErrProxyPortInUse))
	})

	t.Run("returns description for wrapped described error", func(t *testing.T) {
		wrapped := fmt.Errorf("setup failed: %w", ErrProxyPortInUse)
		assert.Equal(t, ErrProxyPortInUse.Description(), ErrorMessage(wrapped))
	})

	t.Run("returns Error for plain error", func(t *testing.T) {
		err := errors.New("something broke")
		assert.Equal(t, "something broke", ErrorMessage(err))
	})
}

func TestConnectionError(t *testing.T) {
	pingError := func(t *testing.T, socketPath string) error {
		c, err := client.New(client.WithHost("unix://" + socketPath))
		require.NoError(t, err)
		_, err = c.Ping(context.Background(), client.PingOptions{})
		require.Error(t, err)
		return err
	}

	t.Run("permission denied", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can always connect to the socket")
		}

		socketPath := filepath.Join(t.TempDir(), "docker.sock")
		listener, err := net.Listen("unix", socketPath)
		require.NoError(t, err)
		defer listener.Close()
		require.NoError(t, os.Chmod(socketPath, 0))

		err = connectionError(pingError(t, socketPath))
		assert.ErrorIs(t, err, ErrDockerPermissionDenied)
		assert.Equal(t, ErrDockerPermissionDenied.Description(), ErrorMessage(err))
	})

	t.Run("daemon not running", func(t *testing.T) {
		socketPath := filepath.Join(t.TempDir(), "missing.sock")

		err := connectionError(pingError(t, socketPath))
		assert.ErrorIs(t, err, ErrDockerNotRunning)
		assert.Equal(t, ErrDockerNotRunning.Description(), ErrorMessage(err))
	})

	t.Run("passes through other errors", func(t *testing.T) {
		assert.Equal(t, assert.AnError, connectionError(assert.AnError))
		assert.NoError(t, connectionError(nil))
	})
}

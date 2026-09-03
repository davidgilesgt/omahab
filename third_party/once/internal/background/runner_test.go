package background

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRunnerShutdown(t *testing.T) {
	runner := NewRunner("test")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runner.Run(ctx)
	assert.NoError(t, err)
}

func TestCheckInterval(t *testing.T) {
	assert.Equal(t, 5*time.Minute, CheckInterval)
}

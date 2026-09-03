package command

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitExecArgs(t *testing.T) {
	host, commandArgs, err := splitExecArgs([]string{"myapp.example.com", "rails", "console"})

	require.NoError(t, err)
	assert.Equal(t, "myapp.example.com", host)
	assert.Equal(t, []string{"rails", "console"}, commandArgs)
}

func TestSplitExecArgsAllowsFlagArgs(t *testing.T) {
	host, commandArgs, err := splitExecArgs([]string{"myapp.example.com", "ls", "-la"})

	require.NoError(t, err)
	assert.Equal(t, "myapp.example.com", host)
	assert.Equal(t, []string{"ls", "-la"}, commandArgs)
}

func TestSplitExecArgsAllowsSeparator(t *testing.T) {
	host, commandArgs, err := splitExecArgs([]string{"myapp.example.com", "--", "sh", "-lc", "echo hi"})

	require.NoError(t, err)
	assert.Equal(t, "myapp.example.com", host)
	assert.Equal(t, []string{"sh", "-lc", "echo hi"}, commandArgs)
}

func TestSplitExecArgsRequiresCommand(t *testing.T) {
	_, _, err := splitExecArgs([]string{"myapp.example.com", "--"})

	require.Error(t, err)
	assert.Equal(t, "command is required", err.Error())
}

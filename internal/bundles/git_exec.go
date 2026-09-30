package bundles

import (
	"context"
	"os"
	"os/exec"
)

// gitIdentityEnv pins the committer identity: omahabd and the apply unit run
// as root with no git config, so commits to the bundles repo must not depend
// on $HOME or global settings.
var gitIdentityEnv = []string{
	"GIT_AUTHOR_NAME=omahab",
	"GIT_AUTHOR_EMAIL=omahab@localhost",
	"GIT_COMMITTER_NAME=omahab",
	"GIT_COMMITTER_EMAIL=omahab@localhost",
}

func gitCommand(ctx context.Context, repo string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), gitIdentityEnv...)
	return cmd
}

// gitExecOutput runs git -C repo with args and returns stdout.
func gitExecOutput(ctx context.Context, repo string, args []string) ([]byte, error) {
	return gitCommand(ctx, repo, args).Output()
}

// gitExecRun runs git -C repo with args, discarding stdout.
func gitExecRun(ctx context.Context, repo string, args ...string) error {
	return gitCommand(ctx, repo, args).Run()
}

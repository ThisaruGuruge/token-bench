package intscenario

import (
	"context"
	"os/exec"
)

func runDocker(ctx context.Context, arguments ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
	return string(output), err
}

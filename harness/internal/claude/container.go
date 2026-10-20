package claude

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Container runs Claude Code in a fresh container per session instead of on
// the host. The image's entrypoint reads the API key from the first line of
// stdin into a file on a tmpfs, then execs claude with the remaining stdin,
// so the key never appears in the container's configuration, an image layer,
// a command line or the environment of the commands the agent runs.
type Container struct {
	Image string
	// Platform (os/arch) is passed as --platform when set.
	Platform string
	// Mounts are host:container bind mounts; the working directory is mounted
	// at WorkDir in addition.
	Mounts []string
	// Env entries (KEY=value) are set in the container. Never pass a secret here.
	Env []string
	// WorkDir is the agent's working directory inside the container.
	WorkDir string
	// Network is a docker network to join instead of the default bridge.
	Network string
	// PluginDir is a plugin directory inside the image, passed as --plugin-dir.
	PluginDir string
}

const (
	// ContainerKeyHelper is the apiKeyHelper inside the container.
	ContainerKeyHelper = "cat /run/bench-secret/key"
	containerSecretDir = "/run/bench-secret"
	containerUserID    = "1000"
)

// containerEnvironment is always set in the container: the harness' own
// setting and no update checks or nonessential traffic during a run.
var containerEnvironment = []string{
	"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1",
}

func newContainerName() string {
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	return "token-bench-" + hex.EncodeToString(suffix)
}

// dockerArguments are the `docker run` arguments before the claude arguments.
func (c Container) dockerArguments(name, hostWorkDir string) []string {
	arguments := []string{
		"run", "--rm", "-i", "--init", "--name", name,
		"--tmpfs", containerSecretDir + ":rw,noexec,nosuid,size=64k,mode=0700,uid=" + containerUserID + ",gid=" + containerUserID,
		"-v", hostWorkDir + ":" + c.WorkDir,
		"-w", c.WorkDir,
	}
	if c.Platform != "" {
		arguments = append(arguments, "--platform", c.Platform)
	}
	if c.Network != "" {
		arguments = append(arguments, "--network", c.Network)
	}
	for _, mount := range c.Mounts {
		arguments = append(arguments, "-v", mount)
	}
	for _, entry := range append(append([]string(nil), containerEnvironment...), c.Env...) {
		arguments = append(arguments, "-e", entry)
	}
	return append(arguments, c.Image)
}

// readAPIKey runs the host helper and returns its trimmed output. The output
// is the secret: callers must only write it to the container's stdin.
func readAPIKey(helper string) ([]byte, error) {
	command := exec.Command("sh", "-c", helper)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	key := []byte(strings.TrimSpace(string(output)))
	clear(output)
	if err != nil || len(key) == 0 || strings.ContainsAny(string(key), "\r\n") {
		clear(key)
		return nil, fmt.Errorf("API key helper failed or printed no single-line key: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return key, nil
}

// sendKey writes the key line the entrypoint reads, then wipes the copy.
func sendKey(stdin io.Writer, key []byte) error {
	line := append(key, '\n')
	_, err := stdin.Write(line)
	clear(line)
	if err != nil {
		return errors.New("send the API key to the container: " + err.Error())
	}
	return nil
}

// removeContainer force-removes a container that outlived its docker client.
func removeContainer(name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

package arm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// RunEnv is the per-run environment of an arm: a PATH shim directory (arms
// without discover) and a private XDG cache so each run starts with a cold
// discover cache.
type RunEnv struct {
	Dir      string
	ShimDir  string
	CacheDir string
	// Env holds KEY=value entries to append to the agent process' environment.
	Env []string
}

// PrepareRun creates the per-run directory. realBal is the absolute path of the
// real `bal`; it is only used when the arm hides discover. basePath is the PATH
// the shim directory is prepended to.
func PrepareRun(resolved Resolved, realBal, basePath string) (*RunEnv, error) {
	dir, err := os.MkdirTemp("", "token-bench-arm-*")
	if err != nil {
		return nil, fmt.Errorf("create arm run directory: %w", err)
	}
	run := &RunEnv{Dir: dir, CacheDir: filepath.Join(dir, "xdg-cache")}
	if err := os.Mkdir(run.CacheDir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	run.Env = append(run.Env, "XDG_CACHE_HOME="+run.CacheDir)
	if !resolved.DiscoverVisible {
		run.ShimDir = filepath.Join(dir, "bin")
		if err := WriteBalShim(run.ShimDir, realBal); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		run.Env = append(run.Env, "PATH="+run.ShimDir+string(os.PathListSeparator)+basePath)
	}
	keys := make([]string, 0, len(resolved.Env))
	for key := range resolved.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		run.Env = append(run.Env, key+"="+resolved.Env[key])
	}
	return run, nil
}

// Cleanup removes the per-run directory.
func (r *RunEnv) Cleanup() error {
	if r == nil || r.Dir == "" {
		return nil
	}
	return os.RemoveAll(r.Dir)
}

// FindRealBal locates `bal` on PATH, ignoring directories listed in skip.
func FindRealBal(path string, skip ...string) (string, error) {
	for _, directory := range filepath.SplitList(path) {
		if directory == "" || contains(skip, directory) {
			continue
		}
		candidate := filepath.Join(directory, "bal")
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("bal not found on PATH")
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// UnknownCommandMessage is what the real `bal` prints on stderr, and exits 1
// with, for a subcommand it does not know. Verified against Ballerina
// 2201.13.6: `bal foobar`.
const UnknownCommandMessage = "ballerina: unknown command '%s'\nRun 'bal help' for usage."

// WriteBalShim writes dir/bal, a wrapper that forwards to realBal except for
// the discover subcommand (`bal discover ...`, `bal help discover`), which
// fails like an unknown command, and `bal tool list`, whose discover row is
// dropped so the tool cannot be learnt from there.
func WriteBalShim(dir, realBal string) error {
	if !filepath.IsAbs(realBal) {
		return fmt.Errorf("real bal path %q must be absolute", realBal)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf(`#!/bin/sh
REAL_BAL=%s
unknown() {
  printf "ballerina: unknown command '%%s'\n" discover >&2
  printf "Run 'bal help' for usage.\n" >&2
  exit 1
}
if [ "$1" = "discover" ]; then
  unknown
fi
if [ "$1" = "help" ] && [ "$2" = "discover" ]; then
  unknown
fi
if [ "$1" = "tool" ] && [ "$2" = "list" ]; then
  output=$("$REAL_BAL" "$@")
  status=$?
  printf '%%s\n' "$output" | grep -v '^|discover[ |]'
  exit $status
fi
exec "$REAL_BAL" "$@"
`, shellQuote(realBal))
	return os.WriteFile(filepath.Join(dir, "bal"), []byte(script), 0o755)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// Runner executes a command and returns its trimmed combined output.
type Runner func(name string, args ...string) (string, error)

// ExecRunner runs real commands.
func ExecRunner(name string, args ...string) (string, error) {
	output, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// Versions are the tool versions seen by a run.
type Versions struct {
	Bal      string
	Discover string
	Claude   string
}

// CaptureVersions records `bal version`, the discover row of `bal tool list`
// (real bal, not the shim) and `claude --version`. Failures are recorded as
// text rather than returned: a missing tool must not abort a run.
func CaptureVersions(run Runner, realBal, claude string) Versions {
	versions := Versions{Bal: "unavailable", Discover: "not installed", Claude: "unavailable"}
	if output, err := run(realBal, "version"); err == nil && output != "" {
		versions.Bal = strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
	}
	if output, err := run(realBal, "tool", "list"); err == nil {
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Split(line, "|")
			if len(fields) >= 3 && strings.TrimSpace(fields[1]) == "discover" {
				versions.Discover = strings.TrimSpace(fields[2])
			}
		}
	}
	if claude != "" {
		if output, err := run(claude, "--version"); err == nil && output != "" {
			versions.Claude = output
		}
	}
	return versions
}

package arm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunEnv is the per-run environment of an arm: a PATH shim directory (arms
// without discover) and a private XDG cache so each run starts with a cold
// discover cache.
type RunEnv struct {
	Dir      string
	ShimDir  string
	CacheDir string
	// Env holds KEY=value entries to append to the agent process' environment
	// (for a container arm: the container's environment, in container paths).
	Env []string
	// CallLogDir holds the bal-calllog log and saved outputs (container arms).
	CallLogDir string
	// SeedFiles counts the cache seed files copied in.
	SeedFiles int
	// Container is set for a container arm: its image and Dir's mount point.
	Container *ContainerRun
}

// ContainerRun is the per-run side of a container arm.
type ContainerRun struct {
	Image    string
	Platform string
	// BenchDir is mounted at BenchMount; it holds the cache and the call log.
	BenchDir string
}

// Paths inside a container arm's container.
const (
	BenchMount     = "/bench"
	WorkMount      = "/work"
	CallLogName    = "calls.jsonl"
	callLogSubdir  = "calls"
	cacheSubdir    = "xdg-cache"
	discoverSubdir = "bal-discover"
)

// PrepareRun creates the per-run directory. realBal is the absolute path of the
// real `bal`; it is only used when the arm hides discover. basePath is the PATH
// the shim directory is prepended to.
func PrepareRun(resolved Resolved, realBal, basePath string) (*RunEnv, error) {
	return PrepareRunIn(resolved, realBal, basePath, "")
}

// PrepareRunIn is PrepareRun with the per-run directory created under
// parent (the system temporary directory when empty). A container arm needs
// a parent the container runtime can mount.
func PrepareRunIn(resolved Resolved, realBal, basePath, parent string) (*RunEnv, error) {
	if resolved.Container != nil {
		return prepareContainerRun(resolved, parent)
	}
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

func prepareContainerRun(resolved Resolved, parent string) (*RunEnv, error) {
	if parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp(parent, "token-bench-arm-*")
	if err != nil {
		return nil, fmt.Errorf("create arm run directory: %w", err)
	}
	run := &RunEnv{Dir: dir, CacheDir: filepath.Join(dir, cacheSubdir), Container: &ContainerRun{Image: resolved.Container.Image, BenchDir: dir}}
	fail := func(err error) (*RunEnv, error) {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(run.CacheDir, discoverSubdir), 0o777); err != nil {
		return fail(err)
	}
	if resolved.CacheSeedDir != "" {
		if run.SeedFiles, err = CopySeed(resolved.CacheSeedDir, filepath.Join(run.CacheDir, discoverSubdir), time.Now()); err != nil {
			return fail(fmt.Errorf("copy cache seed: %w", err))
		}
	}
	run.Env = append(run.Env, "XDG_CACHE_HOME="+BenchMount+"/"+cacheSubdir)
	if resolved.CallLog {
		run.CallLogDir = filepath.Join(dir, callLogSubdir)
		if err := os.Mkdir(run.CallLogDir, 0o777); err != nil {
			return fail(err)
		}
		run.Env = append(run.Env, "TOKEN_BENCH_CALL_LOG="+BenchMount+"/"+callLogSubdir+"/"+CallLogName)
	}
	// The container user is not the host user; let it write the mounted tree.
	if err := makeWritable(dir); err != nil {
		return fail(err)
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

func makeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o666)
		if entry.IsDir() {
			mode = 0o777
		}
		return os.Chmod(path, mode)
	})
}

// LiveFetches counts the files the run added to its discover cache beyond the seed.
func (r *RunEnv) LiveFetches(seed string) (int, error) {
	cache := filepath.Join(r.CacheDir, discoverSubdir)
	if seed == "" {
		return NewCacheFiles(filepath.Join(r.Dir, "no-seed"), cache)
	}
	return NewCacheFiles(seed, cache)
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
// fails like an unknown command, and the outputs that list installed tools:
// `bal tool list` and the top-level help (`bal`, `bal help`, `bal --help`,
// `bal -h`, which show a "Tool Commands" section), whose discover row is
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
if [ $# -eq 0 ] || { [ $# -eq 1 ] && { [ "$1" = "help" ] || [ "$1" = "--help" ] || [ "$1" = "-h" ]; }; }; then
  output=$("$REAL_BAL" "$@")
  status=$?
  # Drop the discover row of "Tool Commands" and any wrapped description lines.
  printf '%%s\n' "$output" | awk '
    /^[ \t]+discover([ \t]|$)/ { skip = 1; next }
    skip && /^[ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t][ \t]/ { next }
    { skip = 0; print }'
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

// Image is what `docker image inspect` reports about a container arm's image.
type Image struct {
	ID string
	// Platform is the image's os/architecture, passed to docker run so a
	// DOCKER_DEFAULT_PLATFORM in the environment cannot pick another one.
	Platform string
	Labels   map[string]string
}

// InspectImage reads the image's id and its org.token-bench.* labels; a
// missing image is an error (build it first).
func InspectImage(run Runner, image string) (Image, error) {
	output, err := run("docker", "image", "inspect", "--format", "{{.Id}}\t{{.Os}}/{{.Architecture}}\t{{json .Config.Labels}}", image)
	if err != nil {
		return Image{}, fmt.Errorf("inspect image %s (build it with scripts/grammar/build-images.sh): %v: %s", image, err, output)
	}
	id, rest, _ := strings.Cut(output, "\t")
	platform, labelsJSON, _ := strings.Cut(rest, "\t")
	var labels map[string]string
	_ = json.Unmarshal([]byte(labelsJSON), &labels)
	kept := map[string]string{}
	for key, value := range labels {
		if strings.HasPrefix(key, "org.token-bench.") {
			kept[key] = value
		}
	}
	return Image{ID: strings.TrimSpace(id), Platform: strings.TrimSpace(platform), Labels: kept}, nil
}

// versionSeparator splits the three outputs; `bal tool list` draws its table
// with dashes, so it must not be one.
const versionSeparator = "@@token-bench-version@@"

// CaptureContainerVersions records the versions inside a container arm's
// image, the same three CaptureVersions reads on the host.
func CaptureContainerVersions(run Runner, image, platform string) Versions {
	versions := Versions{Bal: "unavailable", Discover: "not installed", Claude: "unavailable"}
	output, err := run("docker", "run", "--rm", "--platform", platform, "--entrypoint", "sh", image, "-c",
		"bal version 2>/dev/null | head -n 1; echo "+versionSeparator+"; bal tool list 2>/dev/null; echo "+versionSeparator+"; claude --version 2>/dev/null")
	if err != nil {
		return versions
	}
	parts := strings.SplitN(output, versionSeparator, 3)
	if len(parts) != 3 {
		return versions
	}
	if bal := strings.TrimSpace(parts[0]); bal != "" {
		versions.Bal = bal
	}
	for _, line := range strings.Split(parts[1], "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 3 && strings.TrimSpace(fields[1]) == "discover" {
			versions.Discover = strings.TrimSpace(fields[2])
		}
	}
	if claude := strings.TrimSpace(parts[2]); claude != "" {
		versions.Claude = claude
	}
	return versions
}

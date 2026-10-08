package arm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBal records its arguments and prints a marker, standing in for the real bal.
func fakeBal(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim is POSIX only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bal")
	script := `#!/bin/sh
if [ "$1" = "tool" ] && [ "$2" = "list" ]; then
  printf '|TOOL ID|VERSION|\n|----|----|\n|discover |0.1.0 [repo = local]|\n|openapi |2.4.2|\n'
  exit 3
fi
if [ $# -eq 0 ] || [ "$1" = "help" ] || [ "$1" = "--help" ] || [ "$1" = "-h" ]; then
  if [ -z "$2" ]; then
    printf '   Tool Commands:\n        asyncapi                  Generate sources.\n        discover                  Learn a library API\n                                  without leaving the terminal.\n        graphql                   Generates clients\n                                  for GraphQL.\n'
    exit 0
  fi
fi
printf 'real-bal %s\n' "$*"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runShim(t *testing.T, shim string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	command := exec.Command(shim, args...)
	var out, errOut strings.Builder
	command.Stdout, command.Stderr = &out, &errOut
	err := command.Run()
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exit = exitError.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), exit
}

func TestShimForwardsEverythingButDiscover(t *testing.T) {
	real := fakeBal(t)
	dir := t.TempDir()
	if err := WriteBalShim(dir, real); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "bal")
	stdout, _, exit := runShim(t, shim, "build", "--offline", "a b")
	if exit != 0 || stdout != "real-bal build --offline a b\n" {
		t.Fatalf("forwarding broken: %q exit %d", stdout, exit)
	}
	for _, args := range [][]string{{"discover"}, {"discover", "ballerinax/github", "client"}, {"help", "discover"}} {
		stdout, stderr, exit := runShim(t, shim, args...)
		want := "ballerina: unknown command 'discover'\nRun 'bal help' for usage.\n"
		if exit != 1 || stdout != "" || stderr != want {
			t.Errorf("bal %v: exit %d stdout %q stderr %q", args, exit, stdout, stderr)
		}
	}
	if stdout, _, _ := runShim(t, shim, "help", "build"); !strings.Contains(stdout, "real-bal help build") {
		t.Errorf("help for another command was not forwarded: %q", stdout)
	}
	if stdout, _, _ := runShim(t, shim, "search", "discover"); !strings.Contains(stdout, "real-bal search discover") {
		t.Errorf("a later argument named discover must not be blocked: %q", stdout)
	}
}

func TestShimMatchesTheRealUnknownCommandMessage(t *testing.T) {
	if got := strings.ReplaceAll(UnknownCommandMessage, "%s", "discover"); got != "ballerina: unknown command 'discover'\nRun 'bal help' for usage." {
		t.Fatalf("message constant drifted: %q", got)
	}
}

func TestShimHidesDiscoverFromToolListAndKeepsExitStatus(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBalShim(dir, fakeBal(t)); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit := runShim(t, filepath.Join(dir, "bal"), "tool", "list")
	if exit != 3 {
		t.Errorf("exit status not preserved: %d", exit)
	}
	if strings.Contains(stdout, "discover") || !strings.Contains(stdout, "openapi") {
		t.Errorf("unexpected tool list %q", stdout)
	}
}

func TestShimHidesDiscoverFromTopLevelHelp(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBalShim(dir, fakeBal(t)); err != nil {
		t.Fatal(err)
	}
	want := "   Tool Commands:\n        asyncapi                  Generate sources.\n        graphql                   Generates clients\n                                  for GraphQL.\n"
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		stdout, _, exit := runShim(t, filepath.Join(dir, "bal"), args...)
		if exit != 0 || stdout != want {
			t.Errorf("bal %v: exit %d, output %q; want %q", args, exit, stdout, want)
		}
	}
	if stdout, _, _ := runShim(t, filepath.Join(dir, "bal"), "help", "build"); !strings.Contains(stdout, "real-bal help build") {
		t.Errorf("help for a command must be forwarded untouched: %q", stdout)
	}
}

func TestShimQuotesTheRealBalPath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "it's here")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "bal")
	if err := os.WriteFile(real, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := WriteBalShim(dir, real); err != nil {
		t.Fatal(err)
	}
	if stdout, _, exit := runShim(t, filepath.Join(dir, "bal"), "x"); stdout != "ok\n" || exit != 0 {
		t.Fatalf("got %q exit %d", stdout, exit)
	}
	if err := WriteBalShim(dir, "relative/bal"); err == nil {
		t.Error("relative bal path accepted")
	}
}

func TestPrepareRunBuildsEnvironmentPerArm(t *testing.T) {
	real := fakeBal(t)
	hidden, err := PrepareRun(Resolved{Config: Config{ID: "A", Env: map[string]string{"B": "2", "A": "1"}}}, real, "/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	defer hidden.Cleanup()
	wantPath := "PATH=" + hidden.ShimDir + string(os.PathListSeparator) + "/usr/bin"
	got := strings.Join(hidden.Env, "\n")
	for _, want := range []string{"XDG_CACHE_HOME=" + hidden.CacheDir, wantPath, "A=1\nB=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("env missing %q:\n%s", want, got)
		}
	}
	visible, err := PrepareRun(Resolved{Config: Config{ID: "C", DiscoverVisible: true}}, real, "/usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	defer visible.Cleanup()
	if visible.ShimDir != "" || strings.Contains(strings.Join(visible.Env, "\n"), "PATH=") {
		t.Errorf("discover-visible arm must not get a shim: %+v", visible)
	}
	if visible.CacheDir == hidden.CacheDir {
		t.Error("cache directories must be per run")
	}
	if err := hidden.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hidden.Dir); !os.IsNotExist(err) {
		t.Error("cleanup left the directory behind")
	}
}

func TestFindRealBalSkipsTheShimDirectory(t *testing.T) {
	real := fakeBal(t)
	shimDir := t.TempDir()
	if err := WriteBalShim(shimDir, real); err != nil {
		t.Fatal(err)
	}
	path := shimDir + string(os.PathListSeparator) + filepath.Dir(real)
	found, err := FindRealBal(path, shimDir)
	if err != nil || found != real {
		t.Fatalf("found %q, %v", found, err)
	}
	if _, err := FindRealBal(shimDir, shimDir); err == nil {
		t.Error("expected an error when only the shim exists")
	}
}

func TestCaptureVersions(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "version":
			return "Ballerina 2201.13.6 (Swan Lake Update 13)\nLanguage specification 2024R1", nil
		case "tool list":
			return "|TOOL ID |VERSION |\n|discover |0.1.0 [repo = local] |", nil
		case "--version":
			return "2.1.291 (Claude Code)", nil
		}
		return "", errors.New("unexpected")
	}
	versions := CaptureVersions(run, "bal", "claude")
	if versions.Bal != "Ballerina 2201.13.6 (Swan Lake Update 13)" || versions.Discover != "0.1.0 [repo = local]" || versions.Claude != "2.1.291 (Claude Code)" {
		t.Fatalf("unexpected %+v", versions)
	}
	failing := CaptureVersions(func(string, ...string) (string, error) { return "", errors.New("boom") }, "bal", "claude")
	if failing.Bal != "unavailable" || failing.Discover != "not installed" || failing.Claude != "unavailable" {
		t.Fatalf("unexpected %+v", failing)
	}
}

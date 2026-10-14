package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"token-bench/calllog"
)

// TestWrapperForwardsAndLogs builds the wrapper and runs it over a fake bal.
func TestWrapperForwardsAndLogs(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "bal")
	if output, err := exec.Command("go", "build", "-o", wrapper, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	fake := filepath.Join(dir, "real-bal")
	script := "#!/bin/sh\nprintf 'out:%s\\n' \"$*\"\nprintf 'err\\n' >&2\nread line\nprintf 'in:%s\\n' \"$line\"\nexit 3\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls", "calls.jsonl")
	for index := 0; index < 2; index++ {
		command := exec.Command(wrapper, "discover", "a b", "--output", "json")
		command.Env = append(os.Environ(), "TOKEN_BENCH_REAL_BAL="+fake, "TOKEN_BENCH_CALL_LOG="+logPath)
		command.Stdin = strings.NewReader("hello\n")
		var stdout, stderr strings.Builder
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 3 {
			t.Fatalf("exit: %v", err)
		}
		if stdout.String() != "out:discover a b --output json\nin:hello\n" || stderr.String() != "err\n" {
			t.Fatalf("passthrough: %q %q", stdout.String(), stderr.String())
		}
	}
	calls, err := calllog.Read(logPath)
	if err != nil || len(calls) != 2 {
		t.Fatalf("log: %v %d", err, len(calls))
	}
	call := calls[1]
	if call.Seq != 2 || call.ExitCode != 3 || call.Argv[1] != "a b" || call.StderrBytes != 4 || call.OutputSHA256 == "" {
		t.Fatalf("call %+v", call)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "calls", call.StdoutFile))
	if err != nil || len(saved) != call.StdoutBytes {
		t.Fatalf("saved output: %v %d", err, len(saved))
	}
	// Without a log path the wrapper only forwards.
	command := exec.Command(wrapper, "version")
	command.Env = append(os.Environ(), "TOKEN_BENCH_REAL_BAL="+fake, "TOKEN_BENCH_CALL_LOG=")
	command.Stdin = strings.NewReader("\n")
	_ = command.Run()
	if again, _ := calllog.Read(logPath); len(again) != 2 {
		t.Fatal("logged without a log path")
	}
}

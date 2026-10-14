// Command bal-calllog stands in for `bal` on an agent's PATH. It runs the real
// bal with the same arguments and stdin, writes its stdout and stderr through
// unchanged with the same exit code, and appends one JSON line per call to
// $TOKEN_BENCH_CALL_LOG, saving the raw outputs beside it.
//
// Output is buffered to be counted, so interleaved stdout and stderr reach the
// caller stdout first. The agent's Bash tool is not a TTY, so this changes
// nothing the tool decides from the terminal.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"token-bench/calllog"
)

const (
	defaultRealBal = "/usr/lib/ballerina/bin/bal"
	outputsDirName = "outputs"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	realBal := os.Getenv("TOKEN_BENCH_REAL_BAL")
	if realBal == "" {
		realBal = defaultRealBal
	}
	var stdout, stderr bytes.Buffer
	command := exec.Command(realBal, arguments...)
	command.Stdin = os.Stdin
	command.Stdout = &stdout
	command.Stderr = &stderr
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	started := time.Now()
	exitCode := 0
	if err := command.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "bal-calllog: cannot run %s: %v\n", realBal, err)
		return 127
	}
	go func() {
		for received := range signals {
			_ = command.Process.Signal(received)
		}
	}()
	if err := command.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			fmt.Fprintf(os.Stderr, "bal-calllog: %v\n", err)
			return 127
		}
		exitCode = exitErr.ExitCode()
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			exitCode = 128 + int(status.Signal())
		}
	}
	duration := time.Since(started)
	signal.Stop(signals)
	_, _ = os.Stdout.Write(stdout.Bytes())
	_, _ = os.Stderr.Write(stderr.Bytes())
	if logPath := os.Getenv("TOKEN_BENCH_CALL_LOG"); logPath != "" {
		if err := record(logPath, arguments, exitCode, stdout.Bytes(), stderr.Bytes(), started, duration); err != nil {
			fmt.Fprintf(os.Stderr, "bal-calllog: cannot log the call: %v\n", err)
		}
	}
	return exitCode
}

// record appends the call under an exclusive lock, so concurrent calls get
// distinct sequence numbers and whole lines.
func record(logPath string, arguments []string, exitCode int, stdout, stderr []byte, started time.Time, duration time.Duration) error {
	logDir := filepath.Dir(logPath)
	outputs := filepath.Join(logDir, outputsDirName)
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	if err := syscall.Flock(int(log.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(log.Fd()), syscall.LOCK_UN)
	existing, err := calllog.Read(logPath)
	if err != nil {
		return err
	}
	sequence := len(existing) + 1
	base := fmt.Sprintf("%04d", sequence)
	call := calllog.Call{
		Seq:         sequence,
		Argv:        arguments,
		ExitCode:    exitCode,
		StdoutBytes: len(stdout),
		StderrBytes: len(stderr),
		StartedAt:   started.UTC().Format(time.RFC3339Nano),
		DurationMs:  duration.Milliseconds(),
		StdoutFile:  filepath.Join(outputsDirName, base+".stdout"),
		StderrFile:  filepath.Join(outputsDirName, base+".stderr"),
	}
	hash := sha256.New()
	hash.Write(stdout)
	hash.Write(stderr)
	call.OutputSHA256 = hex.EncodeToString(hash.Sum(nil))
	if err := os.WriteFile(filepath.Join(logDir, call.StdoutFile), stdout, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(logDir, call.StderrFile), stderr, 0o644); err != nil {
		return err
	}
	line, err := json.Marshal(call)
	if err != nil {
		return err
	}
	_, err = log.Write(append(line, '\n'))
	return err
}

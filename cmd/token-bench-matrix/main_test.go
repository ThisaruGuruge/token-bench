package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func armsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../arms")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func invoke(t *testing.T, arguments ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"--arms-dir", armsDir(t)}, arguments...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestPhaseZeroDryRun(t *testing.T) {
	target := t.TempDir()
	code, out, errText := invoke(t, "--phase=phase0", "--dry-run", "--target", target)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errText)
	}
	for _, want := range []string{"2 arms x 1 scenarios x 3 runs = 6 runs", "Arms:      A, D", "Scenarios: checkout-sync", "Model: sonnet",
		"Runs to execute: 6 of 6", "Worst-case spend: 6 x $20.00 = $120.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("a dry run must not write to the target: %v", entries)
	}
}

func TestExplicitFlagsEquivalentToPhaseZero(t *testing.T) {
	code, out, errText := invoke(t, "--arms", "A,D", "--scenarios", "checkout-sync", "--runs", "3", "--dry-run", "--target", t.TempDir())
	if code != 0 || !strings.Contains(out, "= 6 runs") || !strings.Contains(out, "Per-run budget cap: $20.00") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errText)
	}
}

func TestFlagsOverridePhaseAndBudgetScalesWorstCase(t *testing.T) {
	code, out, _ := invoke(t, "--phase=phase0", "--arms=A", "--runs=2", "--model=opus", "--max-budget-usd=5", "--dry-run", "--target", t.TempDir())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"1 arms x 1 scenarios x 2 runs = 2 runs", "Model: opus", "Worst-case spend: 2 x $5.00 = $10.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestInvalidInputsAreRejected(t *testing.T) {
	for name, arguments := range map[string][]string{
		"unknown arm":      {"--arms=Z", "--scenarios=checkout-sync", "--dry-run"},
		"unknown scenario": {"--arms=A", "--scenarios=nope", "--dry-run"},
		"unknown phase":    {"--phase=phase9", "--dry-run"},
		"nothing chosen":   {"--dry-run"},
	} {
		if code, _, _ := invoke(t, arguments...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestListShowsArmsAndPhases(t *testing.T) {
	code, out, _ := invoke(t, "--list")
	if code != 0 || !strings.Contains(out, "A  bare agent") || !strings.Contains(out, "phase0") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestRunsCellsThroughTheRunnerAndResumes(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	runner := filepath.Join(dir, "fake-token-bench")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\nfor a in \"$@\"; do case \"$a\" in --target=*) t=${a#--target=};; esac; done\nn=1; for a in \"$@\"; do case \"$a\" in --run-number=*) n=${a#--run-number=};; esac; done\nmkdir -p \"$t/run\" && printf '{\"schemaVersion\":5,\"task\":\"checkout-sync\",\"language\":\"ballerina\",\"harness\":\"claude\",\"model\":\"sonnet\",\"thinkingBudget\":\"high\",\"run\":%s,\"requestedRuns\":9}' \"$n\" > \"$t/run/result.json\"\n"
	if err := os.WriteFile(runner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "results")
	arguments := []string{"--arms=A,D", "--scenarios=checkout-sync", "--runs=2", "--runner", runner, "--target", target, "--seed", "5"}
	if code, out, errText := invoke(t, arguments...); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errText)
	}
	content, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 runner calls, got %d:\n%s", len(lines), content)
	}
	for _, want := range []string{"checkout-sync ballerina claude --n-runs=1", "--arm=A", "--arm=D", "--model=sonnet", "--max-budget-usd=20", "--requested-runs=2"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("runner args lack %q:\n%s", want, content)
		}
	}
	if code, out, _ := invoke(t, arguments...); code != 0 || !strings.Contains(out, "Ran 0, skipped 4") {
		t.Fatalf("second invocation must skip everything: exit %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(log); len(after) != len(content) {
		t.Error("runner was called again for completed cells")
	}
	if code, _, errText := invoke(t, append(arguments[:len(arguments)-2], "--model=opus")...); code != 2 || !strings.Contains(errText, "use another --target") {
		t.Errorf("changing the model in an existing target must be refused: exit %d %s", code, errText)
	}
}

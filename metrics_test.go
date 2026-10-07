package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"token-bench/harness"
	"token-bench/task"
	"token-bench/tracing"
)

type fakeHarness struct {
	model     string
	directory string
	stream    string
	reported  *float64
	delay     time.Duration
}

func (f *fakeHarness) Model() string          { return f.model }
func (f *fakeHarness) ThinkingBudget() string { return "high" }
func (f *fakeHarness) Start(directory string, _ tracing.BenchmarkSpan) error {
	f.directory = directory
	return nil
}
func (f *fakeHarness) SendMessage(string) (string, error) {
	time.Sleep(f.delay)
	return "done", nil
}
func (f *fakeHarness) TokenCount() (harness.TokenCount, error) {
	return harness.TokenCount{Input: 1_000_000, Output: 100_000, Total: 1_100_000}, nil
}
func (f *fakeHarness) ReportedCostUSD() (float64, bool) {
	if f.reported == nil {
		return 0, false
	}
	return *f.reported, true
}
func (f *fakeHarness) End() error {
	if f.stream == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(f.directory, harness.ClaudeStreamArtifact), []byte(f.stream), 0o600)
}

type fakeHandle struct{}

func (fakeHandle) Setup() (func() error, error) { return func() error { return nil }, nil }
func (fakeHandle) Prompt() (string, error)      { return "requirements", nil }
func (fakeHandle) Feedback() (string, error)    { return "", nil }
func (fakeHandle) Validation() (bool, error)    { return true, nil }

type fakeLanguage struct{}

func (fakeLanguage) Name() string              { return "go" }
func (fakeLanguage) InitializeCommand() string { return "go mod init x" }
func (fakeLanguage) TaskPromptSuffix() string  { return "" }
func (fakeLanguage) StartProject(string, int) (func() error, error) {
	return func() error { return nil }, nil
}

func runFake(t *testing.T, agent *fakeHarness, arm string) runResult {
	t.Helper()
	definition := task.Task{
		Name:      "fake",
		Resources: task.Resources{NPorts: 1},
		InitFn:    func(task.AllocResources) task.TaskHandle { return fakeHandle{} },
	}
	current := runResult{Task: "fake", Language: "go", Harness: "claude", Arm: arm, Run: 1, RequestedRuns: 1}
	return executeRun(current, t.TempDir(), definition, fakeLanguage{}, func() harness.Harness { return agent }, nil)
}

func TestExecuteRunRecordsDurationCostAndToolUsage(t *testing.T) {
	reported := 3.5
	agent := &fakeHarness{
		model: "claude-sonnet-5-5", reported: &reported, delay: 50 * time.Millisecond,
		stream: `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"bal discover ballerinax/x"}}]}}` + "\n",
	}
	result := runFake(t, agent, "D")
	if result.Error != "" || !result.Passed {
		t.Fatalf("unexpected outcome: passed=%v error=%q", result.Passed, result.Error)
	}
	if result.Arm != "D" {
		t.Errorf("arm = %q", result.Arm)
	}
	if result.DurationSeconds < 0.05 || result.DurationSeconds > 30 {
		t.Errorf("duration = %v", result.DurationSeconds)
	}
	// 1M input * $2 + 0.1M output * $10
	if result.CostUSD == nil || math.Abs(*result.CostUSD-3.0) > 1e-9 {
		t.Errorf("estimated cost = %v, want 3.0", result.CostUSD)
	}
	if result.ReportedCostUSD == nil || *result.ReportedCostUSD != 3.5 {
		t.Errorf("reported cost = %v, want 3.5", result.ReportedCostUSD)
	}
	if result.ToolUsage == nil || result.ToolUsage.Families["bal discover"] != 1 || result.ToolUsage.AssistantTurns != 1 {
		t.Errorf("tool usage = %+v", result.ToolUsage)
	}
}

func TestExecuteRunWithoutStreamLogOrKnownPrice(t *testing.T) {
	result := runFake(t, &fakeHarness{model: "some-unpriced-model"}, "")
	if result.Error != "" {
		t.Fatalf("unexpected error %q", result.Error)
	}
	if result.CostUSD != nil || result.ReportedCostUSD != nil || result.ToolUsage != nil {
		t.Fatalf("expected cost and tool usage to be unset: %+v", result)
	}
	if result.DurationSeconds <= 0 {
		t.Errorf("duration = %v", result.DurationSeconds)
	}
}

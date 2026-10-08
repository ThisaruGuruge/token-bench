package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"token-bench/harness"
	benchresult "token-bench/result"
	"token-bench/streamlog"
	"token-bench/task"
	"token-bench/tracing"
)

type fakeHarness struct {
	model     string
	directory string
	stream    string
	reported  *float64
	delay     time.Duration
	sendErr   error
}

func (f *fakeHarness) Model() string          { return f.model }
func (f *fakeHarness) ThinkingBudget() string { return "high" }
func (f *fakeHarness) Start(directory string, _ tracing.BenchmarkSpan) error {
	f.directory = directory
	return nil
}
func (f *fakeHarness) SendMessage(string) (string, error) {
	time.Sleep(f.delay)
	return "done", f.sendErr
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

func TestEstimatedCostPrefersStreamUsageWithCacheTTL(t *testing.T) {
	result := runResult{
		Model:  "sonnet",
		Tokens: benchresult.TokenCount{Input: 1_000_000, CacheWrite: 1_000_000},
		ToolUsage: &streamlog.Summary{Models: map[string]streamlog.ModelUsage{
			"claude-sonnet-5-5": {Input: 1_000_000, CacheWrite1h: 500_000, CacheWriteUnsplit: 500_000},
		}},
	}
	applyEstimatedCost(&result)
	// $2 input + 1M 1-hour writes at $4 (unsplit writes priced at 1 hour).
	if result.CostUSD == nil || math.Abs(*result.CostUSD-6) > 1e-9 || result.CostEstimateBasis != benchresult.CostBasisStreamUsage {
		t.Fatalf("cost = %v (%s), want 6 (stream-usage)", result.CostUSD, result.CostEstimateBasis)
	}

	// An unpriced model in the stream falls back to the token totals.
	result.ToolUsage.Models["mystery-model"] = streamlog.ModelUsage{Input: 1}
	applyEstimatedCost(&result)
	// $2 input + 1M 5-minute writes at $2.50.
	if math.Abs(*result.CostUSD-4.5) > 1e-9 || result.CostEstimateBasis != benchresult.CostBasisTokenTotals {
		t.Fatalf("fallback cost = %v (%s), want 4.5 (token-totals)", *result.CostUSD, result.CostEstimateBasis)
	}
}

type failingSetupHandle struct{ fakeHandle }

func (failingSetupHandle) Setup() (func() error, error) {
	return nil, errors.New("start MySQL container: docker daemon not running")
}

func TestExecuteRunClassifiesFailures(t *testing.T) {
	definition := task.Task{
		Name:      "fake",
		Resources: task.Resources{NPorts: 1},
		InitFn:    func(task.AllocResources) task.TaskHandle { return failingSetupHandle{} },
	}
	current := runResult{Task: "fake", Language: "go", Harness: "claude", Run: 1, RequestedRuns: 1}
	setup := executeRun(current, t.TempDir(), definition, fakeLanguage{}, func() harness.Harness { return &fakeHarness{model: "sonnet"} }, nil)
	if !setup.InfraFailure || setup.Passed || !strings.Contains(setup.Error, "docker daemon") {
		t.Errorf("setup failure: %+v", setup)
	}

	api := runFake(t, &fakeHarness{model: "sonnet", sendErr: &harness.InfraError{Err: errors.New("Claude API error (HTTP 529)")}}, "A")
	if !api.InfraFailure || api.StopReason != "" {
		t.Errorf("API failure: infra=%v stop=%q", api.InfraFailure, api.StopReason)
	}

	budget := runFake(t, &fakeHarness{model: "sonnet", sendErr: &harness.StopError{Reason: "error_max_budget_usd"}}, "A")
	if budget.InfraFailure || budget.StopReason != "error_max_budget_usd" || budget.Passed {
		t.Errorf("budget stop: infra=%v stop=%q passed=%v", budget.InfraFailure, budget.StopReason, budget.Passed)
	}
}

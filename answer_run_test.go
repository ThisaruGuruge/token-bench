package main

import (
	"os"
	"path/filepath"
	"testing"

	"token-bench/harness"
	"token-bench/task"
	"token-bench/tracing"
)

// answerAgent writes a fixed answer when it receives the prompt.
type answerAgent struct {
	directory string
	answer    string
	sendErr   error
	prompts   []string
}

func (a *answerAgent) Model() string          { return "fake" }
func (a *answerAgent) ThinkingBudget() string { return "high" }
func (a *answerAgent) Start(directory string, _ tracing.BenchmarkSpan) error {
	a.directory = directory
	return nil
}
func (a *answerAgent) SendMessage(message string) (string, error) {
	a.prompts = append(a.prompts, message)
	if a.answer != "" {
		if err := os.WriteFile(filepath.Join(a.directory, "answer.json"), []byte(a.answer), 0o644); err != nil {
			return "", err
		}
	}
	return "done", a.sendErr
}
func (a *answerAgent) TokenCount() (harness.TokenCount, error) { return harness.TokenCount{}, nil }
func (a *answerAgent) ReportedCostUSD() (float64, bool)        { return 0, true }
func (a *answerAgent) End() error                              { return nil }

func runAnswer(t *testing.T, agent *answerAgent) runResult {
	t.Helper()
	definition, ok := task.ByName("dq-D01")
	if !ok {
		t.Fatal("dq-D01 not registered")
	}
	return executeRun(runResult{Task: definition.Name}, t.TempDir(), definition, nil,
		func() harness.Harness { return agent }, nil)
}

func TestAnswerRunIsSingleShotAndGraded(t *testing.T) {
	agent := &answerAgent{answer: `{"operations":["send","close","flush","getTopicPartitions","sendWithMetadata"]}`}
	result := runAnswer(t, agent)
	if !result.Passed || result.Answer == nil || result.Answer.Correct != 1 || result.Error != "" || result.InfraFailure {
		t.Fatalf("result %+v answer %+v", result, result.Answer)
	}
	if len(agent.prompts) != 1 || agent.prompts[0] == "" {
		t.Fatalf("prompts %q", agent.prompts)
	}
}

func TestAnswerRunWithoutAnAnswerFails(t *testing.T) {
	result := runAnswer(t, &answerAgent{})
	if result.Passed || result.Answer == nil || result.Answer.Present || result.Error != hiddenValidationFailure {
		t.Fatalf("result %+v", result)
	}
}

func TestAnswerRunInfraErrorIsNotGraded(t *testing.T) {
	result := runAnswer(t, &answerAgent{sendErr: &harness.InfraError{Err: os.ErrDeadlineExceeded}})
	if !result.InfraFailure || result.Answer != nil {
		t.Fatalf("result %+v", result)
	}
}

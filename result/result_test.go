package result

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"token-bench/streamlog"
)

func TestWriteRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	want := Result{
		SchemaVersion:  SchemaVersion,
		Task:           "router",
		Language:       "go",
		Harness:        "pi",
		Model:          "gpt-5.6-sol",
		ThinkingBudget: "high",
		SkillsDir:      "/tmp/benchmark-skills",
		PromptTemplate: "benchmark prompt template",
		Run:            1,
		RequestedRuns:  2,
		Passed:         true,
		Tokens:         TokenCount{Input: 10, Output: 20, Total: 30},
	}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadSupportsLegacyResultsWithoutModelMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	content := `{"schemaVersion":2,"task":"router","language":"go","harness":"pi","run":1,"requestedRuns":1,"tokens":{}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "unknown-model" || got.ThinkingBudget != "unknown-thinking" {
		t.Fatalf("unexpected legacy metadata: model=%q thinkingBudget=%q", got.Model, got.ThinkingBudget)
	}
}

func TestValidateRequiresModelMetadataForCurrentSchema(t *testing.T) {
	value := Result{SchemaVersion: SchemaVersion, Task: "router", Language: "go", Harness: "pi", Run: 1, RequestedRuns: 1}
	if err := value.Validate(); err == nil {
		t.Fatal("expected model metadata validation error")
	}
}

func TestValidateRejectsUnknownSchema(t *testing.T) {
	value := Result{SchemaVersion: 1, Task: "router", Language: "go", Harness: "pi", Run: 1, RequestedRuns: 1}
	if err := value.Validate(); err == nil {
		t.Fatal("expected schema validation error")
	}
}

func TestValidateRejectsRelativeSkillsDirectory(t *testing.T) {
	value := Result{
		SchemaVersion: SchemaVersion, Task: "router", Language: "go", Harness: "pi",
		Model: "gpt-5.6-sol", ThinkingBudget: "high", SkillsDir: "skills", Run: 1, RequestedRuns: 1,
	}
	if err := value.Validate(); err == nil {
		t.Fatal("expected skills directory validation error")
	}
}

func TestWriteReadNewMetricFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	estimated, reported := 1.25, 1.5
	want := Result{
		SchemaVersion: SchemaVersion, Task: "router", Language: "go", Harness: "claude", Arm: "D",
		Model: "sonnet", ThinkingBudget: "high", Run: 1, RequestedRuns: 1, Passed: true,
		DurationSeconds: 93.5, CostUSD: &estimated, ReportedCostUSD: &reported,
		ToolUsage: &streamlog.Summary{
			AssistantTurns: 3, ToolCalls: 2,
			Tools:    map[string]int{"Bash": 2},
			Families: map[string]int{"bal discover": 1},
		},
	}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadSupportsPreviousSchemaWithoutNewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	content := `{"schemaVersion":3,"task":"router","language":"go","harness":"claude","model":"opus","thinkingBudget":"high","run":1,"requestedRuns":1,"tokens":{}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Arm != "" || got.DurationSeconds != 0 || got.CostUSD != nil || got.ReportedCostUSD != nil || got.ToolUsage != nil {
		t.Fatalf("unexpected values for absent fields: %+v", got)
	}
}

func TestValidateRejectsNegativeDuration(t *testing.T) {
	value := Result{
		SchemaVersion: SchemaVersion, Task: "router", Language: "go", Harness: "pi",
		Model: "m", ThinkingBudget: "high", Run: 1, RequestedRuns: 1, DurationSeconds: -1,
	}
	if err := value.Validate(); err == nil {
		t.Fatal("expected duration validation error")
	}
}

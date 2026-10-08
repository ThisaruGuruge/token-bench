package report

import (
	"bytes"
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"token-bench/result"
	"token-bench/streamlog"
)

func fp(value float64) *float64 { return &value }

func writeArmRuns(t *testing.T, root string) {
	t.Helper()
	base := result.Result{SchemaVersion: result.SchemaVersion, Task: "checkout-sync", Language: "ballerina", Harness: "claude",
		Model: "sonnet", ThinkingBudget: "high", RequestedRuns: 3}
	runs := []result.Result{
		{Arm: "A", Run: 1, Passed: true, Tokens: result.TokenCount{Total: 1000}, CostUSD: fp(1), DurationSeconds: 100},
		{Arm: "A", Run: 2, Passed: false, Corrections: 3, Tokens: result.TokenCount{Total: 3000}, CostUSD: fp(3), DurationSeconds: 300, Error: "hidden validation failed"},
		{Arm: "A", Run: 3, Passed: true, Tokens: result.TokenCount{Total: 2000}, ReportedCostUSD: fp(2), DurationSeconds: 200},
		{Arm: "D", Run: 1, Passed: true, Corrections: 1, Tokens: result.TokenCount{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, Total: 10}, CostUSD: fp(0.5), DurationSeconds: 50,
			ToolUsage:   &streamlog.Summary{AssistantTurns: 4, ToolCalls: 5, Families: map[string]int{"bal discover": 3, "WebSearch": 1}},
			Environment: &result.Environment{DiscoverVisible: true, BalVersion: "Ballerina 2201.13.6", BalDiscover: "0.1.0 [repo = local]", MaxBudgetUSD: 20}},
	}
	for index, run := range runs {
		merged := base
		merged.Arm, merged.Run, merged.Passed, merged.Corrections = run.Arm, run.Run, run.Passed, run.Corrections
		merged.Tokens, merged.CostUSD, merged.ReportedCostUSD = run.Tokens, run.CostUSD, run.ReportedCostUSD
		merged.DurationSeconds, merged.Error, merged.ToolUsage, merged.Environment = run.DurationSeconds, run.Error, run.ToolUsage, run.Environment
		writeTestResult(t, filepath.Join(root, run.Arm, string(rune('a'+index)), "result.json"), merged)
	}
}

func parseCSV(t *testing.T, content string) []map[string]string {
	t.Helper()
	records, err := csv.NewReader(strings.NewReader(content)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]string
	for _, record := range records[1:] {
		row := map[string]string{}
		for index, column := range records[0] {
			row[column] = record[index]
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRunsCSVHasOneRowPerRunWithToolCounts(t *testing.T) {
	root := t.TempDir()
	writeArmRuns(t, root)
	var output bytes.Buffer
	if err := WriteRunsCSV([]string{root}, &output); err != nil {
		t.Fatal(err)
	}
	rows := parseCSV(t, output.String())
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	header := strings.SplitN(output.String(), "\n", 2)[0]
	if !strings.HasPrefix(header, "arm,scenario,language,run,requested_runs,model,passed,corrections,input_tokens") {
		t.Errorf("unexpected header %s", header)
	}
	var d map[string]string
	for _, row := range rows {
		if row["arm"] == "D" {
			d = row
		}
	}
	for column, want := range map[string]string{
		"scenario": "checkout-sync", "run": "1", "model": "sonnet", "passed": "true", "corrections": "1", "input_tokens": "1",
		"output_tokens": "2", "cache_read_tokens": "3", "cache_write_tokens": "4", "total_tokens": "10", "cost_usd": "0.5",
		"duration_seconds": "50.0", "assistant_turns": "4", "tool_calls": "5", "bal_discover": "3", "web_search": "1", "bal_search": "0",
		"discover_visible": "true", "max_budget_usd": "20", "bal_discover_version": "0.1.0 [repo = local]",
	} {
		if d[column] != want {
			t.Errorf("column %s = %q, want %q", column, d[column], want)
		}
	}
	for _, row := range rows {
		if row["arm"] == "A" && row["run"] == "3" && (row["cost_usd"] != "" || row["reported_cost_usd"] != "2" || row["bal_discover"] != "") {
			t.Errorf("absent values must be empty: %+v", row)
		}
		if row["arm"] == "A" && row["run"] == "2" && row["error"] != "hidden validation failed" {
			t.Errorf("error missing: %+v", row)
		}
	}
}

func TestSummaryCSVAndHTMLTable(t *testing.T) {
	root := t.TempDir()
	writeArmRuns(t, root)
	var output bytes.Buffer
	if err := WriteSummaryCSV([]string{root}, &output); err != nil {
		t.Fatal(err)
	}
	rows := parseCSV(t, output.String())
	if len(rows) != 2 || rows[0]["arm"] != "A" || rows[1]["arm"] != "D" {
		t.Fatalf("unexpected rows %v", rows)
	}
	a := rows[0]
	for column, want := range map[string]string{
		"runs": "3", "passed": "2", "pass": "2/3", "pass_rate_percent": "66.7", "median_total_tokens": "2000",
		"median_cost_usd": "2.0000", "median_duration_seconds": "200.0", "median_corrections": "0.0",
	} {
		if a[column] != want {
			t.Errorf("A %s = %q, want %q", column, a[column], want)
		}
	}
	var html bytes.Buffer
	if err := Generate([]string{root}, &html); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Arm &times; scenario summary", "<td>2/3</td>", "<td>2000</td>", "<td>2.0000</td>", "<td>200.0</td>", "<td>1/1</td>"} {
		if !strings.Contains(html.String(), want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
}

func TestSummaryKeepsModelsAndMissingValuesApart(t *testing.T) {
	root := t.TempDir()
	for index, model := range []string{"sonnet", "opus"} {
		writeTestResult(t, filepath.Join(root, model, "result.json"), result.Result{
			SchemaVersion: result.SchemaVersion, Task: "router", Language: "go", Harness: "claude", Arm: "A",
			Model: model, ThinkingBudget: "high", Run: 1, RequestedRuns: 1, Passed: index == 0,
		})
	}
	discovered, err := discover([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	rows := summarizeArms(discovered)
	if len(rows) != 2 || rows[0].Model != "opus" || rows[1].Model != "sonnet" {
		t.Fatalf("models were mixed: %+v", rows)
	}
	if rows[0].HasCost || rows[0].HasDuration {
		t.Error("a run without cost or duration must not produce a median")
	}
	var output bytes.Buffer
	if err := writeSummaryCSV(rows, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "0/1,0.0,0.0,79.3,0,0,,,0.0") {
		t.Errorf("empty cost and duration expected:\n%s", output.String())
	}
}

func TestSummaryLeavesInfrastructureFailuresOut(t *testing.T) {
	base := result.Result{SchemaVersion: result.SchemaVersion, Task: "checkout-sync", Language: "ballerina", Harness: "claude",
		Model: "sonnet", ThinkingBudget: "high", Arm: "A", RequestedRuns: 3}
	passed, failed, infra := base, base, base
	passed.Run, passed.Passed, passed.Tokens.Total = 1, true, 100
	failed.Run, failed.Tokens.Total = 2, 300
	infra.Run, infra.InfraFailure, infra.Error = 3, true, "docker daemon not running"
	rows := summarizeArms([]discoveredResult{{value: passed}, {value: failed}, {value: infra}})
	if len(rows) != 1 || rows[0].Runs != 2 || rows[0].Passed != 1 || rows[0].InfraFailures != 1 || rows[0].MedianTokens != 200 {
		t.Fatalf("unexpected row %+v", rows)
	}
	// 1/2: Wilson 95% interval 9.5% to 90.5%.
	if low, high := rows[0].PassInterval(); math.Abs(low-9.45) > 0.1 || math.Abs(high-90.55) > 0.1 {
		t.Errorf("interval = %.2f-%.2f", low, high)
	}
}

func TestDiscoverSkipsDotDirectories(t *testing.T) {
	root := t.TempDir()
	value := result.Result{SchemaVersion: result.SchemaVersion, Task: "checkout-sync", Language: "ballerina", Harness: "claude",
		Model: "sonnet", ThinkingBudget: "high", Run: 1, RequestedRuns: 1}
	for _, dir := range []string{"A/checkout-sync/run-01/x", ".infra-failures/A/checkout-sync/run-01-20261008T000000Z/x"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := result.Write(filepath.Join(root, dir, "result.json"), value); err != nil {
			t.Fatal(err)
		}
	}
	found, err := discover([]string{root})
	if err != nil || len(found) != 1 {
		t.Fatalf("found %d results (%v); want only the one outside dot-directories", len(found), err)
	}
}

package matrix

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testPlan(t *testing.T) Plan {
	t.Helper()
	return Plan{
		Arms: []string{"A", "D"}, Scenarios: []string{"checkout-sync"}, Runs: 3, Model: "sonnet",
		Language: "ballerina", MaxBudgetUSD: 20, Seed: 7, Target: t.TempDir(),
	}
}

func TestEnumerateAndValidate(t *testing.T) {
	plan := testPlan(t)
	cells := plan.Enumerate()
	if len(cells) != 6 || cells[0] != (Cell{"A", "checkout-sync", 1}) || cells[5] != (Cell{"D", "checkout-sync", 3}) {
		t.Fatalf("unexpected cells %v", cells)
	}
	exists := func(arm string) bool { return arm == "A" || arm == "D" }
	if err := plan.Validate(exists); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Plan){
		"unknown arm":      func(p *Plan) { p.Arms = []string{"Z"} },
		"unknown scenario": func(p *Plan) { p.Scenarios = []string{"nope"} },
		"no runs":          func(p *Plan) { p.Runs = 0 },
		"no budget":        func(p *Plan) { p.MaxBudgetUSD = 0 },
		"duplicate arms":   func(p *Plan) { p.Arms = []string{"A", "A"} },
		"empty":            func(p *Plan) { p.Arms = nil },
	} {
		broken := plan
		mutate(&broken)
		if broken.Validate(exists) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestShuffleIsDeterministicInterleavesArmsAndKeepsAllCells(t *testing.T) {
	plan := testPlan(t)
	plan.Arms = []string{"A", "B", "C", "D", "E"}
	plan.Runs = 5
	cells := plan.Enumerate()
	first, second := Shuffle(cells, 42), Shuffle(cells, 42)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed gave different orders")
	}
	if reflect.DeepEqual(first, Shuffle(cells, 43)) {
		t.Fatal("different seeds gave the same order")
	}
	if reflect.DeepEqual(first, cells) {
		t.Fatal("shuffle returned the enumeration order")
	}
	if len(first) != len(cells) {
		t.Fatalf("lost cells: %d", len(first))
	}
	seen := map[Cell]bool{}
	for _, cell := range first {
		seen[cell] = true
	}
	if len(seen) != len(cells) {
		t.Fatal("duplicated or dropped cells")
	}
	// Interleaving: the first half must not be dominated by a single arm.
	counts := map[string]int{}
	for _, cell := range first[:len(first)/2] {
		counts[cell.Arm]++
	}
	if len(counts) < 4 {
		t.Errorf("arms not interleaved in the first half: %v", counts)
	}
	if !reflect.DeepEqual(cells, plan.Enumerate()) {
		t.Error("Shuffle mutated its input")
	}
	// Input order must not matter.
	reversed := append([]Cell(nil), cells...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if !reflect.DeepEqual(first, Shuffle(reversed, 42)) {
		t.Error("order depends on input order")
	}
}

func TestArgsAndPaths(t *testing.T) {
	plan := testPlan(t)
	cell := Cell{"D", "checkout-sync", 2}
	args := plan.Args(cell, RunnerArgs{ArmsDir: "/arms", SkillsRepo: "/skills"})
	want := []string{"checkout-sync", "ballerina", "claude", "--n-runs=1", "--run-number=2", "--requested-runs=3",
		"--target=" + filepath.Join(plan.Target, "D", "checkout-sync", "run-02"), "--arm=D", "--model=sonnet",
		"--max-budget-usd=20", "--arms-dir=/arms", "--skills-repo=/skills"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("got %v\nwant %v", args, want)
	}
	if got := plan.MarkerPath(cell); got != filepath.Join(plan.Target, ".token-bench-completed", "D", "checkout-sync", "run-02.done") {
		t.Errorf("marker %s", got)
	}
}

func TestDryRunReportsPlanCountsAndWorstCase(t *testing.T) {
	plan := testPlan(t)
	ordered := Shuffle(plan.Enumerate(), plan.Seed)
	if err := writeMarker(plan.MarkerPath(ordered[0])); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	plan.DescribePlan(&out, ordered)
	text := out.String()
	for _, want := range []string{"2 arms x 1 scenarios x 3 runs = 6 runs", "Per-run budget cap: $20.00", "Runs to execute: 5 of 6 (1 already complete)",
		"Worst-case spend: 5 x $20.00 = $100.00", "done (skipped)", "Seed: 7"} {
		if !strings.Contains(text, want) {
			t.Errorf("dry run lacks %q:\n%s", want, text)
		}
	}
	if plan.WorstCaseUSD(0) != 0 {
		t.Error("zero runs should cost zero")
	}
}

func TestExecuteIsResumableAndRetriesCellsWithoutResult(t *testing.T) {
	plan := testPlan(t)
	ordered := plan.Enumerate()
	var called []Cell
	failures := map[Cell]bool{ordered[1]: true}
	executor := func(cell Cell, args []string) error {
		called = append(called, cell)
		if failures[cell] {
			return errors.New("docker not running")
		}
		path := filepath.Join(plan.CellDir(cell), "x", "result.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if cell == ordered[2] {
			return errors.New("agent failed validation")
		}
		return nil
	}
	var out bytes.Buffer
	first := plan.Execute(&out, ordered, RunnerArgs{}, executor)
	if first.Ran != 5 || len(first.Failed) != 1 || first.Failed[0] != ordered[1] {
		t.Fatalf("unexpected outcome %+v", first)
	}
	if !plan.Done(ordered[2]) || plan.Done(ordered[1]) {
		t.Error("a non-zero exit with a result is complete; a missing result is not")
	}
	called = nil
	failures = nil
	second := plan.Execute(&out, ordered, RunnerArgs{}, executor)
	if len(called) != 1 || called[0] != ordered[1] || second.Ran != 1 || second.Skipped != 5 {
		t.Fatalf("resume should only rerun the failed cell: called %v outcome %+v", called, second)
	}
	third := plan.Execute(&out, ordered, RunnerArgs{}, executor)
	if third.Ran != 0 || third.Skipped != 6 {
		t.Fatalf("everything should be skipped: %+v", third)
	}
}

func TestCheckOrWritePlanRefusesMixedModels(t *testing.T) {
	plan := testPlan(t)
	if err := plan.CheckOrWritePlan(); err != nil {
		t.Fatal(err)
	}
	if err := plan.CheckOrWritePlan(); err != nil {
		t.Fatalf("same plan must resume: %v", err)
	}
	other := plan
	other.Seed = 99
	other.Runs = 5
	if err := other.CheckOrWritePlan(); err != nil {
		t.Fatalf("seed and runs may change between invocations: %v", err)
	}
	other.Model = "opus"
	if err := other.CheckOrWritePlan(); err == nil {
		t.Fatal("a different model in the same target must be refused")
	}
	other = plan
	other.MaxBudgetUSD = 5
	if err := other.CheckOrWritePlan(); err == nil {
		t.Fatal("a different budget in the same target must be refused")
	}
}

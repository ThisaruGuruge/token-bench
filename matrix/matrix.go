// Package matrix plans and executes an arm x scenario x run benchmark matrix.
package matrix

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// KnownScenarios lists the scenarios the runner accepts.
var KnownScenarios = []string{
	"checkout-sync", "claim-check", "content-based-router", "content-enricher",
	"dead-letter-channel", "multi-consumer", "scatter-gather",
}

// Plan is a fully specified matrix.
type Plan struct {
	Arms         []string `json:"arms"`
	Scenarios    []string `json:"scenarios"`
	Runs         int      `json:"runs"`
	Model        string   `json:"model"`
	Language     string   `json:"language"`
	MaxBudgetUSD float64  `json:"maxBudgetUsd"`
	Seed         uint64   `json:"seed"`
	Target       string   `json:"-"`
}

// Cell is one benchmark run: an arm on a scenario, run number 1..Runs.
type Cell struct {
	Arm      string
	Scenario string
	Run      int
}

func (c Cell) String() string {
	return fmt.Sprintf("arm=%s scenario=%s run=%d", c.Arm, c.Scenario, c.Run)
}

// Validate checks the plan; armExists reports whether an arm definition exists.
func (p Plan) Validate(armExists func(string) bool) error {
	if len(p.Arms) == 0 || len(p.Scenarios) == 0 {
		return errors.New("at least one arm and one scenario are required")
	}
	if p.Runs < 1 {
		return errors.New("runs per cell must be at least 1")
	}
	if p.Model == "" || p.Language == "" {
		return errors.New("model and language are required")
	}
	if p.MaxBudgetUSD <= 0 {
		return errors.New("max budget must be positive")
	}
	if p.Target == "" {
		return errors.New("target directory is required")
	}
	for _, arm := range p.Arms {
		if !armExists(arm) {
			return fmt.Errorf("unknown arm %q", arm)
		}
	}
	for _, scenario := range p.Scenarios {
		if !contains(KnownScenarios, scenario) {
			return fmt.Errorf("unknown scenario %q (known: %s)", scenario, strings.Join(KnownScenarios, ", "))
		}
	}
	if duplicated(p.Arms) || duplicated(p.Scenarios) {
		return errors.New("arms and scenarios must not repeat")
	}
	return nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func duplicated(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

// Enumerate lists every cell in a stable order (arm, scenario, run).
func (p Plan) Enumerate() []Cell {
	var cells []Cell
	for _, arm := range p.Arms {
		for _, scenario := range p.Scenarios {
			for run := 1; run <= p.Runs; run++ {
				cells = append(cells, Cell{Arm: arm, Scenario: scenario, Run: run})
			}
		}
	}
	return cells
}

// Shuffle returns the cells in a seed-determined order so arms interleave in
// time. The same seed and the same cells always give the same order.
func Shuffle(cells []Cell, seed uint64) []Cell {
	ordered := append([]Cell(nil), cells...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Arm != ordered[j].Arm {
			return ordered[i].Arm < ordered[j].Arm
		}
		if ordered[i].Scenario != ordered[j].Scenario {
			return ordered[i].Scenario < ordered[j].Scenario
		}
		return ordered[i].Run < ordered[j].Run
	})
	generator := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	generator.Shuffle(len(ordered), func(i, j int) { ordered[i], ordered[j] = ordered[j], ordered[i] })
	return ordered
}

// CellDir is where a cell's token-bench run directory is created.
func (p Plan) CellDir(cell Cell) string {
	return filepath.Join(p.Target, cell.Arm, cell.Scenario, fmt.Sprintf("run-%02d", cell.Run))
}

// MarkerPath is the completion marker of a cell, mirroring the Makefile's
// .token-bench-completed layout.
func (p Plan) MarkerPath(cell Cell) string {
	return filepath.Join(p.Target, ".token-bench-completed", cell.Arm, cell.Scenario, fmt.Sprintf("run-%02d.done", cell.Run))
}

// Done reports whether the cell has a completion marker.
func (p Plan) Done(cell Cell) bool {
	_, err := os.Stat(p.MarkerPath(cell))
	return err == nil
}

// Pending filters out completed cells, preserving order.
func (p Plan) Pending(cells []Cell) []Cell {
	var pending []Cell
	for _, cell := range cells {
		if !p.Done(cell) {
			pending = append(pending, cell)
		}
	}
	return pending
}

// WorstCaseUSD bounds the spend of n runs: every run hitting its budget cap.
// Claude Code checks the cap between turns, so a run can overshoot slightly.
func (p Plan) WorstCaseUSD(runs int) float64 { return float64(runs) * p.MaxBudgetUSD }

// RunnerArgs are the token-bench arguments for one cell.
type RunnerArgs struct {
	ArmsDir         string
	SkillsRepo      string
	PhoenixEndpoint string
}

// Args builds the token-bench command line (without the executable) for a cell.
func (p Plan) Args(cell Cell, extra RunnerArgs) []string {
	args := []string{
		cell.Scenario, p.Language, "claude",
		"--n-runs=1",
		fmt.Sprintf("--run-number=%d", cell.Run),
		fmt.Sprintf("--requested-runs=%d", p.Runs),
		"--target=" + p.CellDir(cell),
		"--arm=" + cell.Arm,
		"--model=" + p.Model,
		fmt.Sprintf("--max-budget-usd=%g", p.MaxBudgetUSD),
	}
	if extra.ArmsDir != "" {
		args = append(args, "--arms-dir="+extra.ArmsDir)
	}
	if extra.SkillsRepo != "" {
		args = append(args, "--skills-repo="+extra.SkillsRepo)
	}
	if extra.PhoenixEndpoint != "" {
		args = append(args, "--phoenix-endpoint="+extra.PhoenixEndpoint)
	}
	return args
}

// DescribePlan writes the dry-run report: every pending cell in execution
// order, counts, and the worst-case spend.
func (p Plan) DescribePlan(out io.Writer, ordered []Cell) {
	pending := p.Pending(ordered)
	fmt.Fprintf(out, "Matrix: %d arms x %d scenarios x %d runs = %d runs\n", len(p.Arms), len(p.Scenarios), p.Runs, len(ordered))
	fmt.Fprintf(out, "Arms:      %s\n", strings.Join(p.Arms, ", "))
	fmt.Fprintf(out, "Scenarios: %s\n", strings.Join(p.Scenarios, ", "))
	fmt.Fprintf(out, "Model: %s   Language: %s   Seed: %d   Target: %s\n", p.Model, p.Language, p.Seed, p.Target)
	fmt.Fprintf(out, "Per-run budget cap: $%.2f\n\n", p.MaxBudgetUSD)
	fmt.Fprintln(out, "Order (seeded shuffle):")
	for index, cell := range ordered {
		status := "pending"
		if p.Done(cell) {
			status = "done (skipped)"
		}
		fmt.Fprintf(out, "  %3d. %-48s %s\n", index+1, cell, status)
	}
	fmt.Fprintf(out, "\nRuns to execute: %d of %d (%d already complete)\n", len(pending), len(ordered), len(ordered)-len(pending))
	fmt.Fprintf(out, "Worst-case spend: %d x $%.2f = $%.2f (every run hitting its cap; a real run costs far less)\n",
		len(pending), p.MaxBudgetUSD, p.WorstCaseUSD(len(pending)))
}

// CheckOrWritePlan records the plan's model, language and budget in the target
// directory, and refuses to continue a matrix started with different ones.
func (p Plan) CheckOrWritePlan() error {
	path := filepath.Join(p.Target, "matrix-plan.json")
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(p.Target, 0o755); err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(struct {
			Plan
			Created string `json:"created"`
		}{p, time.Now().UTC().Format(time.RFC3339)}, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(encoded, '\n'), 0o644)
	}
	if err != nil {
		return err
	}
	var existing Plan
	if err := json.Unmarshal(content, &existing); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if existing.Model != p.Model || existing.Language != p.Language || existing.MaxBudgetUSD != p.MaxBudgetUSD {
		return fmt.Errorf("%s was started with model %s, language %s and budget $%g; use another --target to change them",
			p.Target, existing.Model, existing.Language, existing.MaxBudgetUSD)
	}
	return nil
}

// Outcome of executing a matrix.
type Outcome struct {
	Ran, Skipped int
	Failed       []Cell
}

// Executor runs one cell, returning an error when the run could not even be
// started or exited abnormally.
type Executor func(cell Cell, args []string) error

// Execute runs the pending cells in order. A cell completes when its run wrote
// a result.json, whatever the verdict: an agent failure is a data point and is
// never rerun automatically. A cell without a result is reported and left
// without a marker so the next invocation retries it.
func (p Plan) Execute(out io.Writer, ordered []Cell, extra RunnerArgs, run Executor) Outcome {
	var outcome Outcome
	pending := p.Pending(ordered)
	outcome.Skipped = len(ordered) - len(pending)
	for index, cell := range pending {
		fmt.Fprintf(out, "[%d/%d] %s\n", index+1, len(pending), cell)
		if err := os.MkdirAll(p.CellDir(cell), 0o755); err != nil {
			fmt.Fprintf(out, "  cannot create the cell directory: %v\n", err)
			outcome.Failed = append(outcome.Failed, cell)
			continue
		}
		runErr := run(cell, p.Args(cell, extra))
		if !hasResult(p.CellDir(cell)) {
			fmt.Fprintf(out, "  no result.json written (%v); not marked complete, will retry on the next invocation\n", runErr)
			outcome.Failed = append(outcome.Failed, cell)
			continue
		}
		if err := writeMarker(p.MarkerPath(cell)); err != nil {
			fmt.Fprintf(out, "  cannot write the completion marker: %v\n", err)
			outcome.Failed = append(outcome.Failed, cell)
			continue
		}
		if runErr != nil {
			fmt.Fprintf(out, "  runner exited non-zero (%v); result kept (agent or validation failure)\n", runErr)
		}
		outcome.Ran++
	}
	return outcome
}

func hasResult(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "result.json" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func writeMarker(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

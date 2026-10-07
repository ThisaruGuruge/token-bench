// Command token-bench-matrix runs an arm x scenario x run matrix through the
// token-bench runner, one cell at a time, in a seeded shuffled order.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"token-bench/arm"
	"token-bench/harness"
	"token-bench/matrix"
)

const usage = `usage: token-bench-matrix [--phase=NAME] [--arms=A,D] [--scenarios=a,b] [--runs=N]
                          [--model=NAME] [--target=DIR] [--seed=N] [--max-budget-usd=N]
                          [--dry-run] [--runner=./token-bench] [--language=ballerina]
                          [--arms-dir=DIR] [--skills-repo=DIR] [--phoenix-endpoint=URL] [--list]

Enumerates arm x scenario x run cells, shuffles them with --seed so arms interleave in time,
skips cells that already have a completion marker, and runs each remaining cell with the
token-bench runner. --dry-run prints the plan, the run count and a worst-case spend bound
(pending runs x --max-budget-usd) and runs nothing.

--phase loads a preset from <arms-dir>/phases.json; any other flag overrides the preset.
`

type config struct {
	phase, arms, scenarios, model, target, runner, language, armsDir, skillsRepo, phoenix string
	runs                                                                                  int
	seed                                                                                  uint64
	budget                                                                                float64
	dryRun, list                                                                          bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	var c config
	flags := flag.NewFlagSet("token-bench-matrix", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	flags.StringVar(&c.phase, "phase", "", "phase preset name")
	flags.StringVar(&c.arms, "arms", "", "comma-separated arm ids")
	flags.StringVar(&c.scenarios, "scenarios", "", "comma-separated scenario names")
	flags.IntVar(&c.runs, "runs", 0, "runs per arm x scenario cell (default 3)")
	flags.StringVar(&c.model, "model", "", "Claude model alias or id (default sonnet)")
	flags.StringVar(&c.target, "target", "", "results directory (default ./results)")
	flags.Uint64Var(&c.seed, "seed", 1, "shuffle seed")
	flags.Float64Var(&c.budget, "max-budget-usd", harness.DefaultMaxBudgetUSD, "per-run spend cap")
	flags.BoolVar(&c.dryRun, "dry-run", false, "print the plan and worst-case spend, run nothing")
	flags.BoolVar(&c.list, "list", false, "list arms and phases")
	flags.StringVar(&c.runner, "runner", "./token-bench", "token-bench executable")
	flags.StringVar(&c.language, "language", "ballerina", "target language")
	flags.StringVar(&c.armsDir, "arms-dir", "", "arms directory")
	flags.StringVar(&c.skillsRepo, "skills-repo", "", "skills repository checkout")
	flags.StringVar(&c.phoenix, "phoenix-endpoint", "", "Phoenix OTLP/HTTP endpoint")
	if err := flags.Parse(arguments); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	armsDir := arm.ArmsDirectory(c.armsDir)
	if c.list {
		return list(stdout, stderr, armsDir)
	}
	plan, err := buildPlan(c, set, armsDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := plan.Validate(func(id string) bool { return arm.Exists(armsDir, id) }); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ordered := matrix.Shuffle(plan.Enumerate(), plan.Seed)
	extra := matrix.RunnerArgs{ArmsDir: c.armsDir, SkillsRepo: c.skillsRepo, PhoenixEndpoint: c.phoenix}
	if c.dryRun {
		plan.DescribePlan(stdout, ordered)
		fmt.Fprintf(stdout, "\nArms directory: %s\nSkills repository: %s\n", armsDir, arm.SkillsRepository(c.skillsRepo))
		return 0
	}
	if _, err := os.Stat(c.runner); err != nil {
		fmt.Fprintf(stderr, "runner %s not found (run `make token-bench`): %v\n", c.runner, err)
		return 2
	}
	if err := plan.CheckOrWritePlan(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	executor := func(cell matrix.Cell, args []string) error {
		command := exec.Command(c.runner, args...)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, stdout, stderr
		return command.Run()
	}
	outcome := plan.Execute(stdout, ordered, extra, executor)
	fmt.Fprintf(stdout, "\nRan %d, skipped %d already complete, %d without a result\n", outcome.Ran, outcome.Skipped, len(outcome.Failed))
	for _, cell := range outcome.Failed {
		fmt.Fprintf(stdout, "  retry on next invocation: %s\n", cell)
	}
	if len(outcome.Failed) > 0 {
		return 1
	}
	return 0
}

func buildPlan(c config, set map[string]bool, armsDir string) (matrix.Plan, error) {
	plan := matrix.Plan{Runs: 3, Model: "sonnet", Language: c.language, MaxBudgetUSD: c.budget, Seed: c.seed, Target: "results"}
	if c.phase != "" {
		phases, err := arm.LoadPhases(armsDir)
		if err != nil {
			return plan, fmt.Errorf("load phases: %w", err)
		}
		phase, ok := phases[c.phase]
		if !ok {
			return plan, fmt.Errorf("unknown phase %q (known: %s)", c.phase, strings.Join(arm.PhaseNames(phases), ", "))
		}
		plan.Arms, plan.Scenarios, plan.Runs = phase.Arms, phase.Scenarios, phase.Runs
		if phase.Model != "" {
			plan.Model = phase.Model
		}
	}
	if set["arms"] {
		plan.Arms = split(c.arms)
	}
	if set["scenarios"] {
		plan.Scenarios = split(c.scenarios)
	}
	if set["runs"] {
		plan.Runs = c.runs
	}
	if set["model"] {
		plan.Model = c.model
	}
	if set["target"] {
		plan.Target = c.target
	}
	return plan, nil
}

func split(value string) []string {
	var parts []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func list(stdout, stderr io.Writer, armsDir string) int {
	ids, err := arm.List(armsDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	fmt.Fprintln(stdout, "Arms:")
	for _, id := range ids {
		config, err := arm.Load(armsDir, id)
		if err != nil {
			fmt.Fprintf(stderr, "arm %s: %v\n", id, err)
			return 2
		}
		fmt.Fprintf(stdout, "  %s  %s\n", id, config.Label)
	}
	if phases, err := arm.LoadPhases(armsDir); err == nil {
		fmt.Fprintln(stdout, "Phases:")
		for _, name := range arm.PhaseNames(phases) {
			fmt.Fprintf(stdout, "  %s  %s\n", name, phases[name].Description)
		}
	}
	return 0
}

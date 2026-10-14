package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"token-bench/harness"
	"token-bench/language"
	"token-bench/pricing"
	benchresult "token-bench/result"
	"token-bench/streamlog"
	"token-bench/task"
	"token-bench/task/answer"
	"token-bench/tracing"
)

// TODO: add optional harness flags (system prompt, thinking)
const hiddenValidationFailure = "hidden validation failed"

const usage = `usage: token-bench <task> <language> <harness> [--n-runs=N] [--target=DIR] [--skills=DIR] [--phoenix-endpoint=URL]
                   [--model=NAME] [--arm=LABEL] [--max-budget-usd=N] [--arms-dir=DIR] [--skills-repo=DIR]
                   [--run-number=N] [--requested-runs=N] [--api-key-helper=CMD|none]

Implementations:
  task:      checkout-sync, claim-check, content-based-router, content-enricher, dead-letter-channel, multi-consumer, scatter-gather,
             dq-D01 .. dq-D18 (discover question tasks: single shot, graded from answer.json)
  language:  ballerina, go, java, python
  harness:   claude, pi

Notes:
  claude loads --skills as a temporary plugin; global Claude configuration may still be discovered.
  --model selects the Claude model alias or id (default opus); claude harness only.
  --arm=A..E selects an arm definition from --arms-dir (default ./arms, or $TOKEN_BENCH_ARMS_DIR) and applies its
    skills or plugin, appended system prompt, environment and bal discover visibility (claude harness only). A label
    with no definition is an ad-hoc label: recorded and used for report grouping, with no behavior attached.
  --skills-repo is the ballerina-platform/skills checkout arms read from ($TOKEN_BENCH_SKILLS_REPO, default
    ~/Projects/Ballerina/cli-improvements/benchmark/reference/ballerina-skills).
  --max-budget-usd caps one Claude session's spend (default 20) and is recorded in result.json.
  --api-key-helper is a shell command printing the benchmark API key, made Claude Code's only credential source
    (its apiKeyHelper setting; inherited ANTHROPIC_API_KEY and other credentials are removed from its environment).
    Default: the macOS Keychain item token-bench-anthropic-api-key ($TOKEN_BENCH_API_KEY_HELPER overrides).
    "none" uses the inherited credentials instead. The helper is checked before the first run.
  --run-number and --requested-runs label the first run's number and the cell's run count (used by the matrix runner).
  --phoenix-endpoint enables tracing to a locally hosted Arize Phoenix OTLP/HTTP endpoint.
`

type options struct {
	task            string
	language        string
	harness         string
	runs            int
	target          string
	skillsDir       string
	model           string
	arm             string
	phoenixEndpoint string
	maxBudgetUSD    float64
	armsDir         string
	skillsRepo      string
	runNumber       int
	requestedRuns   int
	apiKeyHelper    string
}

// defaultAPIKeyHelper reads the benchmark key from the macOS Keychain. Store it
// with: security add-generic-password -a "$USER" -s token-bench-anthropic-api-key -w
const defaultAPIKeyHelper = "security find-generic-password -s token-bench-anthropic-api-key -w"

// resolveAPIKeyHelper returns the helper command for the claude harness: the
// flag, else $TOKEN_BENCH_API_KEY_HELPER, else the Keychain default; "none"
// disables it (inherited credentials).
func resolveAPIKeyHelper(flag string) string {
	helper := flag
	if helper == "" {
		helper = os.Getenv("TOKEN_BENCH_API_KEY_HELPER")
	}
	if helper == "" {
		helper = defaultAPIKeyHelper
	}
	if helper == "none" {
		return ""
	}
	return helper
}

// checkAPIKeyHelper runs the helper once and fails unless it prints a value.
// The output is the secret: it is only measured, never printed or stored.
func checkAPIKeyHelper(helper string) error {
	command := exec.Command("sh", "-c", helper)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	empty := len(strings.TrimSpace(string(output))) == 0
	clear(output)
	if err != nil || empty {
		reason := strings.TrimSpace(stderr.String())
		if err != nil && reason == "" {
			reason = err.Error()
		}
		if reason == "" {
			reason = "it printed nothing"
		}
		return fmt.Errorf("API key helper %q failed: %s\nstore the benchmark key with: security add-generic-password -a \"$USER\" -s token-bench-anthropic-api-key -w\n(or pass --api-key-helper=none to use the inherited credentials)", helper, reason)
	}
	return nil
}

type runResult = benchresult.Result

type tokenSummary struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	Total      float64 `json:"total"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

type report struct {
	Task      string       `json:"task"`
	Language  string       `json:"language"`
	Harness   string       `json:"harness"`
	SkillsDir string       `json:"skillsDir,omitempty"`
	Runs      []runResult  `json:"runs"`
	Mean      tokenSummary `json:"mean"`
	Median    tokenSummary `json:"median"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) (exitCode int) {
	var traceProvider tracing.Tracer
	defer func() {
		if traceProvider == nil {
			return
		}
		if err := traceProvider.Shutdown(); err != nil {
			fmt.Fprintln(os.Stderr, renderCLIError(fmt.Sprintf("Tracing error: %v", err)))
		}
	}()

	configuration, err := parseOptions(arguments)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	taskDefinition, err := selectTask(configuration.task)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	promptTemplate, err := task.BenchmarkPromptTemplate(taskDefinition)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	languageImplementation, err := selectLanguage(configuration.language)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	configuration.skillsDir, err = normalizeSkillsDir(configuration.skillsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	statusUpdates := make(chan string, 1)
	harnessConfig := harness.Config{SkillsDir: configuration.skillsDir, Status: statusUpdates, Model: configuration.model, MaxBudgetUSD: configuration.maxBudgetUSD}
	if configuration.harness == "claude" {
		harnessConfig.APIKeyHelper = resolveAPIKeyHelper(configuration.apiKeyHelper)
		if harnessConfig.APIKeyHelper != "" {
			if err := checkAPIKeyHelper(harnessConfig.APIKeyHelper); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
		}
	}
	if _, err := selectHarness(configuration.harness, harnessConfig); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if configuration.phoenixEndpoint != "" {
		traceProvider, err = tracing.NewPhoenix(tracing.PhoenixConfig{Endpoint: configuration.phoenixEndpoint})
		if err != nil {
			fmt.Fprintln(os.Stderr, fmt.Errorf("initialize tracing: %w", err))
			return 1
		}
	}

	target, err := prepareTarget(configuration.target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	setup, err := newArmSetup(configuration, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if setup.arm != nil {
		harnessConfig.SkillsDir = setup.arm.SkillsDir
		harnessConfig.PluginDir = setup.arm.PluginDir
		harnessConfig.ExtraSystemPrompt = setup.arm.ExtraSystemPrompt
		configuration.skillsDir = setup.displaySkills()
		for _, warning := range setup.arm.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", warning)
		}
	}
	display := startStatusDisplay(statusUpdates, os.Stderr)
	result := report{Task: taskDefinition.Name, Language: configuration.language, Harness: configuration.harness, SkillsDir: configuration.skillsDir}
	for run := 1; run <= configuration.runs; run++ {
		directory, err := createRunDirectory(target, configuration.language, taskDefinition.Name, configuration.harness)
		if err != nil {
			_ = display.Close()
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		runEnv, err := setup.prepareRun()
		if err != nil {
			_ = display.Close()
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		runHarnessConfig := harnessConfig
		runEnv.configure(&runHarnessConfig, setup.arm)
		harnessConstructor, err := selectHarness(configuration.harness, runHarnessConfig)
		if err != nil {
			_ = runEnv.cleanup()
			_ = display.Close()
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		current := runResult{
			SchemaVersion:  benchresult.SchemaVersion,
			Task:           taskDefinition.Name,
			Language:       configuration.language,
			Harness:        configuration.harness,
			Arm:            configuration.arm,
			SkillsDir:      configuration.skillsDir,
			PromptTemplate: promptTemplate,
			Run:            configuration.runNumber + run - 1,
			RequestedRuns:  configuration.requestedRuns,
			Environment:    setup.environment(configuration, harnessConfig),
		}
		current = executeRun(current, directory, taskDefinition, languageImplementation, harnessConstructor, traceProvider)
		if err := runEnv.collect(&current, directory, taskDefinition, setup.arm); err != nil {
			current.Error = joinErrors(current.Error, err.Error())
		}
		if err := runEnv.cleanup(); err != nil {
			current.Error = joinErrors(current.Error, err.Error())
		}
		result.Runs = append(result.Runs, current)
		if err := benchresult.Write(filepath.Join(directory, "result.json"), current); err != nil {
			_ = display.Close()
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	result.Mean, result.Median = summarize(result.Runs)
	finalResults, err := renderReport(result, target)
	if err != nil {
		_ = display.Close()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := display.Finish(finalResults); err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("render benchmark results: %w", err))
		return 1
	}
	return benchmarkExitCode(result.Runs)
}

func benchmarkExitCode(runs []runResult) int {
	for _, current := range runs {
		if !current.Passed && current.Error != hiddenValidationFailure {
			return 1
		}
	}
	return 0
}

func executeRun(current runResult, directory string, definition task.Task, implementation language.Language, newHarness func() harness.Harness, traceProvider tracing.Tracer) (result runResult) {
	result = current
	agent := newHarness()
	result.Model = agent.Model()
	result.ThinkingBudget = agent.ThinkingBudget()
	var benchmark tracing.BenchmarkSpan
	if traceProvider != nil {
		benchmark = traceProvider.StartBenchmarkSpan(current.Language, definition.Name, current.Harness, current.SkillsDir)
		defer func() {
			if result.Passed && result.Error == "" {
				benchmark.SetOK()
			} else {
				description := result.Error
				if description == "" {
					description = "benchmark validation failed"
				}
				benchmark.SetError(description)
			}
			benchmark.End()
		}()
	}
	if definition.IsAnswer() {
		return executeAnswerRun(result, directory, definition, agent, benchmark)
	}
	ports, err := allocatePorts(definition.Resources.NPorts)
	if err != nil {
		return infrastructureFailure(result, err)
	}
	allocated := task.AllocResources{Ports: ports, WorkDir: directory}
	defer allocated.Cleanup()
	handle := definition.InitFn(allocated)
	cleanupTask, err := handle.Setup()
	if err != nil {
		return infrastructureFailure(result, err)
	}
	defer func() {
		if err := cleanupTask(); err != nil {
			result.Error = joinErrors(result.Error, err.Error())
		}
	}()

	if len(ports) == 0 {
		return infrastructureFailure(result, errors.New("task allocated no application port"))
	}
	if err := agent.Start(directory, benchmark); err != nil {
		return infrastructureFailure(result, err)
	}
	defer finishAgent(&result, agent, directory)
	started := time.Now()
	defer func() { result.DurationSeconds = time.Since(started).Seconds() }()

	requirements, err := handle.Prompt()
	if err != nil {
		return infrastructureFailure(result, err)
	}
	prompt, err := task.RenderPrompt(task.PromptConfig{
		Language:          implementation.Name(),
		InitializeCommand: implementation.InitializeCommand(),
		TaskPromptSuffix:  implementation.TaskPromptSuffix(),
		Ports:             ports,
		Endpoints:         definition.Endpoints,
		Requirements:      requirements,
	})
	if err != nil {
		return infrastructureFailure(result, err)
	}
	if _, err := agent.SendMessage(prompt); err != nil {
		return agentFailure(result, err)
	}

	for {
		stopProject, startErr := implementation.StartProject(directory, ports[0])
		feedback := ""
		if startErr != nil {
			feedback = fmt.Sprintf("The application failed to start. Exact diagnosis:\n%s", startErr)
		} else {
			feedback, err = handle.Feedback()
			if err == nil && feedback == "" {
				result.Passed, err = handle.Validation()
			}
			if stopErr := stopProject(); stopErr != nil {
				err = errors.Join(err, stopErr)
			}
		}
		if err != nil {
			// Feedback and Validation report the agent's mistakes as feedback;
			// an error means the task's own checks or backends failed.
			return infrastructureFailure(result, err)
		}
		if result.Passed {
			return result
		}
		if feedback == "" {
			result.Error = hiddenValidationFailure
			return result
		}
		if result.Corrections == 3 {
			result.Error = feedback
			return result
		}
		result.Corrections++
		if _, err := agent.SendMessage(feedback); err != nil {
			return agentFailure(result, err)
		}
	}
}

// finishAgent collects the agent's usage and artifacts after a run; deferred so
// it runs on every exit path.
func finishAgent(result *runResult, agent harness.Harness, directory string) {
	if count, err := agent.TokenCount(); err == nil {
		result.Tokens = benchresult.TokenCount{
			Input: count.Input, Output: count.Output, Total: count.Total,
			CacheRead: count.CacheRead, CacheWrite: count.CacheWrite,
		}
	} else {
		result.Error = joinErrors(result.Error, err.Error())
	}
	if err := agent.End(); err != nil {
		result.Error = joinErrors(result.Error, err.Error())
	}
	if reported, ok := agent.ReportedCostUSD(); ok {
		result.ReportedCostUSD = &reported
	}
	if err := attachToolUsage(result, filepath.Join(directory, harness.ClaudeStreamArtifact)); err != nil {
		result.Error = joinErrors(result.Error, err.Error())
	}
	applyEstimatedCost(result)
}

// executeAnswerRun runs a single-shot answer task: no ports, no project, no
// feedback. The prompt goes out as is, and the answer file the agent left in
// directory is graded once the agent's turn ends.
func executeAnswerRun(result runResult, directory string, definition task.Task, agent harness.Harness, benchmark tracing.BenchmarkSpan) runResult {
	handle := definition.InitFn(task.AllocResources{WorkDir: directory})
	cleanupTask, err := handle.Setup()
	if err != nil {
		return infrastructureFailure(result, err)
	}
	defer func() {
		if err := cleanupTask(); err != nil {
			result.Error = joinErrors(result.Error, err.Error())
		}
	}()
	prompt, err := handle.Prompt()
	if err != nil {
		return infrastructureFailure(result, err)
	}
	if err := agent.Start(directory, benchmark); err != nil {
		return infrastructureFailure(result, err)
	}
	started := time.Now()
	_, sendErr := agent.SendMessage(prompt)
	result.DurationSeconds = time.Since(started).Seconds()
	finishAgent(&result, agent, directory)
	if sendErr != nil {
		result = agentFailure(result, sendErr)
		if result.InfraFailure {
			return result
		}
	}
	if grader, ok := handle.(answer.Grader); ok {
		grade, err := grader.Grade()
		if err != nil {
			return infrastructureFailure(result, err)
		}
		result.Answer = &grade
		result.Passed = grade.Passed()
	} else if result.Passed, err = handle.Validation(); err != nil {
		return infrastructureFailure(result, err)
	}
	if !result.Passed && result.Error == "" {
		result.Error = hiddenValidationFailure
	}
	return result
}

// infrastructureFailure records err as a failure of the benchmark machinery
// (ports, Docker backends, the task's checks): the run is invalid and is rerun.
func infrastructureFailure(result runResult, err error) runResult {
	result.Error = joinErrors(result.Error, err.Error())
	result.InfraFailure = true
	return result
}

// agentFailure records an error from the agent harness, classifying a harness
// or model API failure as infrastructure and a spend or turn cap as a stop.
func agentFailure(result runResult, err error) runResult {
	result.Error = joinErrors(result.Error, err.Error())
	if harness.IsInfrastructure(err) {
		result.InfraFailure = true
	}
	if reason, ok := harness.StopReason(err); ok {
		result.StopReason = reason
	}
	return result
}

// applyEstimatedCost fills CostUSD from the price table. It prefers the
// per-model usage parsed from the stream log, which names the resolved model
// of every call (subagents included) and splits cache writes by TTL; writes
// without a TTL breakdown are priced at the 1-hour rate, the one Claude Code
// normally uses. Otherwise it prices the token totals at the run's model with
// 5-minute cache writes. A model missing from the table leaves CostUSD unset.
func applyEstimatedCost(result *runResult) {
	if result.ToolUsage != nil && len(result.ToolUsage.Models) > 0 {
		total, complete := 0.0, true
		for model, usage := range result.ToolUsage.Models {
			cost, ok := pricing.UsageCost(model, pricing.Usage{
				Input: usage.Input, Output: usage.Output, CacheRead: usage.CacheRead,
				CacheWrite5m: usage.CacheWrite5m, CacheWrite1h: usage.CacheWrite1h + usage.CacheWriteUnsplit,
			})
			if !ok {
				complete = false
				break
			}
			total += cost
		}
		if complete {
			result.CostUSD = &total
			result.CostEstimateBasis = benchresult.CostBasisStreamUsage
			return
		}
	}
	if cost, ok := pricing.Cost(result.Model, result.Tokens); ok {
		result.CostUSD = &cost
		result.CostEstimateBasis = benchresult.CostBasisTokenTotals
	}
}

// attachToolUsage summarizes the Claude stream log at path. A missing log is
// not an error: other harnesses write none.
func attachToolUsage(result *runResult, path string) error {
	summary, err := streamlog.AnalyzeFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("analyze stream log: %w", err)
	}
	result.ToolUsage = &summary
	return nil
}

func selectTask(name string) (task.Task, error) {
	definition, ok := task.ByName(name)
	if !ok {
		return task.Task{}, fmt.Errorf("unknown task %q", name)
	}
	return definition, nil
}

func selectLanguage(name string) (language.Language, error) {
	switch name {
	case "ballerina":
		return language.NewBallerina(), nil
	case "go":
		return language.NewGo(), nil
	case "java":
		return language.NewJava(), nil
	case "python":
		return language.NewPython(), nil
	default:
		return nil, fmt.Errorf("unknown language %q", name)
	}
}

func selectHarness(name string, config harness.Config) (func() harness.Harness, error) {
	switch name {
	case "claude":
		return func() harness.Harness { return harness.NewClaude(config) }, nil
	case "pi":
		return func() harness.Harness { return harness.NewPi(config) }, nil
	default:
		return nil, fmt.Errorf("unknown harness %q", name)
	}
}

func parseOptions(arguments []string) (options, error) {
	parsed := options{runs: 1}
	positionals := make([]string, 0, 3)
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-h" || argument == "--help":
			fmt.Print(usage)
			os.Exit(0)
		case strings.HasPrefix(argument, "--n-runs="):
			value, err := strconv.Atoi(strings.TrimPrefix(argument, "--n-runs="))
			if err != nil || value < 1 {
				return options{}, errors.New("--n-runs must be a positive integer")
			}
			parsed.runs = value
		case argument == "--n-runs":
			index++
			if index == len(arguments) {
				return options{}, errors.New("--n-runs requires a value")
			}
			value, err := strconv.Atoi(arguments[index])
			if err != nil || value < 1 {
				return options{}, errors.New("--n-runs must be a positive integer")
			}
			parsed.runs = value
		case strings.HasPrefix(argument, "--skills="):
			if parsed.skillsDir != "" {
				return options{}, errors.New("--skills may only be specified once")
			}
			parsed.skillsDir = strings.TrimPrefix(argument, "--skills=")
			if parsed.skillsDir == "" {
				return options{}, errors.New("--skills requires a value")
			}
		case argument == "--skills":
			if parsed.skillsDir != "" {
				return options{}, errors.New("--skills may only be specified once")
			}
			index++
			if index == len(arguments) || arguments[index] == "" {
				return options{}, errors.New("--skills requires a value")
			}
			parsed.skillsDir = arguments[index]
		case strings.HasPrefix(argument, "--model="):
			if parsed.model != "" {
				return options{}, errors.New("--model may only be specified once")
			}
			parsed.model = strings.TrimPrefix(argument, "--model=")
			if parsed.model == "" {
				return options{}, errors.New("--model requires a value")
			}
		case argument == "--model":
			if parsed.model != "" {
				return options{}, errors.New("--model may only be specified once")
			}
			index++
			if index == len(arguments) || arguments[index] == "" {
				return options{}, errors.New("--model requires a value")
			}
			parsed.model = arguments[index]
		case strings.HasPrefix(argument, "--arm="):
			if parsed.arm != "" {
				return options{}, errors.New("--arm may only be specified once")
			}
			parsed.arm = strings.TrimPrefix(argument, "--arm=")
			if parsed.arm == "" {
				return options{}, errors.New("--arm requires a value")
			}
		case argument == "--arm":
			if parsed.arm != "" {
				return options{}, errors.New("--arm may only be specified once")
			}
			index++
			if index == len(arguments) || arguments[index] == "" {
				return options{}, errors.New("--arm requires a value")
			}
			parsed.arm = arguments[index]
		case strings.HasPrefix(argument, "--phoenix-endpoint="):
			if parsed.phoenixEndpoint != "" {
				return options{}, errors.New("--phoenix-endpoint may only be specified once")
			}
			parsed.phoenixEndpoint = strings.TrimPrefix(argument, "--phoenix-endpoint=")
			if parsed.phoenixEndpoint == "" {
				return options{}, errors.New("--phoenix-endpoint requires a value")
			}
		case argument == "--phoenix-endpoint":
			if parsed.phoenixEndpoint != "" {
				return options{}, errors.New("--phoenix-endpoint may only be specified once")
			}
			index++
			if index == len(arguments) || arguments[index] == "" {
				return options{}, errors.New("--phoenix-endpoint requires a value")
			}
			parsed.phoenixEndpoint = arguments[index]
		case isFlag(argument, "--max-budget-usd"):
			value, err := flagValue(arguments, &index, "--max-budget-usd")
			if err != nil {
				return options{}, err
			}
			budget, err := strconv.ParseFloat(value, 64)
			if err != nil || budget <= 0 {
				return options{}, errors.New("--max-budget-usd must be a positive number")
			}
			parsed.maxBudgetUSD = budget
		case isFlag(argument, "--api-key-helper"):
			value, err := flagValue(arguments, &index, "--api-key-helper")
			if err != nil {
				return options{}, err
			}
			parsed.apiKeyHelper = value
		case isFlag(argument, "--arms-dir"):
			value, err := flagValue(arguments, &index, "--arms-dir")
			if err != nil {
				return options{}, err
			}
			parsed.armsDir = value
		case isFlag(argument, "--skills-repo"):
			value, err := flagValue(arguments, &index, "--skills-repo")
			if err != nil {
				return options{}, err
			}
			parsed.skillsRepo = value
		case isFlag(argument, "--run-number"):
			value, err := flagValue(arguments, &index, "--run-number")
			if err != nil {
				return options{}, err
			}
			if parsed.runNumber, err = strconv.Atoi(value); err != nil || parsed.runNumber < 1 {
				return options{}, errors.New("--run-number must be a positive integer")
			}
		case isFlag(argument, "--requested-runs"):
			value, err := flagValue(arguments, &index, "--requested-runs")
			if err != nil {
				return options{}, err
			}
			if parsed.requestedRuns, err = strconv.Atoi(value); err != nil || parsed.requestedRuns < 1 {
				return options{}, errors.New("--requested-runs must be a positive integer")
			}
		case strings.HasPrefix(argument, "--target="):
			parsed.target = strings.TrimPrefix(argument, "--target=")
		case argument == "--target":
			index++
			if index == len(arguments) {
				return options{}, errors.New("--target requires a value")
			}
			parsed.target = arguments[index]
		case strings.HasPrefix(argument, "-"):
			return options{}, fmt.Errorf("unknown option %q", argument)
		default:
			positionals = append(positionals, argument)
		}
	}
	if len(positionals) != 3 {
		return options{}, errors.New("task, language, and harness are required")
	}
	parsed.task, parsed.language, parsed.harness = positionals[0], positionals[1], positionals[2]
	if parsed.model != "" && parsed.harness != "claude" {
		return options{}, errors.New("--model is only supported by the claude harness")
	}
	if parsed.maxBudgetUSD != 0 && parsed.harness != "claude" {
		return options{}, errors.New("--max-budget-usd is only supported by the claude harness")
	}
	if parsed.runNumber == 0 {
		parsed.runNumber = 1
	}
	if parsed.requestedRuns == 0 {
		parsed.requestedRuns = parsed.runs
	}
	if parsed.runNumber+parsed.runs-1 > parsed.requestedRuns {
		return options{}, errors.New("--run-number plus --n-runs exceeds --requested-runs")
	}
	return parsed, nil
}

func isFlag(argument, name string) bool {
	return argument == name || strings.HasPrefix(argument, name+"=")
}

// flagValue returns the value of a --name=value or --name value argument,
// advancing index past a separate value.
func flagValue(arguments []string, index *int, name string) (string, error) {
	argument := arguments[*index]
	if value, found := strings.CutPrefix(argument, name+"="); found {
		if value == "" {
			return "", fmt.Errorf("%s requires a value", name)
		}
		return value, nil
	}
	*index++
	if *index == len(arguments) || arguments[*index] == "" {
		return "", fmt.Errorf("%s requires a value", name)
	}
	return arguments[*index], nil
}

func allocatePorts(count int) ([]int, error) {
	ports := make([]int, 0, count)
	seen := make(map[int]struct{}, count)
	for len(ports) < count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("allocate port: %w", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			return nil, fmt.Errorf("release port: %w", err)
		}
		if _, duplicate := seen[port]; duplicate {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	return ports, nil
}

func normalizeSkillsDir(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("normalize skills directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect skills directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("skills path %q is not a directory", path)
	}
	return filepath.Clean(absolute), nil
}

func normalizeRunName(value string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsSpace(character) {
			return '_'
		}
		return character
	}, value)
}

func runDirectoryPrefix(language, task, harness string) string {
	return fmt.Sprintf("%s-%s-%s-", normalizeRunName(language), normalizeRunName(task), normalizeRunName(harness))
}

func createRunDirectory(target, language, task, harness string) (string, error) {
	return os.MkdirTemp(target, runDirectoryPrefix(language, task, harness)+"*")
}

func prepareTarget(target string) (string, error) {
	if target == "" {
		created, err := os.MkdirTemp("", "token-bench-")
		if err != nil {
			return "", err
		}
		return created, nil
	}
	absolute, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return "", err
	}
	return absolute, nil
}

func summarize(runs []runResult) (tokenSummary, tokenSummary) {
	inputs := make([]float64, 0, len(runs))
	outputs := make([]float64, 0, len(runs))
	totals := make([]float64, 0, len(runs))
	reads := make([]float64, 0, len(runs))
	writes := make([]float64, 0, len(runs))
	for _, run := range runs {
		inputs = append(inputs, float64(run.Tokens.Input))
		outputs = append(outputs, float64(run.Tokens.Output))
		totals = append(totals, float64(run.Tokens.Total))
		reads = append(reads, float64(run.Tokens.CacheRead))
		writes = append(writes, float64(run.Tokens.CacheWrite))
	}
	mean := tokenSummary{mean(inputs), mean(outputs), mean(totals), mean(reads), mean(writes)}
	median := tokenSummary{median(inputs), median(outputs), median(totals), median(reads), median(writes)}
	return mean, median
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func renderReport(result report, target string) (string, error) {
	var rendered strings.Builder
	writer := tabwriter.NewWriter(&rendered, 0, 4, 2, ' ', 0)
	skills := result.SkillsDir
	if skills == "" {
		skills = "No skills"
	}
	if _, err := fmt.Fprintf(writer, "SKILLS\t%s\n", skills); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintln(writer, "RUN\tPASS\tFIXES\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tTOTAL\tERROR"); err != nil {
		return "", err
	}
	for _, run := range result.Runs {
		if _, err := fmt.Fprintf(writer, "%d\t%t\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", run.Run, run.Passed, run.Corrections, run.Tokens.Input, run.Tokens.Output, run.Tokens.CacheRead, run.Tokens.CacheWrite, run.Tokens.Total, strings.Join(strings.Fields(run.Error), " ")); err != nil {
			return "", err
		}
	}
	if _, err := fmt.Fprintf(writer, "MEAN\t-\t-\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t-\n", result.Mean.Input, result.Mean.Output, result.Mean.CacheRead, result.Mean.CacheWrite, result.Mean.Total); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(writer, "MEDIAN\t-\t-\t%.1f\t%.1f\t%.1f\t%.1f\t%.1f\t-\n", result.Median.Input, result.Median.Output, result.Median.CacheRead, result.Median.CacheWrite, result.Median.Total); err != nil {
		return "", err
	}
	if err := writer.Flush(); err != nil {
		return "", err
	}
	pattern := runDirectoryPrefix(result.Language, result.Task, result.Harness) + "*"
	if _, err := fmt.Fprintln(&rendered, "JSON results:", filepath.Join(target, pattern, "result.json")); err != nil {
		return "", err
	}
	return rendered.String(), nil
}

func joinErrors(current, additional string) string {
	if current == "" {
		return additional
	}
	return current + "; " + additional
}

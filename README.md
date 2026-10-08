# token-bench

`token-bench` benchmarks coding agents by asking them to build integrations from scratch. For each run it creates an isolated project directory, starts the agent harness, checks the generated service, provides corrective feedback when needed, runs hidden validation, and records token usage and results.

## Supported configurations

| Dimension | Values |
| --- | --- |
| Scenarios | `checkout-sync`, `claim-check`, `content-based-router`, `content-enricher`, `dead-letter-channel`, `multi-consumer`, `scatter-gather` |
| Languages | `ballerina`, `go`, `java`, `python` |
| Harnesses | `claude`, `pi` |

## Setup

### Requirements

- Go 1.25.3 or newer
- At least one configured and authenticated coding harness on `PATH`:
  - `claude` for the Claude Code harness
  - `pi` for the Pi harness
- The toolchain for every target language you intend to benchmark:
  - Ballerina: `bal`
  - Go: `go`
  - Java: JDK 21 and `mvn`
  - Python: `uv` with Python 3.12 available
- Docker with a running daemon for scenarios backed by Redis, PostgreSQL, MySQL, RabbitMQ, or Kafka (`checkout-sync`, `claim-check`, `content-enricher`, `dead-letter-channel`, and `multi-consumer`)
- Optional: a local [Arize Phoenix](https://phoenix.arize.com/) instance for tracing

Check that the required commands are available, for example:

```sh
go version
claude --version   # or: pi --version
docker info
```

Download dependencies, run the tests, and build both executables:

```sh
go mod download
go test ./...
make all
```

This creates:

- `./token-bench` — benchmark runner with a terminal UI
- `./token-bench-report` — HTML report generator and CSV exporter
- `./token-bench-matrix` — arm x scenario x run matrix runner

Remove the built executables with:

```sh
make clean
```

## Run the harness

Each benchmark needs a scenario, one or more target languages, a harness, and a directory in which to retain generated projects and result files.

### Make commands

Run a scenario across the default languages (`ballerina go java python`) using Claude Code:

```sh
make run \
  SCENARIO=content-based-router \
  TARGET_DIR=./results
```

Customize the run with Make variables:

```sh
make run \
  SCENARIO=scatter-gather \
  TARGET_DIR=./results \
  LANGUAGES="go python" \
  HARNESS=pi \
  RUNS=1
```

The available variables are:

| Variable | Default | Description |
| --- | --- | --- |
| `SCENARIO` | required | Scenario name from the supported list above |
| `TARGET_DIR` | required | Parent directory for generated projects and results |
| `LANGUAGES` | `ballerina go java python` | Space-separated target languages |
| `HARNESS` | `claude` | `claude` or `pi` |
| `RUNS` | `3` | Number of runs per language |
| `PHOENIX_ENDPOINT` | empty | Optional Phoenix endpoint, such as `http://localhost:6006/` |

For example, enable tracing for a Claude benchmark with:

```sh
make run \
  SCENARIO=content-enricher \
  TARGET_DIR=./results \
  LANGUAGES=go \
  PHOENIX_ENDPOINT=http://localhost:6006/
```

> Phoenix tracing is currently supported by the Claude harness only.

For the full benchmark matrix, use `make bench`:

```sh
make bench \
  SCENARIO=content-based-router \
  TARGET_DIR=./results \
  HARNESS=claude \
  RUNS=3
```

`make bench` runs every selected language without skills, then runs Ballerina again with the project and test skill sets. It expects Phoenix at `http://localhost:6006/` and stores completion markers under `TARGET_DIR/.token-bench-completed`, allowing an interrupted matrix to resume without repeating completed combinations. Its skill locations default to `~/Projects/skills/skills` and `./test-skills`; override them when necessary:

```sh
make bench \
  SCENARIO=content-based-router \
  TARGET_DIR=./results \
  BENCH_PROJECT_SKILLS=/path/to/project-skills \
  BENCH_TEST_SKILLS="$PWD/test-skills"
```

### Direct CLI usage

Build the runner, then invoke it directly:

```sh
make token-bench
./token-bench <task> <language> <harness> [options]
```

A single Go/Pi run looks like this:

```sh
./token-bench content-based-router go pi \
  --n-runs=1 \
  --target=./results
```

The terminal UI displays the agent's current activity while the benchmark is running and replaces it with a result table when all runs finish.

Options:

| Option | Default | Description |
| --- | --- | --- |
| `--n-runs=N` | `1` | Number of independent runs |
| `--target=DIR` | temporary directory | Directory in which generated projects and results are retained |
| `--skills=DIR` | none | Skills directory supplied to the harness |
| `--model=NAME` | `opus` | Claude model alias or id (Claude harness only) |
| `--arm=LABEL` | none | `A`..`E` select an [arm definition](#benchmark-arms-and-the-matrix-runner) from `--arms-dir` and apply it (claude harness only). Any other label is an ad-hoc label: recorded in `result.json` and used for report grouping, with no behavior attached (a note is printed) |
| `--max-budget-usd=N` | `20` | Per-session spend cap passed to Claude Code; recorded in `result.json` (`environment.maxBudgetUsd`) |
| `--arms-dir=DIR` | `./arms` (or `$TOKEN_BENCH_ARMS_DIR`) | Directory of arm definitions |
| `--skills-repo=DIR` | `$TOKEN_BENCH_SKILLS_REPO`, else `~/Projects/Ballerina/cli-improvements/benchmark/reference/ballerina-skills` | Checkout of `ballerina-platform/skills` that arms B, D and E read; never copied into this repository |
| `--run-number=N`, `--requested-runs=N` | `1`, `--n-runs` | Number recorded for the first run and for the cell's run count (the matrix runner sets them) |
| `--phoenix-endpoint=URL` | disabled | Local Phoenix OTLP/HTTP endpoint (Claude only) |

Options also accept a separate value, such as `--target ./results`. Claude loads `--skills` as a temporary plugin; its global configuration may still be discovered. The Pi harness adds the supplied skill directory to Pi's startup configuration.

If `--target` is omitted, the runner creates a temporary directory and prints its result path at completion. To inspect generated code and logs reliably, set an explicit target directory.

## Results and reports

Every run gets its own directory beneath the target and contains a `result.json` plus harness stream and stderr artifacts. The final TUI table reports validation status, corrective feedback count, token usage, and any error.

`result.json` (schema version 5) also records `environment` (`bal version`, the `bal discover` row of `bal tool list`, Claude Code version, whether `bal discover` was reachable, whether the discover cache was cold, skills repository commit and a content hash of the exact skills or plugin tree the agent received, the appended arm prompt, and the budget cap), `arm`, `durationSeconds` (agent start to final validation, including feedback rounds), `costUsd` (estimated with the price table in `pricing/`; `costEstimateBasis` says how: `stream-usage` prices the per-model usage parsed from the stream with cache writes split into 5-minute and 1-hour TTL, `token-totals` prices the token totals at the run's model assuming 5-minute writes, which understates cost because Claude Code mostly writes 1-hour caches), `reportedCostUsd` (Claude Code's own `total_cost_usd`), `infraFailure` and `stopReason` (below) and `toolUsage` (tool-call counts per tool and per family, assistant turns, and per-model usage, parsed from `claude-stream.jsonl` by `streamlog/`; a `bal discover` count in an arm that hides it counts attempts the shim refused). Schema versions 2 to 4 remain readable.

Token and cost accounting. Claude Code reports `total_cost_usd` and `modelUsage` in each `result` event as running totals for the whole session (each correction turn's result includes the earlier turns), so a run records the latest values rather than their sum; per-turn `usage` is summed only when `modelUsage` is absent. This follows the field descriptions in Claude Code 2.1.293's SDK schema.

Infrastructure failures. A run that fails in the benchmark machinery rather than in the agent's work has `infraFailure: true`: port allocation, the task's `Setup` (Docker backends), the task's own checks returning an error, the agent failing to start or its process dying, a turn ended by a model API error (`api_error_status`, or `is_error` on a success result), and Claude Code's `error_during_execution`. Such a run says nothing about the agent: the matrix reruns it and the report leaves it out of pass rates. A spend or turn cap (`error_max_budget_usd`, `error_max_turns`) is an agent outcome: the run fails with `stopReason` set.

Build an HTML report by recursively collecting results from one or more paths:

```sh
make token-bench-report
./token-bench-report --output=report.html ./results
```

Multiple result roots may be supplied:

```sh
./token-bench-report --output=report.html ./results/baseline ./results/with-skills
```

The HTML report starts with an arm x scenario summary table (pass k/n with a 95% Wilson interval, infrastructure failures, median tokens, median cost, median duration, median corrections). Infrastructure failures are counted but are not agent runs, so they are left out of the pass rate and the medians; medians cover all other runs, failed ones included, because a failed run still spent its tokens. Cost is Claude Code's reported `total_cost_usd`, falling back to the estimate. Directories whose name starts with a dot (staged skills, set-aside infrastructure failures) are not searched for results. For Google Sheets, export CSVs:

```sh
./token-bench-report --output=report.html --csv=runs.csv --summary-csv=summary.csv ./results
```

`runs.csv` has one row per run (arm, scenario, run, model, passed, corrections, token dimensions, infrastructure failure and stop reason, estimated cost and its basis, reported cost, duration, tool-call counts for `bal discover`, `bal search`, `bal pull`, web search/fetch, agents, MCP, and the recorded environment). `summary.csv` is the arm x scenario table, including the Wilson interval bounds and the infrastructure-failure count.

## Benchmark arms and the matrix runner

The `bal discover` benchmark compares five arms. Every arm uses the same model, effort, task prompt, Ballerina version and `--setting-sources ""`; each arm is a JSON file under `arms/`.

| Arm | Agent gets | `bal discover` reachable |
| --- | --- | --- |
| A | nothing extra | no |
| B | the `ballerina` skill (`skills/` of the skills repository), as the harness always loaded it | no |
| C | `bal discover` plus a fixed three-line hint appended to the system prompt (`arms/hints/discover-hint.md`) | yes |
| D | the skill with Step 2 (and the other `library`-agent mentions, in Step 4 and `rules/messaging.md`) pointed at `bal discover`, plus `bal discover` | yes |
| E | the whole skills repository as a Claude Code plugin: skill, `library` agent, MCP server, hooks, and its LSP server | no |

An arm definition (`arms/<ID>.json`) has: `label`; `skills` (a directory relative to the skills repository) or `plugin: true` (the repository root as a plugin; the two are exclusive); `skillsPatch` (a patch applied to a staged copy of `skills`); `systemPromptFile` (text appended to the harness system prompt); `discoverVisible`; `env` (extra environment); `skillsRepoCommit` (the commit the arm was designed against; a different checkout prints a warning). `arms/phases.json` holds the phase presets.

How an arm is applied by `token-bench --arm=X`:

- Skills and plugin: arms B and D go through the existing temporary skills plugin; arm E adds `--plugin-dir <skills repository>` as a second plugin directory. The repository is read in place and never modified. Arm D stages a patched copy under `<target>/.token-bench-arms/D/` on every invocation (`arms/patches/ballerina-skill-discover.patch`, made against skills commit `a87ce03`); if the patch does not apply the run stops with an error naming the likely cause.
- Hiding `bal discover` (arms A, B, E): each run gets a fresh directory with a `bal` wrapper that forwards everything to the real `bal` except `bal discover ...` and `bal help discover`, which fail with the real "unknown command" message and exit 1, and the outputs that list installed tools, `bal tool list` and the top-level help (`bal`, `bal help`, `bal --help`, `bal -h`, whose "Tool Commands" section names discover), whose discover row is dropped. The directory is prepended to PATH of the `claude` subprocess only; nothing in `~/.ballerina` or the installed tools is touched. Arms C and D use the unmodified PATH.
- Cold discover cache (all arms): each run gets its own `XDG_CACHE_HOME`, so no run benefits from an earlier run's `bal discover` cache and Central latency stays part of the measured time.
- Everything is recorded per run in `result.json` (`arm`, `environment`).

An `--arm` label with no definition in the arms directory (for example `--arm=baseline-v2`) is not an error: it is recorded and used for grouping, nothing else changes, and a note is printed. `--arm` combined with `--skills` is rejected, and arm definitions require the claude harness.

Arm E limitations. The plugin's `.mcp.json` and hooks use `${CLAUDE_PLUGIN_ROOT}`, which Claude Code expands to the plugin directory, so loading the repository root with `--plugin-dir` is expected to start the `ballerina-library` MCP server (needs `node` on PATH) and the hooks (need `bash` and `jq`). This has been checked statically only (`claude plugin validate`, reading the manifests and the help text), not with a live session. Arm E also exposes the plugin's LSP server (`bal start-language-server`), which arms B and D do not have, so E differs from the others by more than the library lookup. Verify arm E on the first real run: the first line of `claude-stream.jsonl` (`system`/`init`) lists the loaded tools, agents and MCP servers; `ballerina-library` should be connected and the `library` agent present. Also count `bal discover` in `result.json` `toolUsage.families` for arms A, B and E: it must be zero.

### Checking the environment

```sh
make all
./scripts/check-env.sh
```

It verifies `bal` (expects 2201.13.6, warns otherwise), `bal discover --help`, Docker (with the `colima start` hint), Go, `claude --version`, Node 18+ and `jq` for arm E, `patch` for arm D, and the skills repository path and commit, then prints a pinned-versions block to paste into the report. It runs only `--version`-style commands and spends no tokens.

### Running the matrix

`token-bench-matrix` enumerates arm x scenario x run cells, shuffles them with `--seed` so arms interleave in time, skips cells that already have a completion marker (`<target>/.token-bench-completed/<arm>/<scenario>/run-NN.done`), and runs each remaining cell with `./token-bench` (`--n-runs=1`, results under `<target>/<arm>/<scenario>/run-NN/`). A cell is complete once its run wrote a `result.json` that is not an infrastructure failure, whatever the verdict: an agent failure is data and is never rerun automatically. A cell with no result, or whose result has `infraFailure` (for example Docker was down or the API was overloaded), gets no marker and is retried on the next invocation; an infrastructure failure's directory is moved to `<target>/.infra-failures/<arm>/<scenario>/run-NN-<time>/` and logged in `<target>/infra-failures.log`, so reruns are on record. To redo a cell on purpose, delete its marker and directory. A target directory is tied to its model, language and budget cap (`matrix-plan.json`); change them only with a new `--target`.

Dry-run first. It prints the plan, the run count and a worst-case spend (pending runs x `--max-budget-usd`, the cap that applies to each whole run) and runs nothing, writes nothing:

```sh
make all
./token-bench-matrix --phase=phase0 --dry-run --target=./results/phase0
# equivalent, spelled out:
./token-bench-matrix --arms A,D --scenarios checkout-sync --runs 3 --model sonnet --dry-run --target=./results/phase0
```

Phase 0 for real (spends tokens; needs Docker for `checkout-sync`), then the report:

```sh
./token-bench-matrix --phase=phase0 --target=./results/phase0 --seed 1
./token-bench-report --output=phase0.html --csv=phase0-runs.csv --summary-csv=phase0-summary.csv ./results/phase0
```

Presets in `arms/phases.json` (`./token-bench-matrix --list` shows them): `phase0` (A, D; `checkout-sync`; 3 runs; Sonnet), `phase1` (B, C, E on the same scenario), `phase2` (all arms, all existing scenarios, 5 runs; add the connector scenarios once they exist), `phase3` (Opus confirmation). Any explicit flag overrides the preset. Options: `--phase`, `--arms`, `--scenarios`, `--runs`, `--model` (default `sonnet`), `--target`, `--seed` (default 1), `--max-budget-usd` (default 20), `--dry-run`, `--runner`, `--language` (default `ballerina`), `--arms-dir`, `--skills-repo`, `--phoenix-endpoint`, `--list`.

## Adding new scenarios
> In most case you can ask your agent to add the scenario by using the `add-test-scenario`. Just validating the generated test should be enough

- Each test scenario must conform to `Task` and `TaskHandle` interfaces defined in `task.go`
- `Task` defines resources you want to get allocated from the benchmark harness as well as metadata it use to track the scenario
- It will allocated the those resources and pass them in when creating the `TaskHandle`
- Benchmark tool first call `Setup()` method in the `TaskHandle`. You should use this to setup any back-end services such as databases, docker containers etc. You return a tear down function that harness will call after completing a given run
- `Prompt` should then build and return the task specific prompt we provide to the agent to start the turn.
- Once agent has indicated that task is done harness will call `feedback`, while the agent generated code is running. You need to validate agent's implementation using a "visible" (there is a these could be exposed to agent depending on your feedback so it may try to hard code responses) set of test cases and provide feedback that is passed back to the agent.
- If you provide no feedback, we then move to `validation` (you should use a different set of values for this) where you should indicate to harness that task was implemented successfully or not

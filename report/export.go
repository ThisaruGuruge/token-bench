package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	"token-bench/result"
	"token-bench/streamlog"
)

// SummaryRow aggregates every valid run of one arm on one scenario, language
// and model. Medians are over all valid runs, failed ones included, because a
// failed run still spent its tokens, money and time. Cost and duration medians
// skip runs that recorded no value. Infrastructure failures are not runs of the
// agent: they are counted in InfraFailures and left out of everything else.
type SummaryRow struct {
	Arm                   string
	Scenario              string
	Language              string
	Model                 string
	Runs                  int
	Passed                int
	InfraFailures         int
	MedianTokens          float64
	MedianCorrections     float64
	MedianCostUSD         float64
	HasCost               bool
	MedianDurationSeconds float64
	HasDuration           bool
	// MeanPartial is the mean fraction of answer fields (or checks) correct.
	MeanPartial float64
	HasPartial  bool
}

// PassFraction renders "k/n".
func (s SummaryRow) PassFraction() string { return fmt.Sprintf("%d/%d", s.Passed, s.Runs) }

// PassRate is the percentage of runs that passed.
func (s SummaryRow) PassRate() float64 {
	if s.Runs == 0 {
		return 0
	}
	return float64(s.Passed) / float64(s.Runs) * 100
}

// PassInterval is the 95% Wilson score interval of the pass rate, in percent.
func (s SummaryRow) PassInterval() (low, high float64) {
	if s.Runs == 0 {
		return 0, 100
	}
	const z = 1.959963984540054
	n, p := float64(s.Runs), float64(s.Passed)/float64(s.Runs)
	center := (p + z*z/(2*n)) / (1 + z*z/n)
	margin := z / (1 + z*z/n) * math.Sqrt(p*(1-p)/n+z*z/(4*n*n))
	return math.Max(0, center-margin) * 100, math.Min(1, center+margin) * 100
}

// PassIntervalLabel renders the interval as "low-high%".
func (s SummaryRow) PassIntervalLabel() string {
	low, high := s.PassInterval()
	return fmt.Sprintf("%.0f-%.0f%%", low, high)
}

// ArmLabel is the arm shown in tables; "-" when none was recorded.
func (s SummaryRow) ArmLabel() string { return armLabel(s.Arm) }

// runCost is the run's cost for summaries: Claude Code's own total_cost_usd
// when recorded (it covers every API call and knows the cache TTLs), else the
// price-table estimate.
func runCost(run result.Result) (float64, bool) {
	if run.ReportedCostUSD != nil {
		return *run.ReportedCostUSD, true
	}
	if run.CostUSD != nil {
		return *run.CostUSD, true
	}
	return 0, false
}

// summarizeArms builds the arm x scenario table.
func summarizeArms(discovered []discoveredResult) []SummaryRow {
	type key struct{ arm, scenario, language, model string }
	groups := map[key][]result.Result{}
	for _, item := range discovered {
		run := item.value
		k := key{run.Arm, run.Task, run.Language, run.Model}
		groups[k] = append(groups[k], run)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.scenario != b.scenario {
			return a.scenario < b.scenario
		}
		if a.arm != b.arm {
			return a.arm < b.arm
		}
		if a.language != b.language {
			return a.language < b.language
		}
		return a.model < b.model
	})
	rows := make([]SummaryRow, 0, len(keys))
	for _, k := range keys {
		runs := groups[k]
		row := SummaryRow{Arm: k.arm, Scenario: k.scenario, Language: k.language, Model: k.model}
		var tokens, corrections, costs, durations, partials []float64
		for _, run := range runs {
			if run.InfraFailure {
				row.InfraFailures++
				continue
			}
			row.Runs++
			if run.Passed {
				row.Passed++
			}
			if score, ok := partialScore(run); ok {
				partials = append(partials, score)
			}
			tokens = append(tokens, float64(run.Tokens.Total))
			corrections = append(corrections, float64(run.Corrections))
			if cost, ok := runCost(run); ok {
				costs = append(costs, cost)
			}
			if run.DurationSeconds > 0 {
				durations = append(durations, run.DurationSeconds)
			}
		}
		if len(partials) > 0 {
			sum := 0.0
			for _, score := range partials {
				sum += score
			}
			row.MeanPartial, row.HasPartial = sum/float64(len(partials)), true
		}
		row.MedianTokens = medianOf(tokens)
		row.MedianCorrections = medianOf(corrections)
		if len(costs) > 0 {
			row.MedianCostUSD, row.HasCost = medianOf(costs), true
		}
		if len(durations) > 0 {
			row.MedianDurationSeconds, row.HasDuration = medianOf(durations), true
		}
		rows = append(rows, row)
	}
	return rows
}

func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	return quantile(ordered, 0.5)
}

var runColumns = []string{
	"arm", "scenario", "language", "run", "requested_runs", "model", "passed", "corrections",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "total_tokens",
	"infra_failure", "stop_reason",
	"cost_usd", "cost_estimate_basis", "reported_cost_usd", "duration_seconds",
	"assistant_turns", "tool_calls", "bal_discover", "bal_search", "bal_pull", "bal_build", "bal_run",
	"web_search", "web_fetch", "task_agent", "mcp", "ballerina_repo_reads",
	"discover_visible", "bal_version", "bal_discover_version", "max_budget_usd", "error", "result_path",
	"answer_correct", "answer_total", "partial_score",
}

func optionalFloat(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

// WriteRunsCSV writes one row per run, for spreadsheets.
func WriteRunsCSV(paths []string, output io.Writer) error {
	discovered, err := discover(paths)
	if err != nil {
		return err
	}
	return writeRunsCSV(discovered, output)
}

func writeRunsCSV(discovered []discoveredResult, output io.Writer) error {
	writer := csv.NewWriter(output)
	if err := writer.Write(runColumns); err != nil {
		return err
	}
	for _, item := range discovered {
		run := item.value
		families := map[string]int{}
		var turns, calls string
		if run.ToolUsage != nil {
			families = run.ToolUsage.Families
			turns = strconv.Itoa(run.ToolUsage.AssistantTurns)
			calls = strconv.Itoa(run.ToolUsage.ToolCalls)
		}
		family := func(name string) string {
			if run.ToolUsage == nil {
				return ""
			}
			return strconv.Itoa(families[name])
		}
		var visible, balVersion, discoverVersion, budget string
		if run.Environment != nil {
			visible = strconv.FormatBool(run.Environment.DiscoverVisible)
			balVersion, discoverVersion = run.Environment.BalVersion, run.Environment.BalDiscover
			if run.Environment.MaxBudgetUSD > 0 {
				budget = strconv.FormatFloat(run.Environment.MaxBudgetUSD, 'f', -1, 64)
			}
		}
		duration := ""
		if run.DurationSeconds > 0 {
			duration = strconv.FormatFloat(run.DurationSeconds, 'f', 1, 64)
		}
		record := []string{
			run.Arm, run.Task, run.Language, strconv.Itoa(run.Run), strconv.Itoa(run.RequestedRuns), run.Model,
			strconv.FormatBool(run.Passed), strconv.Itoa(run.Corrections),
			strconv.Itoa(run.Tokens.Input), strconv.Itoa(run.Tokens.Output), strconv.Itoa(run.Tokens.CacheRead),
			strconv.Itoa(run.Tokens.CacheWrite), strconv.Itoa(run.Tokens.Total),
			strconv.FormatBool(run.InfraFailure), run.StopReason,
			optionalFloat(run.CostUSD), run.CostEstimateBasis, optionalFloat(run.ReportedCostUSD), duration,
			turns, calls,
			family(streamlog.FamilyBalDiscover), family(streamlog.FamilyBalSearch), family(streamlog.FamilyBalPull),
			family(streamlog.FamilyBalBuild), family(streamlog.FamilyBalRun),
			family(streamlog.FamilyWebSearch), family(streamlog.FamilyWebFetch), family(streamlog.FamilyAgent),
			family(streamlog.FamilyMCP), family(streamlog.FamilyBalRepoRead),
			visible, balVersion, discoverVersion, budget, run.Error, item.path,
			answerCorrect(run), answerTotal(run), partialText(partialScore(run)),
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

var summaryColumns = []string{
	"arm", "scenario", "language", "model", "runs", "passed", "pass", "pass_rate_percent", "mean_partial_score",
	"pass_rate_ci95_low_percent", "pass_rate_ci95_high_percent", "infra_failures",
	"median_total_tokens", "median_cost_usd", "median_duration_seconds", "median_corrections",
}

// WriteSummaryCSV writes the arm x scenario summary.
func partialScore(run result.Result) (float64, bool) {
	if run.Answer == nil || run.Answer.Total == 0 {
		return 0, false
	}
	return float64(run.Answer.Correct) / float64(run.Answer.Total), true
}

func partialText(value float64, ok bool) string {
	if !ok {
		return ""
	}
	return strconv.FormatFloat(value, 'f', 3, 64)
}

func answerCorrect(run result.Result) string {
	if run.Answer == nil {
		return ""
	}
	return strconv.Itoa(run.Answer.Correct)
}

func answerTotal(run result.Result) string {
	if run.Answer == nil {
		return ""
	}
	return strconv.Itoa(run.Answer.Total)
}

func WriteSummaryCSV(paths []string, output io.Writer) error {
	discovered, err := discover(paths)
	if err != nil {
		return err
	}
	return writeSummaryCSV(summarizeArms(discovered), output)
}

func writeSummaryCSV(rows []SummaryRow, output io.Writer) error {
	writer := csv.NewWriter(output)
	if err := writer.Write(summaryColumns); err != nil {
		return err
	}
	for _, row := range rows {
		cost, duration := "", ""
		if row.HasCost {
			cost = strconv.FormatFloat(row.MedianCostUSD, 'f', 4, 64)
		}
		if row.HasDuration {
			duration = strconv.FormatFloat(row.MedianDurationSeconds, 'f', 1, 64)
		}
		low, high := row.PassInterval()
		record := []string{
			row.Arm, row.Scenario, row.Language, row.Model, strconv.Itoa(row.Runs), strconv.Itoa(row.Passed), row.PassFraction(),
			strconv.FormatFloat(row.PassRate(), 'f', 1, 64), partialText(row.MeanPartial, row.HasPartial),
			strconv.FormatFloat(low, 'f', 1, 64), strconv.FormatFloat(high, 'f', 1, 64), strconv.Itoa(row.InfraFailures),
			strconv.FormatFloat(row.MedianTokens, 'f', 0, 64), cost, duration,
			strconv.FormatFloat(row.MedianCorrections, 'f', 1, 64),
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

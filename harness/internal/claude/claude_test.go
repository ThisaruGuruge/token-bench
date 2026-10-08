package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"token-bench/harness/internal/shared"
)

func TestReportsConfiguredModelAndThinkingBudget(t *testing.T) {
	agent := New(Config{})
	if agent.Model() != "opus" || agent.ThinkingBudget() != "high" {
		t.Fatalf("unexpected metadata: model=%q thinkingBudget=%q", agent.Model(), agent.ThinkingBudget())
	}
}

func TestModelOverrideIsReportedAndPassedToClaude(t *testing.T) {
	agent := New(Config{Model: "sonnet"})
	if agent.Model() != "sonnet" {
		t.Fatalf("unexpected model %q", agent.Model())
	}
	arguments := agent.startArguments()
	if arguments[0] != "--model" || arguments[1] != "sonnet" {
		t.Fatalf("model was not passed to claude: %#v", arguments[:2])
	}
}

func TestStartArgumentsUseStreamingPrintMode(t *testing.T) {
	agent := New(Config{})
	agent.pluginPath = "/tmp/plugin with spaces"

	expected := []string{
		"--model", "opus",
		"--effort", "high",
		"--dangerously-skip-permissions",
		"--disallowedTools", "AskUserQuestion",
		"--append-system-prompt", "Do not ask clarifying questions. Make reasonable assumptions and state them in your final output.",
		"--max-budget-usd", "20",
		"--setting-sources", "",
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--plugin-dir", "/tmp/plugin with spaces",
	}
	if actual := agent.startArguments(); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("unexpected Claude arguments:\nwant: %#v\n got: %#v", expected, actual)
	}
}

func TestStreamingProcessTreatsEveryResultAsCompletionAndPersistsLogs(t *testing.T) {
	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
if [ "$CLAUDE_CODE_DISABLE_AUTO_MEMORY" != "1" ]; then
  printf '%s\n' 'auto-memory was not disabled' >&2
  exit 42
fi
turn=0
while IFS= read -r message; do
  turn=$((turn + 1))
  if [ "$turn" -eq 1 ]; then
    printf '%s\n' '{"type":"system","subtype":"init"}'
  fi
  if [ "$turn" -eq 1 ]; then
    printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"reply-$turn\",\"origin\":{\"kind\":\"task-notification\"},\"modelUsage\":{\"main\":{\"inputTokens\":3,\"outputTokens\":5,\"cacheReadInputTokens\":7,\"cacheCreationInputTokens\":11},\"subagent\":{\"inputTokens\":13,\"outputTokens\":17,\"cacheReadInputTokens\":19,\"cacheCreationInputTokens\":23}}}"
  else
    printf '%s\n' "{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"errors\":[\"failed\"],\"result\":\"reply-$turn\",\"modelUsage\":{\"main\":{\"inputTokens\":3,\"outputTokens\":5,\"cacheReadInputTokens\":7,\"cacheCreationInputTokens\":11},\"subagent\":{\"inputTokens\":13,\"outputTokens\":17,\"cacheReadInputTokens\":19,\"cacheCreationInputTokens\":23}}}"
  fi
done
printf '%s\n' 'fake Claude closed' >&2
`
	if err := os.WriteFile(claudePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workingDir := t.TempDir()
	agent := New(Config{})
	if err := agent.Start(workingDir, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{streamArtifactName, stderrArtifactName} {
		if _, err := os.Stat(filepath.Join(workingDir, name)); !os.IsNotExist(err) {
			t.Fatalf("artifact %q became visible before Claude exited: %v", name, err)
		}
	}

	for index, message := range []string{"initial prompt", "correction"} {
		reply, err := agent.SendMessage(message)
		// The second result is error_during_execution: it still completes the
		// turn, but as an infrastructure failure.
		if index == 0 && err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			var infra *shared.InfraError
			if !errors.As(err, &infra) {
				t.Fatalf("error_during_execution must be an infrastructure error, got %v", err)
			}
		}
		expected := "reply-" + string(rune('1'+index))
		if reply != expected {
			t.Fatalf("unexpected reply %q, want %q", reply, expected)
		}
	}
	usage, err := agent.TokenCount()
	if err != nil {
		t.Fatal(err)
	}
	// modelUsage is a running total for the process, so the second result's
	// identical totals mean no new usage, not a second copy of it.
	if usage.Input != 16 || usage.Output != 22 || usage.CacheRead != 26 || usage.CacheWrite != 34 || usage.Total != 98 {
		t.Fatalf("unexpected subagent-inclusive usage: %+v", usage)
	}
	if err := agent.End(); err != nil {
		t.Fatal(err)
	}

	stream, err := os.ReadFile(filepath.Join(workingDir, streamArtifactName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(stream), `"type":"result"`) != 2 || !strings.Contains(string(stream), `"subagent"`) {
		t.Fatalf("unexpected persisted stream: %s", stream)
	}
	stderr, err := os.ReadFile(filepath.Join(workingDir, stderrArtifactName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stderr), "fake Claude closed") {
		t.Fatalf("unexpected persisted stderr: %s", stderr)
	}
}

func TestResultUsageFallsBackWhenModelUsageIsUnavailable(t *testing.T) {
	agent := New(Config{})
	agent.addUsage(resultEvent{Usage: resultUsage{Input: 2, Output: 3, CacheRead: 5, CacheWrite: 7}})
	if agent.tokens != (TokenCount{Input: 2, Output: 3, CacheRead: 5, CacheWrite: 7, Total: 17}) {
		t.Fatalf("unexpected fallback usage: %+v", agent.tokens)
	}
}

func TestPerTurnUsageIsSummedWithoutModelUsage(t *testing.T) {
	agent := New(Config{})
	agent.addUsage(resultEvent{Usage: resultUsage{Input: 2, Output: 3}})
	agent.addUsage(resultEvent{Usage: resultUsage{Input: 1, Output: 1}})
	if agent.tokens != (TokenCount{Input: 3, Output: 4, Total: 7}) {
		t.Fatalf("unexpected per-turn usage: %+v", agent.tokens)
	}
}

func decodeResult(t *testing.T, raw string) resultEvent {
	t.Helper()
	var event resultEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	return event
}

// Claude Code reports total_cost_usd and modelUsage as running totals in a
// streaming-input session, so a correction turn must not count the first turn
// twice.
func TestCumulativeResultTotalsAreNotSummed(t *testing.T) {
	agent := New(Config{})
	if _, ok := agent.ReportedCostUSD(); ok {
		t.Fatal("no cost expected before any result")
	}
	agent.addUsage(resultEvent{})
	if _, ok := agent.ReportedCostUSD(); ok {
		t.Fatal("a result without total_cost_usd must not report a cost")
	}
	agent.addUsage(decodeResult(t, `{"type":"result","total_cost_usd":0.25,
		"usage":{"input_tokens":10,"output_tokens":20},
		"modelUsage":{"claude-sonnet-5-5":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":100,"cacheCreationInputTokens":50}}}`))
	agent.addUsage(decodeResult(t, `{"type":"result","total_cost_usd":0.5,
		"usage":{"input_tokens":4,"output_tokens":6},
		"modelUsage":{"claude-sonnet-5-5":{"inputTokens":14,"outputTokens":26,"cacheReadInputTokens":300,"cacheCreationInputTokens":60},
		"claude-haiku-5-5":{"inputTokens":1,"outputTokens":2}}}`))
	if cost, ok := agent.ReportedCostUSD(); !ok || cost != 0.5 {
		t.Fatalf("cost = %v, %v; want 0.5, true", cost, ok)
	}
	want := TokenCount{Input: 15, Output: 28, CacheRead: 300, CacheWrite: 60, Total: 403}
	if agent.tokens != want {
		t.Fatalf("tokens = %+v; want %+v", agent.tokens, want)
	}

	// A crash result with zeroed totals must not wipe what was measured.
	agent.addUsage(decodeResult(t, `{"type":"result","subtype":"error_during_execution","total_cost_usd":0,
		"modelUsage":{"claude-sonnet-5-5":{}}}`))
	if cost, _ := agent.ReportedCostUSD(); cost != 0.5 {
		t.Fatalf("cost after zeroed result = %v; want 0.5", cost)
	}
	if agent.tokens != want {
		t.Fatalf("tokens after zeroed result = %+v; want %+v", agent.tokens, want)
	}
}

func TestCreateSkillsPluginSupportsSkillCollections(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"review", "testing"} {
		directory := filepath.Join(root, name)
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	agent := New(Config{SkillsDir: root})
	if err := agent.createSkillsPlugin(); err != nil {
		t.Fatal(err)
	}
	defer agent.cleanupPlugin()
	for _, name := range []string{"review", "testing"} {
		path := filepath.Join(agent.pluginPath, "skills", name)
		if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("skill %q was not staged as a symlink: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(agent.pluginPath, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("plugin manifest was not created: %v", err)
	}
}

func TestCreateSkillsPluginSupportsSingleSkillDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: single\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := New(Config{SkillsDir: root})
	if err := agent.createSkillsPlugin(); err != nil {
		t.Fatal(err)
	}
	defer agent.cleanupPlugin()
	entries, err := os.ReadDir(filepath.Join(agent.pluginPath, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one staged skill, got %d", len(entries))
	}
}

func TestArmOptionsReachTheClaudeArguments(t *testing.T) {
	agent := New(Config{Model: "sonnet", PluginDir: "/skills repo", ExtraSystemPrompt: "\nhint line\n", MaxBudgetUSD: 7.5})
	agent.pluginPath = "/tmp/skills-plugin"
	arguments := agent.startArguments()
	index := func(flag string) int {
		for i, argument := range arguments {
			if argument == flag {
				return i
			}
		}
		t.Fatalf("%s missing from %#v", flag, arguments)
		return -1
	}
	if got := arguments[index("--max-budget-usd")+1]; got != "7.5" {
		t.Errorf("budget = %q", got)
	}
	wantPrompt := "Do not ask clarifying questions. Make reasonable assumptions and state them in your final output.\n\nhint line"
	if got := arguments[index("--append-system-prompt")+1]; got != wantPrompt {
		t.Errorf("system prompt = %q", got)
	}
	var plugins []string
	for i, argument := range arguments {
		if argument == "--plugin-dir" {
			plugins = append(plugins, arguments[i+1])
		}
	}
	if !reflect.DeepEqual(plugins, []string{"/tmp/skills-plugin", "/skills repo"}) {
		t.Errorf("plugin dirs = %#v", plugins)
	}
	if got := New(Config{}).config.MaxBudgetUSD; got != 20 {
		t.Errorf("default budget = %v", got)
	}
}

func TestExternalPluginDirIsNeverRemoved(t *testing.T) {
	plugin := t.TempDir()
	if err := os.WriteFile(filepath.Join(plugin, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	agent := New(Config{PluginDir: plugin})
	if err := agent.End(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plugin, "keep")); err != nil {
		t.Fatalf("external plugin directory was modified: %v", err)
	}
}

func TestEnvAndArgumentsAreAppliedToTheProcess(t *testing.T) {
	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "record")
	script := "#!/bin/sh\nprintf 'cache=%s\\nmarker=%s\\nargs=%s\\n' \"$XDG_CACHE_HOME\" \"$ARM_MARKER\" \"$*\" > '" + record + "'\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", "/from/parent")

	agent := New(Config{Env: []string{"XDG_CACHE_HOME=/per/run", "ARM_MARKER=on"}, PluginDir: "/plugin"})
	if err := agent.Start(t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	_ = agent.End()
	content, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cache=/per/run", "marker=on", "--plugin-dir /plugin"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("process did not see %q:\n%s", want, content)
		}
	}
}

func TestResultErrorClassification(t *testing.T) {
	tests := []struct {
		raw   string
		infra bool
		stop  string
	}{
		{`{"type":"result","subtype":"success","result":"done"}`, false, ""},
		{`{"type":"result","subtype":"success","is_error":true,"api_error_status":529,"result":"API Error: 529 overloaded"}`, true, ""},
		{`{"type":"result","subtype":"success","is_error":true,"result":"API Error: auth"}`, true, ""},
		{`{"type":"result","subtype":"error_during_execution","is_error":true}`, true, ""},
		{`{"type":"result","subtype":"error_max_budget_usd","is_error":true}`, false, "error_max_budget_usd"},
		{`{"type":"result","subtype":"error_max_turns","is_error":true}`, false, "error_max_turns"},
	}
	for _, test := range tests {
		err := resultError(decodeResult(t, test.raw))
		var infra *shared.InfraError
		var stop *shared.StopError
		gotStop := ""
		if errors.As(err, &stop) {
			gotStop = stop.Reason
		}
		if errors.As(err, &infra) != test.infra || gotStop != test.stop {
			t.Errorf("%s: got %v, want infra=%v stop=%q", test.raw, err, test.infra, test.stop)
		}
	}
}

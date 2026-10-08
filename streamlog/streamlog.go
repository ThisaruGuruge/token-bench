// Package streamlog summarizes tool usage from a Claude Code stream-json log.
package streamlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// Family keys reported in Summary.Families.
const (
	FamilyBalDiscover = "bal discover"
	FamilyBalSearch   = "bal search"
	FamilyBalPull     = "bal pull"
	FamilyBalBuild    = "bal build"
	FamilyBalRun      = "bal run"
	FamilyBalOther    = "bal other"
	FamilyWebSearch   = "WebSearch"
	FamilyWebFetch    = "WebFetch"
	FamilyAgent       = "Task/Agent"
	FamilyMCP         = "MCP"
	FamilyBalRepoRead = "ballerina repositories read"
)

const balRepositories = ".ballerina/repositories"

// Summary is the tool usage observed in one stream log.
type Summary struct {
	// AssistantTurns counts distinct assistant messages, including subagents.
	AssistantTurns int `json:"assistantTurns"`
	// SubagentTurns is the part of AssistantTurns emitted inside a subagent.
	SubagentTurns int `json:"subagentTurns"`
	ToolCalls     int `json:"toolCalls"`
	// Tools counts tool_use blocks by tool name (MCP tools by full name).
	Tools map[string]int `json:"tools"`
	// Families counts tool calls by family. A Bash call counts once per bal
	// family it invokes; Read, Grep and Glob count under FamilyBalRepoRead when
	// they target a path under ~/.ballerina/repositories.
	Families map[string]int `json:"families"`
	// ResultCostsUSD lists total_cost_usd from each result event, in order.
	// Claude Code reports it as a running total, so the last value is the run's.
	ResultCostsUSD []float64 `json:"resultCostsUsd,omitempty"`
	// Models is token usage per model id from the assistant messages in the
	// stream (main loop and subagents), with cache writes split by TTL. It
	// misses calls Claude Code does not stream, such as compaction, so it can be
	// slightly below the result's modelUsage totals.
	Models map[string]ModelUsage `json:"models,omitempty"`
	// MalformedLines counts log lines that were not valid JSON.
	MalformedLines int `json:"malformedLines,omitempty"`
}

// ModelUsage is the usage of one model. CacheWriteUnsplit counts cache writes
// whose message reported no TTL breakdown.
type ModelUsage struct {
	Messages          int `json:"messages"`
	Input             int `json:"input"`
	Output            int `json:"output"`
	CacheRead         int `json:"cacheRead"`
	CacheWrite5m      int `json:"cacheWrite5m"`
	CacheWrite1h      int `json:"cacheWrite1h"`
	CacheWriteUnsplit int `json:"cacheWriteUnsplit,omitempty"`
}

type messageUsage struct {
	Input         int `json:"input_tokens"`
	Output        int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheWrite    int `json:"cache_creation_input_tokens"`
	CacheCreation *struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// usage converts one message's usage, attributing writes without a TTL
// breakdown (or not covered by it) to CacheWriteUnsplit.
func (u messageUsage) usage() ModelUsage {
	converted := ModelUsage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead}
	if u.CacheCreation != nil {
		converted.CacheWrite5m = u.CacheCreation.Ephemeral5m
		converted.CacheWrite1h = u.CacheCreation.Ephemeral1h
	}
	if rest := u.CacheWrite - converted.CacheWrite5m - converted.CacheWrite1h; rest > 0 {
		converted.CacheWriteUnsplit = rest
	}
	return converted
}

// maxUsage keeps the larger of each field: the events of one message may carry
// usage snapshots taken while it was still streaming.
func maxUsage(a, b ModelUsage) ModelUsage {
	return ModelUsage{
		Input: max(a.Input, b.Input), Output: max(a.Output, b.Output), CacheRead: max(a.CacheRead, b.CacheRead),
		CacheWrite5m: max(a.CacheWrite5m, b.CacheWrite5m), CacheWrite1h: max(a.CacheWrite1h, b.CacheWrite1h),
		CacheWriteUnsplit: max(a.CacheWriteUnsplit, b.CacheWriteUnsplit),
	}
}

type messageKey struct{ model, id string }

type event struct {
	Type            string   `json:"type"`
	ParentToolUseID *string  `json:"parent_tool_use_id"`
	TotalCostUSD    *float64 `json:"total_cost_usd"`
	Message         struct {
		ID      string        `json:"id"`
		Model   string        `json:"model"`
		Usage   *messageUsage `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			ID    string          `json:"id"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// AnalyzeFile summarizes the stream log at path.
func AnalyzeFile(path string) (Summary, error) {
	file, err := os.Open(path)
	if err != nil {
		return Summary{}, err
	}
	defer file.Close()
	return Analyze(file)
}

// Analyze summarizes a stream-json log. Invalid lines are skipped and counted.
func Analyze(reader io.Reader) (Summary, error) {
	summary := Summary{Tools: map[string]int{}, Families: map[string]int{}}
	buffered := bufio.NewReader(reader)
	seenTurns := make(map[string]struct{})
	seenTools := make(map[string]struct{})
	messages := make(map[messageKey]ModelUsage)
	anonymousTurns := 0
	for {
		line, err := buffered.ReadBytes('\n')
		if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
			var current event
			if json.Unmarshal([]byte(trimmed), &current) != nil {
				summary.MalformedLines++
			} else {
				summary.add(current, seenTurns, seenTools, &anonymousTurns)
				addMessageUsage(current, messages)
			}
		}
		if err != nil {
			summary.Models = modelTotals(messages)
			if errors.Is(err, io.EOF) {
				return summary, nil
			}
			return summary, err
		}
	}
}

// syntheticModel marks messages Claude Code makes up locally (no API call).
const syntheticModel = "<synthetic>"

func addMessageUsage(current event, messages map[messageKey]ModelUsage) {
	if current.Type != "assistant" || current.Message.Usage == nil || current.Message.Model == "" ||
		current.Message.Model == syntheticModel || current.Message.ID == "" {
		return
	}
	key := messageKey{current.Message.Model, current.Message.ID}
	messages[key] = maxUsage(messages[key], current.Message.Usage.usage())
}

func modelTotals(messages map[messageKey]ModelUsage) map[string]ModelUsage {
	if len(messages) == 0 {
		return nil
	}
	totals := make(map[string]ModelUsage)
	for key, usage := range messages {
		total := totals[key.model]
		total.Messages++
		total.Input += usage.Input
		total.Output += usage.Output
		total.CacheRead += usage.CacheRead
		total.CacheWrite5m += usage.CacheWrite5m
		total.CacheWrite1h += usage.CacheWrite1h
		total.CacheWriteUnsplit += usage.CacheWriteUnsplit
		totals[key.model] = total
	}
	return totals
}

func (s *Summary) add(current event, seenTurns, seenTools map[string]struct{}, anonymousTurns *int) {
	switch current.Type {
	case "result":
		if current.TotalCostUSD != nil {
			s.ResultCostsUSD = append(s.ResultCostsUSD, *current.TotalCostUSD)
		}
	case "assistant":
		// Claude Code emits one assistant event per content block, all sharing
		// the message id, so turns are counted by distinct id.
		key := current.Message.ID
		if key == "" {
			*anonymousTurns++
			key = "anonymous-" + strconv.Itoa(*anonymousTurns)
		}
		if _, seen := seenTurns[key]; !seen {
			seenTurns[key] = struct{}{}
			s.AssistantTurns++
			if current.ParentToolUseID != nil {
				s.SubagentTurns++
			}
		}
		for _, block := range current.Message.Content {
			if block.Type != "tool_use" {
				continue
			}
			if block.ID != "" {
				if _, seen := seenTools[block.ID]; seen {
					continue
				}
				seenTools[block.ID] = struct{}{}
			}
			s.ToolCalls++
			s.Tools[block.Name]++
			for _, family := range classify(block.Name, block.Input) {
				s.Families[family]++
			}
		}
	}
}

func classify(name string, input json.RawMessage) []string {
	var arguments map[string]any
	_ = json.Unmarshal(input, &arguments)
	text := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := arguments[key].(string); ok && value != "" {
				return value
			}
		}
		return ""
	}
	switch {
	case name == "Bash":
		return bashFamilies(text("command"))
	case name == "WebSearch":
		return []string{FamilyWebSearch}
	case name == "WebFetch":
		return []string{FamilyWebFetch}
	case name == "Task" || name == "Agent":
		return []string{FamilyAgent}
	case strings.HasPrefix(name, "mcp__"):
		return []string{FamilyMCP}
	case name == "Read" || name == "Grep" || name == "Glob":
		for _, value := range []string{text("file_path"), text("path"), text("pattern")} {
			if strings.Contains(value, balRepositories) {
				return []string{FamilyBalRepoRead}
			}
		}
	}
	return nil
}

var balFamilies = map[string]string{
	"discover": FamilyBalDiscover,
	"search":   FamilyBalSearch,
	"pull":     FamilyBalPull,
	"build":    FamilyBalBuild,
	"run":      FamilyBalRun,
}

// bashFamilies returns the distinct bal families invoked by a shell command
// line. Segments are split on shell separators; leading VAR=value
// assignments, a path prefix on bal, quotes, and wrappers such as
// `bash -c '...'`, `timeout 60` or `env` are looked through.
func bashFamilies(command string) []string {
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '|' || r == '&' || r == '\n' || r == '(' || r == ')'
	})
	var families []string
	seen := make(map[string]struct{})
	for _, segment := range segments {
		family, ok := balFamily(strings.Fields(segment))
		if !ok {
			continue
		}
		if _, duplicate := seen[family]; !duplicate {
			seen[family] = struct{}{}
			families = append(families, family)
		}
	}
	return families
}

// wrappers run the command that follows their own options; the value is the
// number of positional arguments the wrapper itself takes (timeout's duration).
var wrappers = map[string]int{"env": 0, "timeout": 1, "time": 0, "nohup": 0, "command": 0, "exec": 0, "nice": 0}

var shells = map[string]bool{"bash": true, "sh": true, "zsh": true}

// balFamily classifies one simple command given as fields.
func balFamily(fields []string) (string, bool) {
	for {
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			return "", false
		}
		program := strings.Trim(fields[0], `"'`)
		if index := strings.LastIndex(program, "/"); index >= 0 {
			program = program[index+1:]
		}
		if positional, ok := wrappers[program]; ok {
			fields = fields[1:]
			for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
				fields = fields[1:]
			}
			for ; positional > 0 && len(fields) > 0; positional-- {
				fields = fields[1:]
			}
			continue
		}
		if shells[program] {
			// Only `<shell> -c <script>` (or -lc, -ec, ...) runs an inline script.
			rest := fields[1:]
			for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && !strings.Contains(rest[0], "c") {
				rest = rest[1:]
			}
			if len(rest) < 2 || !strings.HasPrefix(rest[0], "-") {
				return "", false
			}
			fields = rest[1:]
			continue
		}
		if program != "bal" {
			return "", false
		}
		family := FamilyBalOther
		for _, field := range fields[1:] {
			field = strings.Trim(field, `"'`)
			if strings.HasPrefix(field, "-") {
				continue
			}
			if known, ok := balFamilies[field]; ok {
				family = known
			}
			break
		}
		return family, true
	}
}

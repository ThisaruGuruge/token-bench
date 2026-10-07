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
	ResultCostsUSD []float64 `json:"resultCostsUsd,omitempty"`
	// MalformedLines counts log lines that were not valid JSON.
	MalformedLines int `json:"malformedLines,omitempty"`
}

type event struct {
	Type            string   `json:"type"`
	ParentToolUseID *string  `json:"parent_tool_use_id"`
	TotalCostUSD    *float64 `json:"total_cost_usd"`
	Message         struct {
		ID      string `json:"id"`
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
	anonymousTurns := 0
	for {
		line, err := buffered.ReadBytes('\n')
		if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
			var current event
			if json.Unmarshal([]byte(trimmed), &current) != nil {
				summary.MalformedLines++
			} else {
				summary.add(current, seenTurns, seenTools, &anonymousTurns)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return summary, nil
			}
			return summary, err
		}
	}
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
// line. Segments are split on shell separators; a leading VAR=value
// assignments and a path prefix on bal are ignored.
func bashFamilies(command string) []string {
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '|' || r == '&' || r == '\n' || r == '(' || r == ')'
	})
	var families []string
	seen := make(map[string]struct{})
	for _, segment := range segments {
		fields := strings.Fields(segment)
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		program := fields[0]
		if index := strings.LastIndex(program, "/"); index >= 0 {
			program = program[index+1:]
		}
		if program != "bal" {
			continue
		}
		family := FamilyBalOther
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "-") {
				continue
			}
			if known, ok := balFamilies[field]; ok {
				family = known
			}
			break
		}
		if _, duplicate := seen[family]; !duplicate {
			seen[family] = struct{}{}
			families = append(families, family)
		}
	}
	return families
}

// Package result defines the persisted result of one benchmark run.
package result

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"token-bench/calllog"
	"token-bench/streamlog"
	"token-bench/task/answer"
)

const (
	SchemaVersion         = 6
	schemaVersion5        = 5
	schemaVersion4        = 4
	previousSchemaVersion = 3
	legacySchemaVersion   = 2
	unknownModel          = "unknown-model"
	unknownThinking       = "unknown-thinking"
)

// Cost estimate bases recorded in Result.CostEstimateBasis.
const (
	// CostBasisStreamUsage prices the per-model usage parsed from the stream
	// log, with cache writes split by TTL.
	CostBasisStreamUsage = "stream-usage"
	// CostBasisTokenTotals prices Tokens at the run's model, assuming 5-minute
	// cache writes; it understates cost when Claude Code wrote 1-hour caches.
	CostBasisTokenTotals = "token-totals"
)

// TokenCount contains the token usage dimensions recorded for a run.
type TokenCount struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Total      int `json:"total"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// Environment records the toolchain and arm setup a run was executed with, so
// results stay interpretable after the machine changes.
type Environment struct {
	BalVersion string `json:"balVersion,omitempty"`
	// BalDiscover is the discover row of `bal tool list` ("not installed" when absent),
	// whether or not the arm could see it.
	BalDiscover   string `json:"balDiscover,omitempty"`
	ClaudeVersion string `json:"claudeVersion,omitempty"`
	// DiscoverVisible is whether `bal discover` was reachable by the agent.
	DiscoverVisible bool `json:"discoverVisible"`
	// ColdDiscoverCache is true when the run had a private, empty XDG cache.
	ColdDiscoverCache bool `json:"coldDiscoverCache"`
	// SkillsRepoCommit is the skills repository commit found; SkillsHash hashes
	// the exact skills or plugin tree (after any patch) the agent received.
	SkillsRepoCommit string `json:"skillsRepoCommit,omitempty"`
	SkillsHash       string `json:"skillsHash,omitempty"`
	// ExtraSystemPrompt is the arm text appended to the base system prompt.
	ExtraSystemPrompt string `json:"extraSystemPrompt,omitempty"`
	// MaxBudgetUSD is the per-run cap handed to the harness.
	MaxBudgetUSD float64 `json:"maxBudgetUsd,omitempty"`
	// Credentials names the credential source (never the credential):
	// "apiKeyHelper: <command>" or "inherited".
	Credentials string `json:"credentials,omitempty"`
	// Container records the image an arm ran in (schema 6), when it ran in one.
	Container *Container `json:"container,omitempty"`
	// CacheSeed records the discover cache seed copied into the run (schema 6).
	CacheSeed *CacheSeed `json:"cacheSeed,omitempty"`
	// DisallowedTools lists the tools denied to the agent beyond the harness default.
	DisallowedTools []string `json:"disallowedTools,omitempty"`
}

// Container identifies the image a run's agent ran in.
type Container struct {
	Image    string `json:"image"`
	ImageID  string `json:"imageId,omitempty"`
	Platform string `json:"platform,omitempty"`
	// Labels are the image's org.token-bench.* labels (branch commit, bala hash, versions).
	Labels map[string]string `json:"labels,omitempty"`
}

// CacheSeed identifies the discover cache seed a run started from.
type CacheSeed struct {
	Dir   string `json:"dir"`
	Hash  string `json:"hash"`
	Files int    `json:"files"`
}

// Result is the persisted outcome and normalized CLI configuration for one run.
type Result struct {
	SchemaVersion  int        `json:"schemaVersion"`
	Task           string     `json:"task"`
	Language       string     `json:"language"`
	Harness        string     `json:"harness"`
	Arm            string     `json:"arm,omitempty"`
	Model          string     `json:"model"`
	ThinkingBudget string     `json:"thinkingBudget"`
	SkillsDir      string     `json:"skillsDir,omitempty"`
	PromptTemplate string     `json:"promptTemplate,omitempty"`
	Run            int        `json:"run"`
	RequestedRuns  int        `json:"requestedRuns"`
	Passed         bool       `json:"passed"`
	Corrections    int        `json:"corrections"`
	Tokens         TokenCount `json:"tokens"`
	// DurationSeconds is the wall-clock time from agent start to the end of the
	// final validation, including every feedback round. Absent in older results.
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	// CostUSD is estimated with the repository price table, on the basis named
	// by CostEstimateBasis.
	CostUSD           *float64 `json:"costUsd,omitempty"`
	CostEstimateBasis string   `json:"costEstimateBasis,omitempty"`
	// ReportedCostUSD is the cost reported by the harness itself (Claude Code's
	// total_cost_usd), when available.
	ReportedCostUSD *float64 `json:"reportedCostUsd,omitempty"`
	// ToolUsage summarizes tool calls parsed from the harness stream log.
	ToolUsage *streamlog.Summary `json:"toolUsage,omitempty"`
	// Answer is the field-by-field grade of an answer task (schema 6).
	Answer *answer.Grade `json:"answer,omitempty"`
	// Discover summarizes the `bal discover` call log of an arm that logs calls (schema 6).
	Discover *calllog.Summary `json:"discover,omitempty"`
	// Environment records the toolchain and arm setup; absent in older results.
	Environment *Environment `json:"environment,omitempty"`
	Error       string       `json:"error,omitempty"`
	// InfraFailure marks a run that failed in the benchmark machinery (ports,
	// Docker backends, the task's checks, the harness process or the model
	// API) rather than in the agent's work. Such a run is invalid: it is left
	// out of pass rates and rerun.
	InfraFailure bool `json:"infraFailure,omitempty"`
	// StopReason is the limit the harness stopped the agent on, such as
	// "error_max_budget_usd"; an agent outcome, not an infrastructure failure.
	StopReason string `json:"stopReason,omitempty"`
}

// Write persists one result as indented JSON.
func Write(path string, value Result) error {
	if err := value.Validate(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// Read decodes and validates one persisted result.
func Read(path string) (Result, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var value Result
	if err := json.Unmarshal(content, &value); err != nil {
		return Result{}, err
	}
	if value.SchemaVersion == legacySchemaVersion {
		if value.Model == "" {
			value.Model = unknownModel
		}
		if value.ThinkingBudget == "" {
			value.ThinkingBudget = unknownThinking
		}
	}
	if err := value.Validate(); err != nil {
		return Result{}, err
	}
	return value, nil
}

// Validate checks the persisted schema and required metadata.
func (r Result) Validate() error {
	switch r.SchemaVersion {
	case legacySchemaVersion, previousSchemaVersion, schemaVersion4, schemaVersion5, SchemaVersion:
	default:
		return fmt.Errorf("unsupported result schema version %d", r.SchemaVersion)
	}
	if r.Task == "" || r.Language == "" || r.Harness == "" {
		return errors.New("task, language, and harness are required")
	}
	if r.SchemaVersion >= previousSchemaVersion && (r.Model == "" || r.ThinkingBudget == "") {
		return errors.New("model and thinkingBudget are required")
	}
	if r.SkillsDir != "" && !filepath.IsAbs(r.SkillsDir) {
		return errors.New("skillsDir must be an absolute path")
	}
	if r.Run < 1 || r.RequestedRuns < 1 || r.Run > r.RequestedRuns {
		return errors.New("run must be within requestedRuns")
	}
	if r.DurationSeconds < 0 {
		return errors.New("durationSeconds cannot be negative")
	}
	if r.Corrections < 0 {
		return errors.New("corrections cannot be negative")
	}
	return nil
}

// Package harness provide abstractions necessray to interact with a coding harness
package harness

import (
	"errors"

	"token-bench/harness/internal/claude"
	"token-bench/harness/internal/pi"
	"token-bench/harness/internal/shared"
	"token-bench/tracing"
)

type (
	// Config contains options shared by coding harness implementations.
	Config struct {
		SkillsDir string
		// Model overrides the harness model; empty selects the harness default.
		// Only the Claude harness supports it.
		Model string
		// PluginDir is a complete Claude Code plugin directory loaded with
		// --plugin-dir; Claude harness only.
		PluginDir string
		// ExtraSystemPrompt is appended to the harness' own system prompt;
		// Claude harness only.
		ExtraSystemPrompt string
		// Env holds KEY=value entries added to the agent process' environment;
		// Claude harness only.
		Env []string
		// MaxBudgetUSD caps one Claude session's spend; zero selects the default.
		MaxBudgetUSD float64
		// APIKeyHelper is a shell command printing the API key, made Claude
		// Code's only credential source; empty keeps inherited credentials.
		// Claude harness only.
		APIKeyHelper string
		// Status receives best-effort formatted activity updates. The caller owns
		// the capacity-one channel and must not close it while a harness is running.
		Status chan string
	}

	Harness interface {
		Model() string
		ThinkingBudget() string
		// Start launches the harness. benchmark may be nil when tracing is disabled.
		Start(workingDir string, benchmark tracing.BenchmarkSpan) error
		// Send a message to the given harness. Message could be either the starting prompt or feedback
		SendMessage(message string) (string, error)
		TokenCount() (TokenCount, error)
		// ReportedCostUSD is the cost the harness itself reported for the run,
		// and false when it reports none.
		ReportedCostUSD() (float64, bool)
		End() error
	}
	TokenCount = shared.TokenCount
	// InfraError marks a harness or model API failure; see IsInfrastructure.
	InfraError = shared.InfraError
	// StopError marks a session stopped on a spend or turn cap; see StopReason.
	StopError = shared.StopError
)

// NewPi initializes the Pi coding harness.
func NewPi(config Config) Harness {
	return pi.New(pi.Config{SkillsDir: config.SkillsDir, Status: config.Status})
}

// NewClaude initializes the Claude Code harness.
func NewClaude(config Config) Harness {
	return claude.New(claude.Config{
		SkillsDir: config.SkillsDir, Status: config.Status, Model: config.Model, PluginDir: config.PluginDir,
		ExtraSystemPrompt: config.ExtraSystemPrompt, Env: config.Env, MaxBudgetUSD: config.MaxBudgetUSD,
		APIKeyHelper: config.APIKeyHelper,
	})
}

// IsInfrastructure reports whether err is a harness or model API failure
// rather than an agent outcome.
func IsInfrastructure(err error) bool {
	var infra *shared.InfraError
	return errors.As(err, &infra)
}

// StopReason returns the limit a harness stopped the agent on, if any.
func StopReason(err error) (string, bool) {
	var stop *shared.StopError
	if errors.As(err, &stop) {
		return stop.Reason, true
	}
	return "", false
}

// DefaultMaxBudgetUSD is the per-run spend cap passed to Claude Code unless overridden.
const DefaultMaxBudgetUSD = claude.DefaultMaxBudgetUSD

// ClaudeStreamArtifact is the name of the Claude stream log written to the working directory.
const ClaudeStreamArtifact = claude.StreamArtifactName

// Package claude implements the Claude Code streaming harness.
package claude

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"token-bench/harness/internal/shared"
	"token-bench/tracing"
)

//go:embed append_system_prompt.md
var appendSystemPrompt string

const (
	defaultModel = "opus"
	// DefaultMaxBudgetUSD is the per-session spend cap when Config sets none.
	DefaultMaxBudgetUSD = 20.0
	effort              = "high"
	streamArtifactName  = "claude-stream.jsonl"
	stderrArtifactName  = "claude-stderr.log"
)

// StreamArtifactName is the file, in the working directory, that receives the raw stream log.
const StreamArtifactName = streamArtifactName

// Config contains Claude Code startup configuration.
type Config struct {
	SkillsDir string
	Status    chan string
	// Model is the Claude Code model alias or id. Empty selects the default.
	Model string
	// PluginDir is a complete plugin directory passed as an extra --plugin-dir.
	// It is never modified or removed.
	PluginDir string
	// ExtraSystemPrompt is appended to the built-in appended system prompt.
	ExtraSystemPrompt string
	// Env entries (KEY=value) are added to the process environment, overriding duplicates.
	Env []string
	// MaxBudgetUSD is passed as --max-budget-usd; zero selects DefaultMaxBudgetUSD.
	MaxBudgetUSD float64
	// APIKeyHelper, when set, is the only credential source: a shell command
	// printing the API key, passed as the apiKeyHelper setting. Claude Code
	// runs it itself, so the key is never in the environment of Claude Code or
	// of the commands the agent runs, and inherited credentials are removed.
	APIKeyHelper string
}

// credentialVariables select or carry model API credentials; with an
// APIKeyHelper they are removed from the inherited environment so no other
// credential (such as the user's own ANTHROPIC_API_KEY) can be used.
var credentialVariables = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"AWS_BEARER_TOKEN_BEDROCK", "ANTHROPIC_FOUNDRY_API_KEY",
}

// processEnvironment is the environment of the claude process: the inherited
// one (without credential variables when an APIKeyHelper is set), then the
// harness' own settings and the configured entries.
func (c *claude) processEnvironment(inherited []string) []string {
	var environment []string
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		if c.config.APIKeyHelper != "" && slices.Contains(credentialVariables, name) {
			continue
		}
		environment = append(environment, entry)
	}
	return append(append(environment, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"), c.config.Env...)
}

// TokenCount contains usage dimensions reported by Claude Code.
type TokenCount = shared.TokenCount

type claude struct {
	config     Config
	mu         sync.Mutex
	process    *runningProcess
	pluginPath string
	tokens     TokenCount
	lastReply  string
	costUSD    float64
	costSeen   bool
	// modelUsageSeen is set once a result carried modelUsage (cumulative).
	modelUsageSeen bool
}

type runningProcess struct {
	command     *exec.Cmd
	stdin       io.WriteCloser
	results     chan resultEvent
	stopping    chan struct{}
	stopOnce    sync.Once
	processDone *asyncStatus
	readerDone  *asyncStatus
	streamFile  *os.File
	stderrFile  *os.File
	streamPath  string
	stderrPath  string
	workingDir  string
	trace       *traceAdapter
	status      chan string
}

type asyncStatus struct {
	done chan struct{}
	err  error
}

func newAsyncStatus() *asyncStatus {
	return &asyncStatus{done: make(chan struct{})}
}

func (s *asyncStatus) complete(err error) {
	s.err = err
	close(s.done)
}

func (s *asyncStatus) result() error {
	<-s.done
	return s.err
}

type resultEvent struct {
	Type    string   `json:"type"`
	Subtype string   `json:"subtype"`
	Result  string   `json:"result"`
	IsError bool     `json:"is_error"`
	Errors  []string `json:"errors"`
	// APIErrorStatus is the HTTP status of the API error that ended the turn.
	APIErrorStatus *int `json:"api_error_status"`
	// TotalCostUSD is Claude Code's own cost for the turn; nil when absent.
	TotalCostUSD *float64              `json:"total_cost_usd"`
	Usage        resultUsage           `json:"usage"`
	ModelUsage   map[string]modelUsage `json:"modelUsage"`
	Origin       struct {
		Kind string `json:"kind"`
	} `json:"origin"`
}

type resultUsage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
}

type modelUsage struct {
	Input      int `json:"inputTokens"`
	Output     int `json:"outputTokens"`
	CacheRead  int `json:"cacheReadInputTokens"`
	CacheWrite int `json:"cacheCreationInputTokens"`
}

// New initializes the private Claude Code implementation.
func New(config Config) *claude {
	if config.Model == "" {
		config.Model = defaultModel
	}
	if config.MaxBudgetUSD == 0 {
		config.MaxBudgetUSD = DefaultMaxBudgetUSD
	}
	return &claude{config: config}
}

func (c *claude) Model() string {
	return c.config.Model
}

func (c *claude) ThinkingBudget() string {
	return effort
}

func (c *claude) Start(workingDir string, benchmark tracing.BenchmarkSpan) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.process != nil {
		return errors.New("Claude is already started")
	}
	executable, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("find claude: %w", err)
	}
	if info, err := os.Stat(workingDir); err != nil {
		return fmt.Errorf("inspect working directory: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", workingDir)
	}
	if err := c.createSkillsPlugin(); err != nil {
		_ = c.cleanupPlugin()
		return err
	}

	running, err := c.startProcess(executable, workingDir, benchmark)
	if err != nil {
		_ = c.cleanupPlugin()
		return err
	}
	c.process = running
	c.tokens = TokenCount{}
	c.costUSD, c.costSeen, c.modelUsageSeen = 0, false, false
	c.lastReply = ""
	return nil
}

func (c *claude) startProcess(executable, workingDir string, benchmark tracing.BenchmarkSpan) (*runningProcess, error) {
	streamFile, err := os.CreateTemp("", "token-bench-claude-stream-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create Claude stream log: %w", err)
	}
	stderrFile, err := os.CreateTemp("", "token-bench-claude-stderr-*.log")
	if err != nil {
		_ = streamFile.Close()
		_ = os.Remove(streamFile.Name())
		return nil, fmt.Errorf("create Claude stderr log: %w", err)
	}

	command := exec.Command(executable, c.startArguments()...)
	command.Dir = workingDir
	command.Env = c.processEnvironment(os.Environ())
	command.Stderr = stderrFile
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, c.cleanupUnstartedProcess(streamFile, stderrFile, workingDir, fmt.Errorf("open Claude stdin: %w", err))
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, c.cleanupUnstartedProcess(streamFile, stderrFile, workingDir, fmt.Errorf("open Claude stdout: %w", err))
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, c.cleanupUnstartedProcess(streamFile, stderrFile, workingDir, fmt.Errorf("start Claude: %w", err))
	}

	running := &runningProcess{
		command:     command,
		stdin:       stdin,
		results:     make(chan resultEvent, 64),
		stopping:    make(chan struct{}),
		processDone: newAsyncStatus(),
		readerDone:  newAsyncStatus(),
		streamFile:  streamFile,
		stderrFile:  stderrFile,
		streamPath:  streamFile.Name(),
		stderrPath:  stderrFile.Name(),
		workingDir:  workingDir,
		trace:       newTraceAdapter(benchmark, c.Model()),
		status:      c.config.Status,
	}
	go func() {
		// StdoutPipe must be drained before Wait so the final result and log tail
		// cannot be truncated when Claude exits.
		<-running.readerDone.done
		running.processDone.complete(command.Wait())
	}()
	go readStream(io.TeeReader(stdout, streamFile), running)
	return running, nil
}

func (c *claude) cleanupUnstartedProcess(streamFile, stderrFile *os.File, workingDir string, cause error) error {
	streamPath := streamFile.Name()
	stderrPath := stderrFile.Name()
	closeErr := errors.Join(streamFile.Close(), stderrFile.Close())
	persistErr := errors.Join(
		persistArtifact(streamPath, filepath.Join(workingDir, streamArtifactName)),
		persistArtifact(stderrPath, filepath.Join(workingDir, stderrArtifactName)),
	)
	return errors.Join(cause, closeErr, persistErr)
}

func readStream(reader io.Reader, running *runningProcess) {
	var readErr error
	defer func() {
		running.trace.Close()
		running.readerDone.complete(readErr)
		close(running.results)
	}()

	decoder := json.NewDecoder(reader)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			readErr = fmt.Errorf("decode Claude stream: %w", err)
			return
		}

		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			readErr = fmt.Errorf("decode Claude stream envelope: %w", err)
			return
		}
		if envelope.Type == "result" {
			var result resultEvent
			if err := json.Unmarshal(raw, &result); err != nil {
				readErr = fmt.Errorf("decode Claude result: %w", err)
				return
			}
			select {
			case running.results <- result:
			case <-running.stopping:
			}
		}
		if err := running.trace.Process(envelope.Type, raw); err != nil {
			readErr = fmt.Errorf("trace Claude stream event: %w", err)
			return
		}
		publishStatus(running.status, envelope.Type, raw)
	}
}

func (c *claude) createSkillsPlugin() error {
	if c.config.SkillsDir == "" {
		return nil
	}
	plugin, err := os.MkdirTemp("", "token-bench-claude-plugin-*")
	if err != nil {
		return fmt.Errorf("create Claude skills plugin: %w", err)
	}
	c.pluginPath = plugin
	manifestDir := filepath.Join(plugin, ".claude-plugin")
	if err := os.Mkdir(manifestDir, 0o755); err != nil {
		return fmt.Errorf("create Claude plugin manifest directory: %w", err)
	}
	manifest := []byte(`{"name":"token-bench-skills","version":"1.0.0","description":"Skills supplied to token-bench","author":{"name":"token-bench"}}` + "\n")
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), manifest, 0o600); err != nil {
		return fmt.Errorf("write Claude plugin manifest: %w", err)
	}
	skillsDir := filepath.Join(plugin, "skills")
	if err := os.Mkdir(skillsDir, 0o755); err != nil {
		return fmt.Errorf("create Claude plugin skills directory: %w", err)
	}

	sources, err := findSkillDirectories(c.config.SkillsDir)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("skills directory %q contains no SKILL.md files", c.config.SkillsDir)
	}
	for _, source := range sources {
		relative, err := filepath.Rel(c.config.SkillsDir, source)
		if err != nil {
			return fmt.Errorf("name Claude skill %q: %w", source, err)
		}
		name := filepath.Base(c.config.SkillsDir)
		if relative != "." {
			name = strings.ReplaceAll(relative, string(filepath.Separator), "--")
		}
		if err := os.Symlink(source, filepath.Join(skillsDir, name)); err != nil {
			return fmt.Errorf("stage Claude skill %q: %w", source, err)
		}
	}
	return nil
}

func findSkillDirectories(root string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err == nil {
		return []string{root}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect skill in %q: %w", root, err)
	}
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "SKILL.md" {
			directories = append(directories, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover Claude skills: %w", err)
	}
	return directories, nil
}

func (c *claude) startArguments() []string {
	arguments := []string{
		"--model", c.config.Model,
		"--effort", effort,
		"--dangerously-skip-permissions",
		"--disallowedTools", "AskUserQuestion",
		"--append-system-prompt", c.systemPrompt(),
		"--max-budget-usd", strconv.FormatFloat(c.config.MaxBudgetUSD, 'f', -1, 64),
		"--setting-sources", "",
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if c.pluginPath != "" {
		arguments = append(arguments, "--plugin-dir", c.pluginPath)
	}
	if c.config.PluginDir != "" {
		arguments = append(arguments, "--plugin-dir", c.config.PluginDir)
	}
	if c.config.APIKeyHelper != "" {
		settings, _ := json.Marshal(map[string]string{"apiKeyHelper": c.config.APIKeyHelper})
		arguments = append(arguments, "--settings", string(settings))
	}
	return arguments
}

func (c *claude) systemPrompt() string {
	prompt := strings.TrimSpace(appendSystemPrompt)
	if extra := strings.TrimSpace(c.config.ExtraSystemPrompt); extra != "" {
		prompt += "\n\n" + extra
	}
	return prompt
}

func (c *claude) SendMessage(message string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.process == nil {
		return "", errors.New("Claude is not started")
	}

	record := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		ParentToolUseID *string `json:"parent_tool_use_id"`
	}{Type: "user"}
	record.Message.Role = "user"
	record.Message.Content = message
	c.process.trace.SetNextInput(message)
	if err := json.NewEncoder(c.process.stdin).Encode(record); err != nil {
		return "", fmt.Errorf("send Claude prompt: %w", err)
	}

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		select {
		case result, ok := <-c.process.results:
			if !ok {
				return "", c.processFailure("Claude stream ended before a result", c.process)
			}
			c.addUsage(result)
			c.lastReply = result.Result
			return c.lastReply, resultError(result)
		case <-timer.C:
			return "", errors.New("wait for Claude completion: timed out after 1h")
		}
	}
}

// addUsage folds one result event into the run totals. In a streaming-input
// session Claude Code reports total_cost_usd and modelUsage as running totals
// for the whole process (each result carries everything so far), so the latest
// result replaces the previous totals; summing them would count every earlier
// turn again after each correction. A crash or startup-error result may carry
// zeroed totals, so a smaller value never replaces a larger one. usage, by
// contrast, is per turn and main loop only; it is summed, and used only when
// modelUsage is absent.
func (c *claude) addUsage(result resultEvent) {
	if result.TotalCostUSD != nil && (!c.costSeen || *result.TotalCostUSD >= c.costUSD) {
		c.costUSD = *result.TotalCostUSD
		c.costSeen = true
	}
	if len(result.ModelUsage) == 0 {
		if !c.modelUsageSeen {
			c.tokens.Input += result.Usage.Input
			c.tokens.Output += result.Usage.Output
			c.tokens.CacheRead += result.Usage.CacheRead
			c.tokens.CacheWrite += result.Usage.CacheWrite
		}
	} else {
		var cumulative TokenCount
		for _, usage := range result.ModelUsage {
			cumulative.Input += usage.Input
			cumulative.Output += usage.Output
			cumulative.CacheRead += usage.CacheRead
			cumulative.CacheWrite += usage.CacheWrite
		}
		cumulative.Total = cumulative.Input + cumulative.Output + cumulative.CacheRead + cumulative.CacheWrite
		if !c.modelUsageSeen || cumulative.Total >= c.tokens.Total {
			c.tokens = cumulative
		}
		c.modelUsageSeen = true
	}
	c.tokens.Total = c.tokens.Input + c.tokens.Output + c.tokens.CacheRead + c.tokens.CacheWrite
}

func (c *claude) TokenCount() (TokenCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.process == nil {
		return TokenCount{}, errors.New("Claude is not started")
	}
	return c.tokens, nil
}

func (c *claude) ReportedCostUSD() (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.costUSD, c.costSeen
}

func (c *claude) End() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endLocked()
}

func (c *claude) endLocked() error {
	if c.process == nil {
		return c.cleanupPlugin()
	}
	running := c.process
	running.stopOnce.Do(func() { close(running.stopping) })

	var result error
	if err := running.stdin.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("close Claude stdin: %w", err))
	}
	select {
	case <-running.processDone.done:
	case <-time.After(10 * time.Second):
		result = errors.Join(result, errors.New("Claude did not exit within 10s"))
		if err := running.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			result = errors.Join(result, fmt.Errorf("kill Claude: %w", err))
		}
		<-running.processDone.done
	}
	if err := running.processDone.result(); err != nil {
		result = errors.Join(result, fmt.Errorf("wait for Claude: %w", err))
	}

	select {
	case <-running.readerDone.done:
		if err := running.readerDone.result(); err != nil {
			result = errors.Join(result, err)
		}
	case <-time.After(2 * time.Second):
		result = errors.Join(result, errors.New("Claude stream reader did not stop within 2s"))
	}
	if err := running.streamFile.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("close Claude stream log: %w", err))
	}
	if err := running.stderrFile.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("close Claude stderr log: %w", err))
	}
	if err := persistArtifact(running.streamPath, filepath.Join(running.workingDir, streamArtifactName)); err != nil {
		result = errors.Join(result, fmt.Errorf("persist Claude stream log: %w", err))
	}
	if err := persistArtifact(running.stderrPath, filepath.Join(running.workingDir, stderrArtifactName)); err != nil {
		result = errors.Join(result, fmt.Errorf("persist Claude stderr log: %w", err))
	}
	c.process = nil
	return errors.Join(result, c.cleanupPlugin())
}

func (c *claude) processFailure(prefix string, running *runningProcess) error {
	parts := []string{prefix}
	select {
	case <-running.readerDone.done:
		if err := running.readerDone.result(); err != nil {
			parts = append(parts, err.Error())
		}
	default:
	}
	select {
	case <-running.processDone.done:
		if err := running.processDone.result(); err != nil {
			parts = append(parts, err.Error())
		}
	default:
	}
	if err := running.stderrFile.Sync(); err == nil {
		if content, readErr := os.ReadFile(running.stderrPath); readErr == nil {
			if stderr := strings.TrimSpace(string(content)); stderr != "" {
				parts = append(parts, stderr)
			}
		}
	}
	return &shared.InfraError{Err: errors.New(strings.Join(parts, ": "))}
}

// resultError classifies a result event that did not end the turn normally.
// A turn ended by an API error (is_error on a success result, or an
// api_error_status) and error_during_execution are infrastructure failures; a
// spend or turn cap is an agent outcome. Anything else completes the turn.
func resultError(result resultEvent) error {
	detail := strings.TrimSpace(strings.Join(append([]string{result.Result}, result.Errors...), "; "))
	switch {
	case result.Subtype == "error_max_budget_usd" || result.Subtype == "error_max_turns":
		return &shared.StopError{Reason: result.Subtype, Detail: detail}
	case result.APIErrorStatus != nil:
		return &shared.InfraError{Err: fmt.Errorf("Claude API error (HTTP %d): %s", *result.APIErrorStatus, detail)}
	case result.Subtype == "error_during_execution":
		return &shared.InfraError{Err: fmt.Errorf("Claude Code error_during_execution: %s", detail)}
	case result.IsError:
		return &shared.InfraError{Err: fmt.Errorf("Claude turn ended on an error (%s): %s", result.Subtype, detail)}
	}
	return nil
}

func (c *claude) cleanupPlugin() error {
	if c.pluginPath == "" {
		return nil
	}
	err := os.RemoveAll(c.pluginPath)
	c.pluginPath = ""
	if err != nil {
		return fmt.Errorf("remove Claude skills plugin: %w", err)
	}
	return nil
}

func persistArtifact(source, destination string) error {
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(source, destination); err == nil {
		return nil
	}

	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Remove(source)
}

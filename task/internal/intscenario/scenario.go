// Package intscenario is the shared base of the integration scenarios of the
// grammar benchmark: a per-run docker network with real sidecars (databases,
// brokers) and mock API sidecars, the agent's program run in a fresh container
// of the arm's integration image on that network, and a registry of named
// checks that make up the grade.
package intscenario

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"token-bench/task/answer"
)

const (
	// MockImage runs one mock API per container (-service <name>).
	MockImage = "token-bench-mock:latest"
	// MockPort is the port every mock listens on.
	MockPort = "8080"
	platform = "linux/arm64"

	startTimeout = 4 * time.Minute
	setupTimeout = 4 * time.Minute
)

// Sidecar is one container on the run network, reachable by its alias.
type Sidecar struct {
	Alias string
	Image string
	// Args follow the image name (the container's command line).
	Args []string
	Env  []string
	// Port is the container port also published on 127.0.0.1 for the grader.
	Port string
	// Mock marks a mock API (MockImage, -service Alias, linux/arm64).
	Mock bool
	// Ready waits until the sidecar answers; addr is its published host:port.
	Ready func(ctx context.Context, addr string) error
}

// Scenario is what a concrete scenario provides.
type Scenario interface {
	// Name is the scenario name (lead-routing style).
	Name() string
	Prompt() string
	Sidecars() []Sidecar
	// AppPort is the port the agent's program serves on ("" when it serves none).
	AppPort() string
	// AppReady reports whether the started program is ready for requests.
	AppReady(ctx context.Context, env *Env) error
	// Reset returns every sidecar to its initial state. It runs before the
	// agent starts and again before grading.
	Reset(ctx context.Context, env *Env) error
	// Grade runs every check against the running program.
	Grade(ctx context.Context, env *Env, checks *Checks)
	// CheckNames lists the checks, in order.
	CheckNames() []string
}

// Env is the run's docker environment.
type Env struct {
	Network string
	Image   string
	WorkDir string
	suffix  string
	// Addr maps a sidecar alias to its published 127.0.0.1:port.
	Addr map[string]string
	// AppAddr is the program's published address once started.
	AppAddr    string
	containers []string
}

// InfraError marks a grading failure of the machinery, not of the agent.
type InfraError struct{ Err error }

func (e *InfraError) Error() string { return e.Err.Error() }

// Docker runs a docker command.
func Docker(ctx context.Context, arguments ...string) (string, error) {
	output, err := runDocker(ctx, arguments...)
	text := strings.TrimSpace(output)
	if err != nil {
		return text, fmt.Errorf("docker %s: %w: %s", strings.Join(arguments[:min(3, len(arguments))], " "), err, text)
	}
	return text, nil
}

// Name returns a container name for a role in this run.
func (e *Env) Name(role string) string { return "tb-" + role + "-" + e.suffix }

// Exec runs a command in a sidecar container.
func (e *Env) Exec(ctx context.Context, alias string, command ...string) (string, error) {
	return Docker(ctx, append([]string{"exec", e.Name(alias)}, command...)...)
}

// Handle is one run of a scenario; it implements the task handle.
type Handle struct {
	scenario Scenario
	env      *Env
}

// New prepares a run in workDir on the given network, running the agent's
// program in image.
func New(scenario Scenario, workDir, image, network string) *Handle {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return &Handle{scenario: scenario, env: &Env{Network: network, Image: image, WorkDir: workDir, suffix: hex.EncodeToString(suffix), Addr: map[string]string{}}}
}

// Env exposes the run environment.
func (h *Handle) Env() *Env { return h.env }

// Prompt implements the task handle.
func (h *Handle) Prompt() (string, error) { return h.scenario.Prompt(), nil }

// Feedback is empty: scenarios are single shot.
func (h *Handle) Feedback() (string, error) { return "", nil }

// Validation reports whether every check passed.
func (h *Handle) Validation() (bool, error) {
	grade, err := h.Grade()
	return grade.Passed(), err
}

// Setup creates the network and sidecars, waits for them and resets state.
func (h *Handle) Setup() (func() error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	env := h.env
	if _, err := Docker(ctx, "network", "create", env.Network); err != nil {
		return func() error { return nil }, err
	}
	for _, sidecar := range h.scenario.Sidecars() {
		if err := h.start(ctx, sidecar); err != nil {
			return h.cleanup, err
		}
	}
	for _, sidecar := range h.scenario.Sidecars() {
		if sidecar.Ready == nil {
			continue
		}
		if err := waitReady(ctx, sidecar.Ready, env.Addr[sidecar.Alias]); err != nil {
			return h.cleanup, fmt.Errorf("%s not ready: %w", sidecar.Alias, err)
		}
	}
	if err := h.scenario.Reset(ctx, env); err != nil {
		return h.cleanup, fmt.Errorf("reset: %w", err)
	}
	return h.cleanup, nil
}

func (h *Handle) start(ctx context.Context, sidecar Sidecar) error {
	env := h.env
	name := env.Name(sidecar.Alias)
	env.containers = append(env.containers, name)
	arguments := []string{"run", "-d", "--rm", "--name", name, "--network", env.Network, "--network-alias", sidecar.Alias}
	port := sidecar.Port
	if sidecar.Mock {
		arguments = append(arguments, "--platform", platform)
		port = MockPort
	}
	if port != "" {
		arguments = append(arguments, "-p", "127.0.0.1::"+port)
	}
	for _, entry := range sidecar.Env {
		arguments = append(arguments, "-e", entry)
	}
	if sidecar.Mock {
		arguments = append(arguments, MockImage, "-service", sidecar.Alias)
	} else {
		arguments = append(arguments, sidecar.Image)
		arguments = append(arguments, sidecar.Args...)
	}
	if _, err := Docker(ctx, arguments...); err != nil {
		return err
	}
	if port == "" {
		return nil
	}
	address, err := Docker(ctx, "port", name, port)
	if err != nil {
		return err
	}
	env.Addr[sidecar.Alias] = strings.Split(address, "\n")[0]
	return nil
}

func waitReady(ctx context.Context, ready func(context.Context, string) error, addr string) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		err := ready(ctx, addr)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Second)
	}
}

const readyTimeout = 120 * time.Second

func (h *Handle) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var failures []string
	for _, container := range h.env.containers {
		if _, err := Docker(ctx, "rm", "-f", container); err != nil && !strings.Contains(err.Error(), "No such container") {
			failures = append(failures, err.Error())
		}
	}
	if _, err := Docker(ctx, "network", "rm", h.env.Network); err != nil && !strings.Contains(err.Error(), "not found") {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// Grade starts the agent's program and runs the scenario's checks.
func (h *Handle) Grade() (answer.Grade, error) {
	checks := NewChecks(h.scenario.CheckNames())
	grade, err := h.run(checks)
	if err != nil {
		return answer.Grade{}, &InfraError{Err: err}
	}
	return grade, nil
}

func (h *Handle) run(checks *Checks) (answer.Grade, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	env := h.env
	project, ok := projectDir(env.WorkDir)
	if !ok {
		checks.Fail(checks.names[0], "no Ballerina.toml in the working directory or one level below")
		return checks.Result("no Ballerina project"), nil
	}
	if err := h.scenario.Reset(ctx, env); err != nil {
		return answer.Grade{}, fmt.Errorf("reset before grading: %w", err)
	}
	app := env.Name("app")
	env.containers = append(env.containers, app)
	runArguments := []string{"run", "-d", "--platform", platform, "--name", app, "--network", env.Network,
		"-v", project + ":/app", "-w", "/app"}
	if port := h.scenario.AppPort(); port != "" {
		runArguments = append(runArguments, "-p", "127.0.0.1::"+port)
	}
	runArguments = append(runArguments, "--entrypoint", "bal", env.Image, "run")
	if _, err := Docker(ctx, runArguments...); err != nil {
		return answer.Grade{}, err
	}
	defer func() {
		logs, _ := Docker(context.Background(), "logs", app)
		_ = os.WriteFile(filepath.Join(env.WorkDir, "grader-app.log"), []byte(logs+"\n"), 0o644)
		_, _ = Docker(context.Background(), "rm", "-f", app)
	}()
	if port := h.scenario.AppPort(); port != "" {
		address, err := Docker(ctx, "port", app, port)
		if err != nil {
			return answer.Grade{}, err
		}
		env.AppAddr = strings.Split(address, "\n")[0]
	}
	if reason := waitStarted(ctx, h.scenario, env, app); reason != "" {
		checks.Fail(checks.names[0], reason)
		return checks.Result(""), nil
	}
	checks.Pass(checks.names[0])
	h.scenario.Grade(ctx, env, checks)
	return checks.Result(""), nil
}

func waitStarted(ctx context.Context, scenario Scenario, env *Env, app string) string {
	deadline := time.Now().Add(startTimeout)
	var last error
	for time.Now().Before(deadline) {
		if last = scenario.AppReady(ctx, env); last == nil {
			return ""
		}
		if state, _ := Docker(ctx, "inspect", "-f", "{{.State.Running}}", app); state == "false" {
			logs, _ := Docker(ctx, "logs", "--tail", "15", app)
			return "the program exited: " + OneLine(logs)
		}
		time.Sleep(time.Second)
	}
	return fmt.Sprintf("not ready after %s: %v", startTimeout, last)
}

// OneLine shortens text for a check's reason.
func OneLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 300 {
		text = text[len(text)-300:]
	}
	return text
}

func projectDir(workDir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(workDir, "Ballerina.toml")); err == nil {
		return workDir, true
	}
	matches, _ := filepath.Glob(filepath.Join(workDir, "*", "Ballerina.toml"))
	if len(matches) == 1 {
		return filepath.Dir(matches[0]), true
	}
	return "", false
}

// Checks is the registry of a grade's named checks. A check passes only if
// nothing failed it; the first failure's reason is kept.
type Checks struct {
	names   []string
	verdict map[string]string
}

// NewChecks starts a registry for the given check names.
func NewChecks(names []string) *Checks { return &Checks{names: names, verdict: map[string]string{}} }

// Fail records the first failure of a check.
func (c *Checks) Fail(name, reason string) {
	if _, seen := c.verdict[name]; !seen || c.verdict[name] == "" {
		c.verdict[name] = reason
	}
}

// Pass marks a check passed unless it already failed.
func (c *Checks) Pass(name string) {
	if _, seen := c.verdict[name]; !seen {
		c.verdict[name] = ""
	}
}

// Failed reports whether the check has a failure recorded.
func (c *Checks) Failed(name string) bool { return c.verdict[name] != "" }

// Result builds the grade; every unreached check fails as "not reached".
func (c *Checks) Result(problem string) answer.Grade {
	grade := answer.Grade{Present: problem == "", Problem: problem, Total: len(c.names)}
	for _, name := range c.names {
		reason, seen := c.verdict[name]
		if !seen {
			reason = "not reached"
		}
		field := answer.FieldGrade{Field: name, Correct: seen && reason == "", Got: reason, Expected: "pass"}
		if field.Correct {
			grade.Correct++
		}
		grade.Fields = append(grade.Fields, field)
	}
	return grade
}

// WaitHTTP waits for a GET on url to answer with any status.
func WaitHTTP(ctx context.Context, url string) error {
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}

// Request is one request a mock received.
type Request struct {
	Seq     int               `json:"seq"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

// MockRequests reads the requests a mock recorded.
func (e *Env) MockRequests(alias string) ([]Request, error) {
	response, err := http.Get("http://" + e.Addr[alias] + "/__requests")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var requests []Request
	if err := json.NewDecoder(response.Body).Decode(&requests); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return requests, nil
}

// MockReset clears a mock's recorded requests and state.
func (e *Env) MockReset(alias string) error {
	response, err := http.Post("http://"+e.Addr[alias]+"/__reset", "application/json", strings.NewReader(""))
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}

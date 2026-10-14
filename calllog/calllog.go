// Package calllog records and summarizes the `bal` calls an agent made, as
// logged by the bal-calllog wrapper: one JSON line per call, with the raw
// stdout and stderr saved beside the log.
package calllog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Call is one logged `bal` invocation.
type Call struct {
	Seq         int      `json:"seq"`
	Argv        []string `json:"argv"`
	ExitCode    int      `json:"exitCode"`
	StdoutBytes int      `json:"stdoutBytes"`
	StderrBytes int      `json:"stderrBytes"`
	StartedAt   string   `json:"startedAt"`
	DurationMs  int64    `json:"durationMs"`
	// OutputSHA256 hashes stdout followed by stderr.
	OutputSHA256 string `json:"outputSha256"`
	// StdoutFile and StderrFile are the saved outputs, relative to the log's directory.
	StdoutFile string `json:"stdoutFile"`
	StderrFile string `json:"stderrFile"`
}

// IsDiscover reports whether the call is `bal discover ...`.
func (c Call) IsDiscover() bool { return len(c.Argv) > 0 && c.Argv[0] == "discover" }

// DiscoverArgs are the arguments after `discover`.
func (c Call) DiscoverArgs() []string {
	if !c.IsDiscover() {
		return nil
	}
	return c.Argv[1:]
}

// IsHelp reports whether a discover call asks for help: --help, -h, or a
// `help` word.
func (c Call) IsHelp() bool {
	for _, argument := range c.DiscoverArgs() {
		if argument == "--help" || argument == "-h" || argument == "help" {
			return true
		}
	}
	return false
}

// IsRootHelp reports whether a help call names nothing but help.
func (c Call) IsRootHelp() bool {
	if !c.IsHelp() {
		return false
	}
	for _, argument := range c.DiscoverArgs() {
		if argument != "--help" && argument != "-h" && argument != "help" {
			return false
		}
	}
	return true
}

// Read parses a call log. A missing log is an empty list.
func Read(path string) ([]Call, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Parse(file)
}

// Parse reads JSON lines of calls; a malformed line is an error.
func Parse(reader io.Reader) ([]Call, error) {
	var calls []Call
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var call Call
		if err := json.Unmarshal([]byte(text), &call); err != nil {
			return nil, fmt.Errorf("call log line %d: %w", line, err)
		}
		calls = append(calls, call)
	}
	return calls, scanner.Err()
}

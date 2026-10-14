package calllog

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// BytesPerToken is the fallback token estimate for tool output (design §5).
const BytesPerToken = 3.5

// Summary is the `discover` block of a result: the agent's use of `bal
// discover`, from the call log.
type Summary struct {
	// Calls counts `bal discover` calls; OtherBalCalls counts every other `bal` call.
	Calls         int `json:"calls"`
	OtherBalCalls int `json:"otherBalCalls"`
	// Failed counts discover calls with a non-zero exit; FailedByExit splits them by code.
	Failed       int            `json:"failed"`
	FailedByExit map[string]int `json:"failedByExit,omitempty"`
	// HelpCalls counts discover calls asking for help; RootHelpCalls is the
	// part that names nothing else (the rest is per-noun help).
	HelpCalls     int `json:"helpCalls"`
	RootHelpCalls int `json:"rootHelpCalls"`
	// Bytes read from the tool (stdout + stderr), split by kind of call.
	AnswerBytes int `json:"answerBytes"`
	ErrorBytes  int `json:"errorBytes"`
	HelpBytes   int `json:"helpBytes"`
	TotalBytes  int `json:"totalBytes"`
	// EstimatedTokens is TotalBytes / BytesPerToken.
	EstimatedTokens int `json:"estimatedTokens"`
	// ToolMs is the summed duration of every discover call.
	ToolMs int64 `json:"toolMs"`
	// ErrorEpisodes counts maximal runs of consecutive failed calls;
	// RecoveredEpisodes those followed later by a successful call.
	ErrorEpisodes     int `json:"errorEpisodes"`
	RecoveredEpisodes int `json:"recoveredEpisodes"`
	// HelpFirst is true when the first discover call asked for help.
	HelpFirst bool `json:"helpFirst"`
	// FirstCommand is the argv after `discover` of the first non-help call.
	FirstCommand []string `json:"firstCommand,omitempty"`
	// FirstValid: the first non-help call exited 0. FirstOnTarget: it matched
	// one of the task's on-target first commands for the arm's grammar (nil
	// when the task or arm defines none). FirstBeforeHelp: it came before any
	// help call.
	FirstValid      bool  `json:"firstValid"`
	FirstOnTarget   *bool `json:"firstOnTarget,omitempty"`
	FirstBeforeHelp bool  `json:"firstBeforeHelp"`
	// Successful counts discover calls with exit 0 that were not help.
	Successful int `json:"successful"`
	// LiveFetches counts files the run added to the discover cache beyond the seed.
	LiveFetches int `json:"liveFetches"`
	// Bypasses lists Bash commands that read the tool or libraries around
	// discover (installed balas, jars, the Ballerina home) or reach the web.
	Bypasses []string `json:"bypasses,omitempty"`
}

// Summarize builds the discover block. targets are the on-target first
// commands for the arm's grammar (may be nil).
func Summarize(calls []Call, targets [][]string) Summary {
	summary := Summary{FailedByExit: map[string]int{}}
	// inEpisode: the previous call failed. episodeOpen: some episode has not
	// yet been followed by a successful non-help call.
	inEpisode, episodeOpen, seenHelp, firstSeen := false, false, false, false
	for _, call := range calls {
		if !call.IsDiscover() {
			summary.OtherBalCalls++
			continue
		}
		summary.Calls++
		bytes := call.StdoutBytes + call.StderrBytes
		summary.TotalBytes += bytes
		summary.ToolMs += call.DurationMs
		help := call.IsHelp()
		switch {
		case help:
			summary.HelpCalls++
			summary.HelpBytes += bytes
			if call.IsRootHelp() {
				summary.RootHelpCalls++
			}
			if summary.Calls == 1 {
				summary.HelpFirst = true
			}
		case call.ExitCode != 0:
			summary.ErrorBytes += bytes
		default:
			summary.AnswerBytes += bytes
			summary.Successful++
		}
		if call.ExitCode != 0 {
			summary.Failed++
			summary.FailedByExit[strconv.Itoa(call.ExitCode)]++
			if !inEpisode {
				summary.ErrorEpisodes++
				inEpisode, episodeOpen = true, true
			}
		} else {
			inEpisode = false
			if episodeOpen && !help {
				summary.RecoveredEpisodes = summary.ErrorEpisodes
				episodeOpen = false
			}
		}
		if !help && !firstSeen {
			firstSeen = true
			summary.FirstCommand = append([]string(nil), call.DiscoverArgs()...)
			summary.FirstValid = call.ExitCode == 0
			summary.FirstBeforeHelp = !seenHelp
			if len(targets) > 0 {
				onTarget := MatchesAny(call.DiscoverArgs(), targets)
				summary.FirstOnTarget = &onTarget
			}
		}
		if help {
			seenHelp = true
		}
	}
	if len(summary.FailedByExit) == 0 {
		summary.FailedByExit = nil
	}
	summary.EstimatedTokens = int(float64(summary.TotalBytes)/BytesPerToken + 0.5)
	return summary
}

// valueFlags take a value; aliases maps long spellings to one key.
var (
	valueFlags = map[string]bool{
		"-p": true, "--package": true, "-m": true, "--module": true, "--client": true, "--class": true,
		"--service": true, "--filter": true, "--page": true, "--output": true,
	}
	flagAliases = map[string]string{"--package": "-p", "--module": "-m"}
	ignored     = map[string]bool{"--output": true, "--refresh": true}
)

// canonical splits argv into positionals and flags, with flag aliases
// unified, `--flag=value` split, versions dropped from package coordinates,
// and --output/--refresh ignored.
func canonical(argv []string) (positionals []string, flags map[string]string) {
	flags = map[string]string{}
	for index := 0; index < len(argv); index++ {
		argument := argv[index]
		if strings.HasPrefix(argument, "-") {
			name, value, hasValue := strings.Cut(argument, "=")
			if !hasValue && valueFlags[name] && index+1 < len(argv) {
				index++
				value = argv[index]
			}
			if ignored[name] {
				continue
			}
			if alias, ok := flagAliases[name]; ok {
				name = alias
			}
			if name == "-p" {
				value = withoutVersion(value)
			}
			flags[name] = value
			continue
		}
		positionals = append(positionals, withoutVersion(argument))
	}
	return positionals, flags
}

var versionedCoordinate = regexp.MustCompile(`^([A-Za-z0-9_]+/[A-Za-z0-9_.]+):[0-9]`)

// withoutVersion drops the version of an org/name:version coordinate and
// leaves anything else (a resource path such as repos/:owner) unchanged.
func withoutVersion(argument string) string {
	if match := versionedCoordinate.FindStringSubmatch(argument); match != nil {
		return match[1]
	}
	return argument
}

// Matches reports whether argv is the same command as target: the same
// positionals in order and the same flags in any order, ignoring versions.
func Matches(argv, target []string) bool {
	gotPositionals, gotFlags := canonical(argv)
	wantPositionals, wantFlags := canonical(target)
	if strings.Join(gotPositionals, "\x00") != strings.Join(wantPositionals, "\x00") || len(gotFlags) != len(wantFlags) {
		return false
	}
	for name, value := range wantFlags {
		if gotFlags[name] != value {
			return false
		}
	}
	return true
}

// MatchesAny reports whether argv matches one of targets.
func MatchesAny(argv []string, targets [][]string) bool {
	for _, target := range targets {
		if Matches(argv, target) {
			return true
		}
	}
	return false
}

// bypassMarkers flag Bash commands that look around the tool.
var bypassMarkers = []string{
	".ballerina/repositories", ".ballerina/.config", "/bala", ".bala", ".jar", "unzip", "jar ",
	"curl ", "wget ", "api.central.ballerina.io", "github.com",
}

// Bypasses returns the commands that match a bypass marker, sorted and unique.
func Bypasses(commands []string) []string {
	seen := map[string]bool{}
	for _, command := range commands {
		for _, marker := range bypassMarkers {
			if strings.Contains(command, marker) {
				seen[command] = true
				break
			}
		}
	}
	out := make([]string, 0, len(seen))
	for command := range seen {
		out = append(out, command)
	}
	sort.Strings(out)
	return out
}

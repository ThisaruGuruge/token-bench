package calllog

import (
	"strings"
	"testing"
)

func call(exit, out, errBytes int, argv ...string) Call {
	return Call{Argv: argv, ExitCode: exit, StdoutBytes: out, StderrBytes: errBytes, DurationMs: 10}
}

func TestSummarizeCountsFailuresHelpAndRecovery(t *testing.T) {
	calls := []Call{
		call(0, 100, 0, "discover", "--help"),
		call(2, 0, 40, "discover", "ballerinax/kafka", "clients"),
		call(1, 0, 30, "discover", "ballerinax/kafka", "client", "Nope"),
		call(0, 500, 0, "discover", "ballerinax/kafka", "client", "Producer"),
		call(3, 0, 60, "discover", "ballerinax/kafka", "client", "getTopicPartitions"),
		call(0, 0, 0, "version"),
	}
	summary := Summarize(calls, [][]string{{"ballerinax/kafka:4.6.6", "client", "Producer"}})
	if summary.Calls != 5 || summary.OtherBalCalls != 1 {
		t.Fatalf("calls %d other %d", summary.Calls, summary.OtherBalCalls)
	}
	if summary.Failed != 3 || summary.FailedByExit["2"] != 1 || summary.FailedByExit["1"] != 1 || summary.FailedByExit["3"] != 1 {
		t.Fatalf("failures %+v", summary.FailedByExit)
	}
	if summary.ErrorEpisodes != 2 || summary.RecoveredEpisodes != 1 {
		t.Fatalf("episodes %d recovered %d", summary.ErrorEpisodes, summary.RecoveredEpisodes)
	}
	if !summary.HelpFirst || summary.HelpCalls != 1 || summary.RootHelpCalls != 1 || summary.HelpBytes != 100 {
		t.Fatalf("help %+v", summary)
	}
	if summary.AnswerBytes != 500 || summary.ErrorBytes != 130 || summary.TotalBytes != 730 || summary.EstimatedTokens != 209 {
		t.Fatalf("bytes %+v", summary)
	}
	if summary.FirstValid || summary.FirstBeforeHelp || summary.FirstOnTarget == nil || *summary.FirstOnTarget {
		t.Fatalf("first command %+v", summary)
	}
	if strings.Join(summary.FirstCommand, " ") != "ballerinax/kafka clients" || summary.ToolMs != 50 || summary.Successful != 1 {
		t.Fatalf("first %v tool %d", summary.FirstCommand, summary.ToolMs)
	}
}

func TestMatchesIgnoresVersionFlagOrderAndOutput(t *testing.T) {
	target := []string{"method", "getTopicPartitions", "-p", "ballerinax/kafka:4.6.6", "--client", "Consumer"}
	same := [][]string{
		{"method", "getTopicPartitions", "--client", "Consumer", "--package", "ballerinax/kafka"},
		{"-p", "ballerinax/kafka:4.6.6", "method", "getTopicPartitions", "--client=Consumer", "--output", "json"},
	}
	for _, argv := range same {
		if !Matches(argv, target) {
			t.Errorf("%v should match", argv)
		}
	}
	if Matches([]string{"method", "getTopicPartitions", "-p", "ballerinax/kafka"}, target) {
		t.Error("a missing container flag must not match")
	}
	if !Matches([]string{"ballerinax/github", "client", "Client", "repos/:owner/:repo/issues", "post"},
		[]string{"ballerinax/github:6.1.0", "client", "Client", "repos/:owner/:repo/issues", "post"}) {
		t.Error("resource path must survive version stripping")
	}
}

func TestParseAndBypasses(t *testing.T) {
	calls, err := Parse(strings.NewReader(`{"seq":1,"argv":["discover","--help"],"exitCode":0}` + "\n\n" + `{"seq":2,"argv":["version"],"exitCode":0}` + "\n"))
	if err != nil || len(calls) != 2 || !calls[0].IsRootHelp() || calls[1].IsDiscover() {
		t.Fatalf("parse: %v %+v", err, calls)
	}
	if _, err := Parse(strings.NewReader("{broken\n")); err == nil {
		t.Fatal("malformed line must fail")
	}
	got := Bypasses([]string{"ls ~/.ballerina/repositories/local", "bal discover x", "curl https://example.com"})
	if len(got) != 2 {
		t.Fatalf("bypasses %v", got)
	}
}

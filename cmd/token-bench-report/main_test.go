package main

import "testing"

func TestParseOptionsAllowsOutputAfterPaths(t *testing.T) {
	parsed, err := parseOptions([]string{"first", "second", "--output", "custom.html"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.output != "custom.html" || len(parsed.paths) != 2 {
		t.Fatalf("unexpected options: %+v", parsed)
	}
}

func TestParseOptionsDefaultsOutput(t *testing.T) {
	parsed, err := parseOptions([]string{"results"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.output != "report.html" {
		t.Fatalf("unexpected output: %s", parsed.output)
	}
}

func TestParseOptionsCSVFlags(t *testing.T) {
	parsed, err := parseOptions([]string{"--csv", "runs.csv", "--summary-csv=summary.csv", "results"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.csv != "runs.csv" || parsed.summaryCSV != "summary.csv" || len(parsed.paths) != 1 {
		t.Fatalf("unexpected options %+v", parsed)
	}
	for _, arguments := range [][]string{{"--csv"}, {"--csv=", "x"}, {"--summary-csv", ""}} {
		if _, err := parseOptions(arguments); err == nil {
			t.Errorf("parseOptions(%q) unexpectedly succeeded", arguments)
		}
	}
}

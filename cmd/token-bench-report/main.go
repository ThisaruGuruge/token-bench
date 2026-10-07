package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"token-bench/report"
)

const usage = `usage: token-bench-report [--output=report.html] [--csv=runs.csv] [--summary-csv=summary.csv] <path> [<path> ...]

Recursively discovers result.json files under each path and generates an HTML report.
--csv writes one row per run and --summary-csv the arm x scenario summary (pass k/n,
median tokens, cost and duration), both for spreadsheets. The HTML report always
includes the summary table.
`

type options struct {
	output     string
	csv        string
	summaryCSV string
	paths      []string
}

func main() {
	configuration, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := generateFile(configuration.paths, configuration.output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("HTML report:", configuration.output)
	if configuration.csv != "" {
		if err := writeCSV(configuration.csv, func(w io.Writer) error { return report.WriteRunsCSV(configuration.paths, w) }); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Runs CSV:", configuration.csv)
	}
	if configuration.summaryCSV != "" {
		if err := writeCSV(configuration.summaryCSV, func(w io.Writer) error { return report.WriteSummaryCSV(configuration.paths, w) }); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Summary CSV:", configuration.summaryCSV)
	}
}

func parseOptions(arguments []string) (options, error) {
	parsed := options{output: "report.html"}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "-h" || argument == "--help":
			fmt.Print(usage)
			os.Exit(0)
		case argument == "--output":
			index++
			if index == len(arguments) || arguments[index] == "" {
				return options{}, errors.New("--output requires a value")
			}
			parsed.output = arguments[index]
		case strings.HasPrefix(argument, "--output="):
			parsed.output = strings.TrimPrefix(argument, "--output=")
			if parsed.output == "" {
				return options{}, errors.New("--output requires a value")
			}
		case argument == "--csv" || argument == "--summary-csv":
			index++
			if index == len(arguments) || arguments[index] == "" {
				return options{}, fmt.Errorf("%s requires a value", argument)
			}
			if argument == "--csv" {
				parsed.csv = arguments[index]
			} else {
				parsed.summaryCSV = arguments[index]
			}
		case strings.HasPrefix(argument, "--csv=") || strings.HasPrefix(argument, "--summary-csv="):
			name, value, _ := strings.Cut(argument, "=")
			if value == "" {
				return options{}, fmt.Errorf("%s requires a value", name)
			}
			if name == "--csv" {
				parsed.csv = value
			} else {
				parsed.summaryCSV = value
			}
		case strings.HasPrefix(argument, "-"):
			return options{}, fmt.Errorf("unknown option %q", argument)
		default:
			parsed.paths = append(parsed.paths, argument)
		}
	}
	if len(parsed.paths) == 0 {
		return options{}, errors.New("at least one result path is required")
	}
	return parsed, nil
}

func writeCSV(path string, write func(io.Writer) error) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return err
	}
	file, err := os.Create(absolute)
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func generateFile(paths []string, outputPath string) error {
	absolute, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(absolute), ".token-bench-report-*.html")
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := report.Generate(paths, temporary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	if err := os.Rename(temporaryPath, absolute); err != nil {
		return fmt.Errorf("save report: %w", err)
	}
	return nil
}

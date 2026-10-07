package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"token-bench/harness"
)

func fakeToolchain(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	bal := "#!/bin/sh\ncase \"$1 $2\" in\n\"version \") echo 'Ballerina 2201.13.6 (Swan Lake Update 13)';;\n\"tool list\") printf '|TOOL ID|VERSION|\\n|discover|0.1.0 [repo = local]|\\n';;\nesac\n"
	claude := "#!/bin/sh\necho '2.1.291 (Claude Code)'\n"
	for name, content := range map[string]string{"bal": bal, "claude": claude} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func repoArmsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("arms")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseArmMatrixFlags(t *testing.T) {
	parsed, err := parseOptions([]string{"checkout-sync", "ballerina", "claude", "--n-runs=1", "--run-number", "2", "--requested-runs=3",
		"--max-budget-usd=7.5", "--arms-dir", "a", "--skills-repo=/s"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.runNumber != 2 || parsed.requestedRuns != 3 || parsed.maxBudgetUSD != 7.5 || parsed.armsDir != "a" || parsed.skillsRepo != "/s" {
		t.Fatalf("unexpected %+v", parsed)
	}
	defaults, err := parseOptions([]string{"claim-check", "go", "claude", "--n-runs=4"})
	if err != nil || defaults.runNumber != 1 || defaults.requestedRuns != 4 || defaults.maxBudgetUSD != 0 {
		t.Fatalf("defaults wrong: %+v %v", defaults, err)
	}
	for _, arguments := range [][]string{
		{"claim-check", "go", "claude", "--max-budget-usd=0"},
		{"claim-check", "go", "claude", "--max-budget-usd=x"},
		{"claim-check", "go", "pi", "--max-budget-usd=5"},
		{"claim-check", "go", "claude", "--run-number=0"},
		{"claim-check", "go", "claude", "--run-number=3", "--requested-runs=2"},
		{"claim-check", "go", "claude", "--arms-dir"},
	} {
		if _, err := parseOptions(arguments); err == nil {
			t.Errorf("parseOptions(%q) unexpectedly succeeded", arguments)
		}
	}
}

func TestArmSetupAppliesDiscoverHintAndColdCache(t *testing.T) {
	fakeToolchain(t)
	configuration := options{harness: "claude", arm: "C", armsDir: repoArmsDir(t)}
	setup, err := newArmSetup(configuration, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if setup.arm == nil || !strings.Contains(setup.arm.ExtraSystemPrompt, "bal discover <org>/<package>") {
		t.Fatalf("hint not loaded: %+v", setup.arm)
	}
	run, err := setup.prepareRun()
	if err != nil {
		t.Fatal(err)
	}
	defer run.cleanup()
	joined := strings.Join(run.envEntries(), "\n")
	if !strings.Contains(joined, "XDG_CACHE_HOME=") || strings.Contains(joined, "PATH=") {
		t.Errorf("arm C env wrong:\n%s", joined)
	}
	environment := setup.environment(configuration, harnessConfigForTest(0))
	if !environment.DiscoverVisible || !environment.ColdDiscoverCache || environment.BalDiscover != "0.1.0 [repo = local]" ||
		environment.BalVersion != "Ballerina 2201.13.6 (Swan Lake Update 13)" || environment.ClaudeVersion != "2.1.291 (Claude Code)" ||
		environment.MaxBudgetUSD != 20 || environment.ExtraSystemPrompt == "" {
		t.Fatalf("unexpected environment %+v", environment)
	}
}

func TestArmSetupHidesDiscoverForBareArm(t *testing.T) {
	fakeToolchain(t)
	setup, err := newArmSetup(options{harness: "claude", arm: "A", armsDir: repoArmsDir(t)}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := setup.prepareRun()
	if err != nil {
		t.Fatal(err)
	}
	defer run.cleanup()
	if !strings.Contains(strings.Join(run.envEntries(), "\n"), "PATH="+run.env.ShimDir) {
		t.Errorf("shim not on PATH: %v", run.envEntries())
	}
	if environment := setup.environment(options{harness: "claude"}, harnessConfigForTest(12.5)); environment.DiscoverVisible || environment.MaxBudgetUSD != 12.5 {
		t.Errorf("unexpected environment %+v", environment)
	}
}

func TestArmSetupAdHocLabelAndConflicts(t *testing.T) {
	fakeToolchain(t)
	adhoc, err := newArmSetup(options{harness: "claude", arm: "my-experiment", armsDir: repoArmsDir(t)}, t.TempDir())
	if err != nil || adhoc.arm != nil {
		t.Fatalf("ad-hoc label should resolve to no arm: %+v %v", adhoc.arm, err)
	}
	run, err := adhoc.prepareRun()
	if err != nil || len(run.envEntries()) != 0 {
		t.Fatalf("ad-hoc label must not change the environment: %v %v", run.envEntries(), err)
	}
	if _, err := newArmSetup(options{harness: "pi", arm: "A", armsDir: repoArmsDir(t)}, t.TempDir()); err == nil {
		t.Error("arm with the pi harness accepted")
	}
	if _, err := newArmSetup(options{harness: "claude", arm: "B", armsDir: repoArmsDir(t), skillsDir: "/x"}, t.TempDir()); err == nil {
		t.Error("--skills together with an arm accepted")
	}
}

func TestArmSetupResolvesPluginAndPatchedSkillsFromConfiguredRepo(t *testing.T) {
	fakeToolchain(t)
	repo := filepath.Join(t.TempDir(), "ballerina-skills")
	if err := os.MkdirAll(filepath.Join(repo, "skills", "ballerina"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "skills", "ballerina", "SKILL.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setup, err := newArmSetup(options{harness: "claude", arm: "E", armsDir: repoArmsDir(t), skillsRepo: repo}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if setup.arm.PluginDir != repo || setup.displaySkills() != repo || len(setup.arm.Warnings) == 0 {
		t.Fatalf("plugin arm wrong (a warning about the unknown commit is expected): %+v", setup.arm)
	}
	if _, err := newArmSetup(options{harness: "claude", arm: "D", armsDir: repoArmsDir(t), skillsRepo: repo}, t.TempDir()); err == nil {
		t.Error("the committed patch must not apply to an unrelated SKILL.md")
	}
}

func harnessConfigForTest(budget float64) harness.Config {
	return harness.Config{MaxBudgetUSD: budget}
}

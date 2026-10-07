package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"token-bench/arm"
	"token-bench/harness"
	benchresult "token-bench/result"
)

const defaultSkillsRepoSuffix = "Projects/Ballerina/cli-improvements/benchmark/reference/ballerina-skills"

// armSetup holds what is resolved once per invocation: the selected arm (nil
// for an ad-hoc label or no arm) and the versions of the toolchain.
type armSetup struct {
	arm      *arm.Resolved
	realBal  string
	versions arm.Versions
	harness  string
}

// armRun is the per-run side of an arm; nil-safe so callers need no checks.
type armRun struct{ env *arm.RunEnv }

func (r armRun) envEntries() []string {
	if r.env == nil {
		return nil
	}
	return r.env.Env
}

func (r armRun) cleanup() error { return r.env.Cleanup() }

func armsDirectory(configuration options) string {
	if configuration.armsDir != "" {
		return configuration.armsDir
	}
	if fromEnv := os.Getenv("TOKEN_BENCH_ARMS_DIR"); fromEnv != "" {
		return fromEnv
	}
	return "arms"
}

func skillsRepository(configuration options) string {
	if configuration.skillsRepo != "" {
		return configuration.skillsRepo
	}
	if fromEnv := os.Getenv("TOKEN_BENCH_SKILLS_REPO"); fromEnv != "" {
		return fromEnv
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, defaultSkillsRepoSuffix)
}

// newArmSetup resolves --arm against the arm definitions and captures versions.
func newArmSetup(configuration options, target string) (*armSetup, error) {
	setup := &armSetup{harness: configuration.harness}
	if configuration.harness == "claude" {
		setup.realBal, _ = arm.FindRealBal(os.Getenv("PATH"))
		claudePath, _ := exec.LookPath("claude")
		setup.versions = arm.CaptureVersions(arm.ExecRunner, setup.realBal, claudePath)
	}
	if configuration.arm == "" {
		return setup, nil
	}
	dir := armsDirectory(configuration)
	if !arm.Exists(dir, configuration.arm) {
		fmt.Fprintf(os.Stderr, "note: no arm definition %q in %s; using it as an ad-hoc label with no behavior attached\n", configuration.arm, dir)
		return setup, nil
	}
	if configuration.harness != "claude" {
		return nil, fmt.Errorf("arm %s is defined for the claude harness only", configuration.arm)
	}
	if configuration.skillsDir != "" {
		return nil, errors.New("--skills conflicts with an arm definition; the arm chooses its own skills")
	}
	config, err := arm.Load(dir, configuration.arm)
	if err != nil {
		return nil, err
	}
	if !config.DiscoverVisible && setup.realBal == "" {
		return nil, errors.New("arm hides bal discover but bal was not found on PATH to forward to")
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	resolved, err := arm.Resolve(config, arm.ResolveOptions{
		ArmsDir:    absoluteDir,
		SkillsRepo: skillsRepository(configuration),
		StageDir:   filepath.Join(target, ".token-bench-arms", config.ID),
	})
	if err != nil {
		return nil, err
	}
	setup.arm = &resolved
	return setup, nil
}

// displaySkills is the path recorded as the run's skills directory.
func (s *armSetup) displaySkills() string {
	if s.arm.PluginDir != "" {
		return s.arm.PluginDir
	}
	return s.arm.SkillsDir
}

func (s *armSetup) prepareRun() (armRun, error) {
	if s.arm == nil {
		return armRun{}, nil
	}
	env, err := arm.PrepareRun(*s.arm, s.realBal, os.Getenv("PATH"))
	return armRun{env: env}, err
}

// environment is the record stored in result.json; nil for non-Claude harnesses.
func (s *armSetup) environment(configuration options, config harness.Config) *benchresult.Environment {
	if s.harness != "claude" {
		return nil
	}
	budget := config.MaxBudgetUSD
	if budget == 0 {
		budget = harness.DefaultMaxBudgetUSD
	}
	environment := &benchresult.Environment{
		BalVersion:      s.versions.Bal,
		BalDiscover:     s.versions.Discover,
		ClaudeVersion:   s.versions.Claude,
		DiscoverVisible: s.versions.Discover != "not installed",
		MaxBudgetUSD:    budget,
	}
	if s.arm != nil {
		environment.DiscoverVisible = s.arm.DiscoverVisible && environment.DiscoverVisible
		environment.ColdDiscoverCache = true
		environment.SkillsRepoCommit = s.arm.SkillsRepoActual
		environment.SkillsHash = s.arm.SkillsHash
		environment.ExtraSystemPrompt = s.arm.ExtraSystemPrompt
	}
	return environment
}

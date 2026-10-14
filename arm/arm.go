// Package arm loads and resolves benchmark arm definitions: which skills or
// plugin the agent gets, which extra system prompt, and whether `bal discover`
// is reachable.
package arm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Config is one arm definition as stored under arms/<ID>.json.
type Config struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Skills is a skills directory relative to the skills repository root. It is
	// loaded as the harness' temporary skills plugin.
	Skills string `json:"skills,omitempty"`
	// SkillsPatch is a patch file, relative to the arms directory, applied with
	// `patch -p1` to a staged copy of the skills repository's Skills directory.
	// Its paths are relative to the skills repository root.
	SkillsPatch string `json:"skillsPatch,omitempty"`
	// Plugin loads the whole skills repository root as a Claude Code plugin
	// (skill, agents, MCP server, hooks, LSP). Mutually exclusive with Skills.
	Plugin bool `json:"plugin,omitempty"`
	// SystemPromptFile is appended to the harness' base system prompt; relative
	// to the arms directory.
	SystemPromptFile string `json:"systemPromptFile,omitempty"`
	// DiscoverVisible keeps `bal discover` working; when false a PATH shim
	// makes it fail like an unknown command.
	DiscoverVisible bool `json:"discoverVisible"`
	// Env is extra environment for the agent process.
	Env map[string]string `json:"env,omitempty"`
	// SkillsRepoCommit is the skills repository commit the arm was designed
	// against; a different checkout produces a warning, not an error.
	SkillsRepoCommit string `json:"skillsRepoCommit,omitempty"`
	// Container runs the agent in a fresh container of this image per run
	// instead of on the host.
	Container *Container `json:"container,omitempty"`
	// CallLog logs every `bal` call through the image's bal-calllog wrapper
	// and summarizes it into the result. Container arms only.
	CallLog bool `json:"callLog,omitempty"`
	// CacheSeed is a pre-warmed discover cache directory (the bal-discover
	// directory itself), copied into each run's private cache. $VAR and a
	// leading ~ are expanded; a relative path is relative to the arms directory.
	CacheSeed string `json:"cacheSeed,omitempty"`
	// DisallowedTools are denied to the agent in addition to the harness default.
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	// Grammar names the discover grammar the arm's build speaks ("A" or "B"),
	// selecting a question task's on-target first commands.
	Grammar string `json:"grammar,omitempty"`
}

// Container is the image a container arm runs in.
type Container struct {
	Image string `json:"image"`
}

// Validate checks the definition's internal consistency.
func (c Config) Validate() error {
	if c.ID == "" {
		return errors.New("arm id is required")
	}
	if strings.ContainsAny(c.ID, `/\ `) {
		return fmt.Errorf("arm id %q must not contain slashes or spaces", c.ID)
	}
	if c.Plugin && c.Skills != "" {
		return fmt.Errorf("arm %s: plugin and skills are mutually exclusive", c.ID)
	}
	if c.SkillsPatch != "" && c.Skills == "" {
		return fmt.Errorf("arm %s: skillsPatch requires skills", c.ID)
	}
	if c.Container != nil {
		if c.Container.Image == "" {
			return fmt.Errorf("arm %s: container.image is required", c.ID)
		}
		if c.UsesSkillsRepo() {
			return fmt.Errorf("arm %s: a container arm cannot load skills or plugins from the host", c.ID)
		}
		if !c.DiscoverVisible {
			return fmt.Errorf("arm %s: a container arm's image decides what is installed; discoverVisible must be true", c.ID)
		}
	}
	if (c.CallLog || c.CacheSeed != "") && c.Container == nil {
		return fmt.Errorf("arm %s: callLog and cacheSeed need a container arm", c.ID)
	}
	return nil
}

// UsesSkillsRepo reports whether the arm reads from the skills repository.
func (c Config) UsesSkillsRepo() bool { return c.Plugin || c.Skills != "" }

// Load reads arms/<id>.json from dir.
func Load(dir, id string) (Config, error) {
	content, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return Config{}, err
	}
	return decode(content, id)
}

func decode(content []byte, wantID string) (Config, error) {
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode arm %s: %w", wantID, err)
	}
	if config.ID != wantID {
		return Config{}, fmt.Errorf("arm file %s.json declares id %q", wantID, config.ID)
	}
	return config, config.Validate()
}

// Exists reports whether dir holds a definition for id.
func Exists(dir, id string) bool {
	if id == "" || id == "phases" || strings.ContainsAny(id, `/\`) {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, id+".json"))
	return err == nil && !info.IsDir()
}

// List returns the ids of every arm defined in dir, sorted.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || name == "phases.json" {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

// Resolved is an arm with every path made absolute and every file checked.
type Resolved struct {
	Config
	// SkillsDir is the directory passed to the harness' skills plugin; for a
	// patched arm it is a staged copy.
	SkillsDir string
	// PluginDir is a whole plugin directory for the harness, or empty.
	PluginDir string
	// ExtraSystemPrompt is the text appended to the base system prompt.
	ExtraSystemPrompt string
	// SkillsRepoActual is the skills repository commit actually found.
	SkillsRepoActual string
	// SkillsHash is a content hash of the exact tree the agent receives.
	SkillsHash string
	// CacheSeedDir is the absolute seed directory; CacheSeedHash hashes it
	// with its latest-version timestamps zeroed, so a re-stamp is not a change.
	CacheSeedDir   string
	CacheSeedHash  string
	CacheSeedFiles int
	Warnings       []string
}

// ResolveOptions locate the inputs of an arm.
type ResolveOptions struct {
	ArmsDir    string
	SkillsRepo string
	// StageDir receives patched copies; required only for arms with a patch.
	StageDir string
}

// Resolve checks the arm's inputs and stages anything that needs staging.
func Resolve(config Config, options ResolveOptions) (Resolved, error) {
	if err := config.Validate(); err != nil {
		return Resolved{}, err
	}
	resolved := Resolved{Config: config}
	if config.SystemPromptFile != "" {
		content, err := os.ReadFile(filepath.Join(options.ArmsDir, config.SystemPromptFile))
		if err != nil {
			return Resolved{}, fmt.Errorf("arm %s: read system prompt: %w", config.ID, err)
		}
		resolved.ExtraSystemPrompt = strings.TrimSpace(string(content))
	}
	if config.CacheSeed != "" {
		seed := expandPath(config.CacheSeed, options.ArmsDir)
		dir, err := absoluteDir(seed)
		if err != nil {
			return Resolved{}, fmt.Errorf("arm %s: cache seed: %w", config.ID, err)
		}
		resolved.CacheSeedDir = dir
		if resolved.CacheSeedHash, resolved.CacheSeedFiles, err = HashSeed(dir); err != nil {
			return Resolved{}, fmt.Errorf("arm %s: cache seed: %w", config.ID, err)
		}
	}
	if !config.UsesSkillsRepo() {
		return resolved, nil
	}
	repo, err := absoluteDir(options.SkillsRepo)
	if err != nil {
		return Resolved{}, fmt.Errorf("arm %s: skills repository: %w", config.ID, err)
	}
	actual, source := RepoCommit(repo)
	resolved.SkillsRepoActual = actual
	if config.SkillsRepoCommit != "" && !commitMatches(config.SkillsRepoCommit, actual) {
		resolved.Warnings = append(resolved.Warnings, fmt.Sprintf(
			"arm %s expects skills repository commit %s but %s reports %s", config.ID, config.SkillsRepoCommit, source, orUnknown(actual)))
	}
	switch {
	case config.Plugin:
		resolved.PluginDir = repo
		resolved.SkillsHash, err = HashTree(repo)
	case config.SkillsPatch == "":
		resolved.SkillsDir, err = absoluteDir(filepath.Join(repo, config.Skills))
		if err == nil {
			resolved.SkillsHash, err = HashTree(resolved.SkillsDir)
		}
	default:
		resolved.SkillsDir, err = stagePatchedSkills(repo, config, options)
		if err == nil {
			resolved.SkillsHash, err = HashTree(resolved.SkillsDir)
		}
	}
	if err != nil {
		return Resolved{}, fmt.Errorf("arm %s: %w", config.ID, err)
	}
	return resolved, nil
}

// expandPath expands $VARS and a leading ~, and resolves a relative path against base.
func expandPath(path, base string) string {
	expanded := os.ExpandEnv(path)
	if rest, found := strings.CutPrefix(expanded, "~/"); found {
		home, _ := os.UserHomeDir()
		expanded = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(base, expanded)
	}
	return expanded
}

func orUnknown(value string) string {
	if value == "" {
		return "no commit"
	}
	return value
}

func commitMatches(want, got string) bool {
	return got != "" && (strings.HasPrefix(got, want) || strings.HasPrefix(want, got))
}

func absoluteDir(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", absolute)
	}
	return filepath.Clean(absolute), nil
}

// stagePatchedSkills copies repo/<skills> into StageDir/<skills>, applies the
// patch there and returns the staged skills directory. The staging directory is
// rebuilt on every call so a stale copy can never be used.
func stagePatchedSkills(repo string, config Config, options ResolveOptions) (string, error) {
	if options.StageDir == "" {
		return "", errors.New("a staging directory is required for a patched skills arm")
	}
	patchFile, err := filepath.Abs(filepath.Join(options.ArmsDir, config.SkillsPatch))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(patchFile); err != nil {
		return "", fmt.Errorf("skills patch: %w", err)
	}
	if err := os.RemoveAll(options.StageDir); err != nil {
		return "", err
	}
	staged := filepath.Join(options.StageDir, config.Skills)
	if err := copyTree(filepath.Join(repo, config.Skills), staged); err != nil {
		return "", fmt.Errorf("stage skills: %w", err)
	}
	patchInput, err := os.Open(patchFile)
	if err != nil {
		return "", err
	}
	defer patchInput.Close()
	command := exec.Command("patch", "-p1", "--no-backup-if-mismatch", "-d", options.StageDir)
	command.Stdin = patchInput
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("apply %s to the skills copy (wrong skills repository commit?): %w\n%s", config.SkillsPatch, err, strings.TrimSpace(string(output)))
	}
	return staged, nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, content, 0o644)
		}
	})
}

// HashTree returns a sha256 over the relative paths and contents of every file
// below root, ignoring .git, so the exact tree an arm used can be recorded even
// when the checkout carries no git metadata.
func HashTree(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", filepath.ToSlash(relative), len(content))
		hash.Write(content)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil))[:16], nil
}

// RepoCommit returns the commit of the skills repository and where it was
// learned: the checkout's own git metadata, or a SOURCES.txt line
// ("<dirname> <sha>") in its parent directory for a snapshot without .git.
func RepoCommit(repo string) (commit, source string) {
	if _, err := os.Stat(filepath.Join(repo, ".git")); err == nil {
		output, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err == nil {
			return strings.TrimSpace(string(output)), "git"
		}
	}
	content, err := os.ReadFile(filepath.Join(filepath.Dir(repo), "SOURCES.txt"))
	if err != nil {
		return "", "no git metadata"
	}
	name := filepath.Base(repo)
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			return fields[1], "SOURCES.txt"
		}
	}
	return "", "no git metadata"
}

const defaultSkillsRepoSuffix = "Projects/Ballerina/cli-improvements/benchmark/reference/ballerina-skills"

// ArmsDirectory resolves the arms directory: the flag, $TOKEN_BENCH_ARMS_DIR, then ./arms.
func ArmsDirectory(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if fromEnv := os.Getenv("TOKEN_BENCH_ARMS_DIR"); fromEnv != "" {
		return fromEnv
	}
	return "arms"
}

// SkillsRepository resolves the skills checkout: the flag, $TOKEN_BENCH_SKILLS_REPO,
// then the pinned reference snapshot under the home directory.
func SkillsRepository(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if fromEnv := os.Getenv("TOKEN_BENCH_SKILLS_REPO"); fromEnv != "" {
		return fromEnv
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, defaultSkillsRepoSuffix)
}

package arm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = ".."

func armsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join(repoRoot, "arms"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCommittedArmsLoadAndValidate(t *testing.T) {
	dir := armsDir(t)
	ids, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "A,B,C,D,E" {
		t.Fatalf("unexpected arms %v", ids)
	}
	want := map[string]struct{ discover, plugin bool }{
		"A": {false, false}, "B": {false, false}, "C": {true, false}, "D": {true, false}, "E": {false, true},
	}
	for _, id := range ids {
		config, err := Load(dir, id)
		if err != nil {
			t.Fatalf("arm %s: %v", id, err)
		}
		if config.DiscoverVisible != want[id].discover || config.Plugin != want[id].plugin {
			t.Errorf("arm %s: discover=%v plugin=%v", id, config.DiscoverVisible, config.Plugin)
		}
	}
}

func TestCommittedHintIsThreeShortLines(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(armsDir(t), "hints", "discover-hint.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 3 {
		t.Fatalf("hint has %d lines", len(lines))
	}
	for _, line := range lines {
		if len(line) > 90 {
			t.Errorf("line too long: %q", line)
		}
	}
}

func TestPhasesReferenceKnownArms(t *testing.T) {
	dir := armsDir(t)
	phases, err := LoadPhases(dir)
	if err != nil {
		t.Fatal(err)
	}
	phase, ok := phases["phase0"]
	if !ok || strings.Join(phase.Arms, ",") != "A,D" || phase.Runs != 3 || phase.Scenarios[0] != "checkout-sync" {
		t.Fatalf("unexpected phase0 %+v", phase)
	}
	for name, phase := range phases {
		for _, id := range phase.Arms {
			if !Exists(dir, id) {
				t.Errorf("%s references unknown arm %s", name, id)
			}
		}
	}
	if Exists(dir, "phases") {
		t.Error("phases.json must not be treated as an arm")
	}
}

func TestValidateRejectsInconsistentConfigs(t *testing.T) {
	for name, config := range map[string]Config{
		"no id":             {},
		"slash":             {ID: "a/b"},
		"plugin and skills": {ID: "X", Plugin: true, Skills: "skills"},
		"patch no skills":   {ID: "X", SkillsPatch: "p.patch"},
	} {
		if config.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDecodeRejectsUnknownFieldsAndWrongID(t *testing.T) {
	if _, err := decode([]byte(`{"id":"A","bogus":1}`), "A"); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := decode([]byte(`{"id":"B"}`), "A"); err == nil {
		t.Error("mismatched id accepted")
	}
}

func fakeSkillsRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "ballerina-skills")
	if err := os.MkdirAll(filepath.Join(repo, "skills", "ballerina"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "skills", "ballerina", "SKILL.md"), "line one\nuse the library agent\nline three\n")
	return repo
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func requirePatch(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("patch not installed")
	}
}

func TestResolveStagesPatchedSkillsWithoutTouchingTheRepo(t *testing.T) {
	requirePatch(t)
	repo := fakeSkillsRepo(t)
	arms := t.TempDir()
	write(t, filepath.Join(arms, "p.patch"), `--- a/skills/ballerina/SKILL.md
+++ b/skills/ballerina/SKILL.md
@@ -1,3 +1,3 @@
 line one
-use the library agent
+use bal discover
 line three
`)
	config := Config{ID: "D", Skills: "skills", SkillsPatch: "p.patch"}
	stage := filepath.Join(t.TempDir(), "stage")
	resolved, err := Resolve(config, ResolveOptions{ArmsDir: arms, SkillsRepo: repo, StageDir: stage})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SkillsDir != filepath.Join(stage, "skills") {
		t.Fatalf("SkillsDir = %s", resolved.SkillsDir)
	}
	patched, _ := os.ReadFile(filepath.Join(resolved.SkillsDir, "ballerina", "SKILL.md"))
	if !strings.Contains(string(patched), "use bal discover") {
		t.Fatalf("patch not applied: %s", patched)
	}
	original, _ := os.ReadFile(filepath.Join(repo, "skills", "ballerina", "SKILL.md"))
	if !strings.Contains(string(original), "library agent") {
		t.Fatal("the skills repository itself was modified")
	}
	plain, err := Resolve(Config{ID: "B", Skills: "skills"}, ResolveOptions{SkillsRepo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if plain.SkillsHash == resolved.SkillsHash || resolved.SkillsHash == "" {
		t.Errorf("hash should differ between plain %q and patched %q skills", plain.SkillsHash, resolved.SkillsHash)
	}
	if _, err := Resolve(config, ResolveOptions{ArmsDir: arms, SkillsRepo: repo, StageDir: stage}); err != nil {
		t.Fatalf("restaging over an existing stage failed: %v", err)
	}
}

func TestResolveFailsClearlyWhenThePatchDoesNotApply(t *testing.T) {
	requirePatch(t)
	repo := fakeSkillsRepo(t)
	arms := t.TempDir()
	write(t, filepath.Join(arms, "p.patch"), `--- a/skills/ballerina/SKILL.md
+++ b/skills/ballerina/SKILL.md
@@ -1,3 +1,3 @@
 something else
-entirely
+different
 here
`)
	_, err := Resolve(Config{ID: "D", Skills: "skills", SkillsPatch: "p.patch"},
		ResolveOptions{ArmsDir: arms, SkillsRepo: repo, StageDir: filepath.Join(t.TempDir(), "s")})
	if err == nil || !strings.Contains(err.Error(), "wrong skills repository commit") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestCommittedPatchAppliesToTheReferenceSnapshot(t *testing.T) {
	requirePatch(t)
	repo := os.Getenv("TOKEN_BENCH_SKILLS_REPO")
	if repo == "" {
		repo = filepath.Join(os.Getenv("HOME"), "Projects/Ballerina/cli-improvements/benchmark/reference/ballerina-skills")
	}
	if _, err := os.Stat(repo); err != nil {
		t.Skip("skills repository not available")
	}
	config, err := Load(armsDir(t), "D")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(config, ResolveOptions{ArmsDir: armsDir(t), SkillsRepo: repo, StageDir: filepath.Join(t.TempDir(), "s")})
	if err != nil {
		t.Fatal(err)
	}
	skill, _ := os.ReadFile(filepath.Join(resolved.SkillsDir, "ballerina", "SKILL.md"))
	if strings.Contains(string(skill), "`library` agent") || !strings.Contains(string(skill), "bal discover") {
		t.Error("patched SKILL.md still points at the library agent")
	}
	messaging, _ := os.ReadFile(filepath.Join(resolved.SkillsDir, "ballerina", "rules", "messaging.md"))
	if strings.Contains(string(messaging), "`library` agent") {
		t.Error("patched messaging.md still mentions the library agent")
	}
}

func TestResolvePluginAndCommitChecks(t *testing.T) {
	repo := fakeSkillsRepo(t)
	write(t, filepath.Join(filepath.Dir(repo), "SOURCES.txt"), "ballerina-skills abc1234def\n")
	resolved, err := Resolve(Config{ID: "E", Plugin: true, SkillsRepoCommit: "abc1234def"}, ResolveOptions{SkillsRepo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.PluginDir != repo || resolved.SkillsDir != "" || resolved.SkillsRepoActual != "abc1234def" || len(resolved.Warnings) != 0 {
		t.Fatalf("unexpected %+v", resolved)
	}
	mismatch, err := Resolve(Config{ID: "E", Plugin: true, SkillsRepoCommit: "ffffff"}, ResolveOptions{SkillsRepo: repo})
	if err != nil || len(mismatch.Warnings) != 1 {
		t.Fatalf("expected one warning, got %+v, %v", mismatch.Warnings, err)
	}
	if _, err := Resolve(Config{ID: "E", Plugin: true}, ResolveOptions{SkillsRepo: filepath.Join(repo, "missing")}); err == nil {
		t.Error("missing skills repository accepted")
	}
}

func TestResolveReadsSystemPromptAndNeedsNoRepoForBareArms(t *testing.T) {
	arms := t.TempDir()
	write(t, filepath.Join(arms, "hint.md"), "\nhello\n\n")
	resolved, err := Resolve(Config{ID: "C", SystemPromptFile: "hint.md", DiscoverVisible: true}, ResolveOptions{ArmsDir: arms})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ExtraSystemPrompt != "hello" || resolved.SkillsDir != "" || resolved.PluginDir != "" {
		t.Fatalf("unexpected %+v", resolved)
	}
	if _, err := Resolve(Config{ID: "C", SystemPromptFile: "missing.md"}, ResolveOptions{ArmsDir: arms}); err == nil {
		t.Error("missing prompt file accepted")
	}
}

func TestHashTreeIgnoresGitAndDetectsChanges(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.md"), "one")
	first, _ := HashTree(root)
	write(t, filepath.Join(root, ".git", "HEAD"), "x")
	again, _ := HashTree(root)
	write(t, filepath.Join(root, "a.md"), "two")
	changed, _ := HashTree(root)
	if first != again || first == changed {
		t.Fatalf("hashes: %s %s %s", first, again, changed)
	}
}

package arm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSeed(t *testing.T) string {
	t.Helper()
	seed := t.TempDir()
	files := map[string]string{
		"v2/docs/central/ballerinax/kafka/4.6.6.json": `{"docs":true}`,
		"v2/latest/central/ballerinax/aws.s3.json":    `{"version":"4.1.0","atMs":1}`,
		"v2/latest/central/ballerina/graphql.json":    `{"version":"1.19.0","atMs":2}`,
	}
	for name, content := range files {
		path := filepath.Join(seed, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return seed
}

func TestCopySeedRestampsLatestOnly(t *testing.T) {
	seed := writeSeed(t)
	destination := filepath.Join(t.TempDir(), "bal-discover")
	now := time.UnixMilli(1_800_000_000_000)
	files, err := CopySeed(seed, destination, now)
	if err != nil || files != 3 {
		t.Fatalf("copy: %d %v", files, err)
	}
	var entry map[string]any
	content, _ := os.ReadFile(filepath.Join(destination, "v2/latest/central/ballerinax/aws.s3.json"))
	if err := json.Unmarshal(content, &entry); err != nil || entry["version"] != "4.1.0" || entry["atMs"].(float64) != 1_800_000_000_000 {
		t.Fatalf("restamp: %s", content)
	}
	docs, _ := os.ReadFile(filepath.Join(destination, "v2/docs/central/ballerinax/kafka/4.6.6.json"))
	if string(docs) != `{"docs":true}` {
		t.Fatalf("docs changed: %s", docs)
	}
	fresh, err := NewCacheFiles(seed, destination)
	if err != nil || fresh != 0 {
		t.Fatalf("a re-stamped copy has no live fetches: %d %v", fresh, err)
	}
	if err := os.WriteFile(filepath.Join(destination, "v2/docs/new.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := NewCacheFiles(seed, destination); fresh != 1 {
		t.Fatalf("new file not counted: %d", fresh)
	}
}

func TestHashSeedIgnoresTimestamps(t *testing.T) {
	seed := writeSeed(t)
	first, files, err := HashSeed(seed)
	if err != nil || files != 3 {
		t.Fatal(files, err)
	}
	if _, err := CopySeed(seed, seed+"-copy", time.Now()); err != nil {
		t.Fatal(err)
	}
	second, _, _ := HashSeed(seed + "-copy")
	if first != second {
		t.Fatal("a re-stamp changed the seed hash")
	}
	if err := os.WriteFile(filepath.Join(seed, "v2/docs/central/ballerinax/kafka/4.6.6.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if third, _, _ := HashSeed(seed); third == first {
		t.Fatal("a content change kept the hash")
	}
}

func TestContainerArmValidationAndRun(t *testing.T) {
	bad := []Config{
		{ID: "X", CallLog: true},
		{ID: "X", Container: &Container{}, DiscoverVisible: true},
		{ID: "X", Container: &Container{Image: "i"}},
		{ID: "X", Container: &Container{Image: "i"}, DiscoverVisible: true, Plugin: true},
	}
	for _, config := range bad {
		if config.Validate() == nil {
			t.Errorf("%+v should be invalid", config)
		}
	}
	seed := writeSeed(t)
	config := Config{ID: "GA", Container: &Container{Image: "img"}, DiscoverVisible: true, CallLog: true,
		CacheSeed: seed, Env: map[string]string{"B": "2"}, Grammar: "A"}
	resolved, err := Resolve(config, ResolveOptions{ArmsDir: t.TempDir()})
	if err != nil || resolved.CacheSeedFiles != 3 || resolved.CacheSeedHash == "" {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	parent := t.TempDir()
	run, err := PrepareRunIn(resolved, "", "/usr/bin", parent)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Cleanup()
	if !strings.HasPrefix(run.Dir, parent) || run.Container == nil || run.Container.Image != "img" || run.SeedFiles != 3 {
		t.Fatalf("run %+v", run)
	}
	want := []string{"XDG_CACHE_HOME=/bench/xdg-cache", "TOKEN_BENCH_CALL_LOG=/bench/calls/calls.jsonl", "B=2"}
	if strings.Join(run.Env, " ") != strings.Join(want, " ") {
		t.Fatalf("env %v", run.Env)
	}
	if _, err := os.Stat(filepath.Join(run.CacheDir, "bal-discover", "v2/latest/central/ballerina/graphql.json")); err != nil {
		t.Fatal(err)
	}
}

package claude

import (
	"bytes"
	"strings"
	"testing"
)

func TestContainerArgumentsCarryNoKey(t *testing.T) {
	c := New(Config{
		Model: "haiku", APIKeyHelper: "printf secret-key-value",
		Container:       &Container{Image: "token-bench-discover:GA", Mounts: []string{"/h/bench:/bench"}, Env: []string{"XDG_CACHE_HOME=/bench/xdg-cache"}, WorkDir: "/work"},
		DisallowedTools: []string{"WebSearch", "WebFetch"},
	})
	arguments := append(c.config.Container.dockerArguments("token-bench-x", "/h/work"), c.startArguments()...)
	joined := strings.Join(arguments, " ")
	for _, want := range []string{"run --rm -i --init --name token-bench-x", "-v /h/work:/work -w /work", "-v /h/bench:/bench",
		"-e XDG_CACHE_HOME=/bench/xdg-cache", "-e DISABLE_AUTOUPDATER=1", "token-bench-discover:GA --model haiku",
		"--disallowedTools AskUserQuestion,WebSearch,WebFetch", `{"apiKeyHelper":"cat /run/bench-secret/key"}`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "secret-key-value") || strings.Contains(joined, "printf") {
		t.Fatal("the key or the host helper leaked into the docker command line")
	}
	for _, entry := range c.hostEnvironment([]string{"ANTHROPIC_API_KEY=x", "PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=y"}) {
		if !strings.HasPrefix(entry, "PATH=") {
			t.Fatalf("credential inherited by the docker client: %s", entry)
		}
	}
}

func TestReadAndSendKey(t *testing.T) {
	key, err := readAPIKey("printf ' k1 \\n'")
	if err != nil || string(key) != "k1" {
		t.Fatalf("key %q %v", key, err)
	}
	var stdin bytes.Buffer
	if err := sendKey(&stdin, key); err != nil || stdin.String() != "k1\n" {
		t.Fatalf("sent %q %v", stdin.String(), err)
	}
	if _, err := readAPIKey("true"); err == nil {
		t.Fatal("an empty helper must fail closed")
	}
	if _, err := readAPIKey("printf 'a\\nb'"); err == nil {
		t.Fatal("a multi-line key must be refused")
	}
	if _, err := readAPIKey("printf secret; exit 1"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("failing helper: %v", err)
	}
}

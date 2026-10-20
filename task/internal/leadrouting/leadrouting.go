package leadrouting

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed prompt.md
var prompt string

// Prompt is the scenario's task text.
func Prompt() string { return prompt }

const (
	kafkaImage     = "apache/kafka:3.9.1"
	mockImage      = "token-bench-mock:latest"
	topic          = "leads"
	consumerGroup  = "lead-mailer"
	appPort        = "8090"
	mockPort       = "8080"
	kafkaBin       = "/opt/kafka/bin/"
	startTimeout   = 4 * time.Minute
	readyTimeout   = 90 * time.Second
	mailWait       = 60 * time.Second
	consumerSettle = 12 * time.Second
	requestTimeout = 60 * time.Second
	platform       = "linux/arm64"
	salesAddress   = "sales@example.com"
	seededID       = "501"
	seededEmail    = "alice@example.com"
)

// mockServices are the sidecars besides Kafka; each is reachable on the run
// network under its own name.
var mockServices = []string{"hubspot", "gmail", "openai", "enrich"}

// Handle is one run of the scenario.
type Handle struct {
	workDir string
	image   string
	network string
	suffix  string
	// controls maps a mock to its control base URL on the host.
	controls   map[string]string
	containers []string
	log        io.Writer
}

// New prepares a run: image is the arm's integration image (it builds and runs
// the agent's program), network the per-run docker network the agent's
// container also joins.
func New(workDir, image, network string) *Handle {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return &Handle{workDir: workDir, image: image, network: network, suffix: hex.EncodeToString(suffix), controls: map[string]string{}, log: io.Discard}
}

// SetLog sends progress lines to writer.
func (h *Handle) SetLog(writer io.Writer) { h.log = writer }

func docker(ctx context.Context, arguments ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		return text, fmt.Errorf("docker %s: %w: %s", strings.Join(arguments[:min(3, len(arguments))], " "), err, text)
	}
	return text, nil
}

func (h *Handle) name(role string) string { return "tb-" + role + "-" + h.suffix }

// Setup creates the network, starts Kafka and the mocks, and waits for them.
func (h *Handle) Setup() (func() error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := docker(ctx, "network", "create", h.network); err != nil {
		return func() error { return nil }, err
	}
	cleanup := h.cleanup
	kafka := h.name("kafka")
	h.containers = append(h.containers, kafka)
	if _, err := docker(ctx, "run", "-d", "--rm", "--name", kafka, "--network", h.network, "--network-alias", "kafka",
		"-e", "KAFKA_NODE_ID=1", "-e", "KAFKA_PROCESS_ROLES=broker,controller",
		"-e", "KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093",
		"-e", "KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://kafka:9092",
		"-e", "KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT",
		"-e", "KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER", "-e", "KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093",
		"-e", "KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1", "-e", "KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1",
		"-e", "KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1", "-e", "KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0",
		kafkaImage); err != nil {
		return cleanup, err
	}
	for _, service := range mockServices {
		container := h.name(service)
		h.containers = append(h.containers, container)
		if _, err := docker(ctx, "run", "-d", "--rm", "--platform", platform, "--name", container, "--network", h.network,
			"--network-alias", service, "-p", "127.0.0.1::"+mockPort, mockImage, "-service", service); err != nil {
			return cleanup, err
		}
		address, err := docker(ctx, "port", container, mockPort)
		if err != nil {
			return cleanup, err
		}
		h.controls[service] = "http://" + strings.Split(address, "\n")[0]
	}
	for _, service := range mockServices {
		if err := waitHTTP(ctx, h.controls[service]+"/__health"); err != nil {
			return cleanup, fmt.Errorf("mock %s not ready: %w", service, err)
		}
	}
	if err := h.waitKafka(ctx); err != nil {
		return cleanup, err
	}
	if err := h.resetState(ctx); err != nil {
		return cleanup, err
	}
	return cleanup, nil
}

func (h *Handle) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var failures []string
	for _, container := range h.containers {
		if _, err := docker(ctx, "rm", "-f", container); err != nil && !strings.Contains(err.Error(), "No such container") {
			failures = append(failures, err.Error())
		}
	}
	if _, err := docker(ctx, "network", "rm", h.network); err != nil && !strings.Contains(err.Error(), "not found") {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func waitHTTP(ctx context.Context, url string) error {
	for {
		response, err := http.Get(url)
		if err == nil {
			response.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (h *Handle) kafkaTool(ctx context.Context, tool string, arguments ...string) (string, error) {
	return docker(ctx, append([]string{"exec", h.name("kafka"), kafkaBin + tool, "--bootstrap-server", "localhost:9092"}, arguments...)...)
}

func (h *Handle) waitKafka(ctx context.Context) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		if _, err := h.kafkaTool(ctx, "kafka-topics.sh", "--list"); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("kafka not ready: %w", err)
		}
		time.Sleep(time.Second)
	}
}

// resetState empties the topic and the consumer group and resets every mock,
// seeding HubSpot with one existing contact. It runs before the agent starts
// and again before grading, so the agent's own trials cannot leak into the grade.
func (h *Handle) resetState(ctx context.Context) error {
	_, _ = h.kafkaTool(ctx, "kafka-consumer-groups.sh", "--delete", "--group", consumerGroup)
	_, _ = h.kafkaTool(ctx, "kafka-topics.sh", "--delete", "--if-exists", "--topic", topic)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := h.kafkaTool(ctx, "kafka-topics.sh", "--create", "--if-not-exists", "--topic", topic, "--partitions", "1", "--replication-factor", "1")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("create topic: %w", err)
		}
		time.Sleep(time.Second)
	}
	seeds := []map[string]string{{"ID": seededID, "Email": seededEmail, "FirstName": "Alice", "LastName": "Archer"}}
	for _, service := range mockServices {
		body := []byte{}
		if service == "hubspot" {
			body, _ = json.Marshal(seeds)
		}
		response, err := http.Post(h.controls[service]+"/__reset", "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("reset %s: %w", service, err)
		}
		response.Body.Close()
	}
	return nil
}

// Prompt implements the task handle.
func (h *Handle) Prompt() (string, error) { return prompt, nil }

// Feedback is empty: the scenario is single shot.
func (h *Handle) Feedback() (string, error) { return "", nil }

// Validation reports whether every check passed.
func (h *Handle) Validation() (bool, error) {
	grade, err := h.Grade()
	return grade.Passed(), err
}

// InfraError marks a grading failure of the machinery, not of the agent.
type InfraError struct{ Err error }

func (e *InfraError) Error() string { return e.Err.Error() }

func projectDir(workDir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(workDir, "Ballerina.toml")); err == nil {
		return workDir, true
	}
	matches, _ := filepath.Glob(filepath.Join(workDir, "*", "Ballerina.toml"))
	if len(matches) == 1 {
		return filepath.Dir(matches[0]), true
	}
	return "", false
}

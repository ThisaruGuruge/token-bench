// Package checkoutsync implements the Checkout Sync task: a checkout endpoint that
// synchronizes a HubSpot contact, captures a PayPal order and records the payment in
// a Dockerized MySQL database. HubSpot and PayPal are benchmark-owned mocks.
package checkoutsync

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	requiredPorts = 4
	mysqlImage    = "mysql:8.0"
	databaseUser  = "tokenbench"
	databasePass  = "tokenbench"
	databaseName  = "tokenbench"
	paymentsTable = "payments"
)

func init() {
	// The driver logs every refused connection while the container is still starting.
	_ = mysql.SetLogger(log.New(io.Discard, "", 0))
}

//go:embed prompt.md.tmpl
var promptTemplate string

type checkoutSync struct {
	applicationPort int
	hubspotPort     int
	paypalPort      int
	databasePort    int
	containerName   string
	database        *sql.DB
	mocks           *mocks
	initErr         error
}

// PromptTemplate returns the unrendered task requirements.
func PromptTemplate() string {
	return promptTemplate
}

// New initializes the private Checkout Sync implementation.
func New(ports []int) *checkoutSync {
	handle := &checkoutSync{}
	if len(ports) != requiredPorts {
		handle.initErr = fmt.Errorf("checkout sync requires %d ports, got %d", requiredPorts, len(ports))
		return handle
	}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			handle.initErr = fmt.Errorf("invalid allocated port %d", port)
			return handle
		}
	}
	handle.applicationPort = ports[0]
	handle.hubspotPort = ports[1]
	handle.paypalPort = ports[2]
	handle.databasePort = ports[3]
	return handle
}

func (c *checkoutSync) Setup() (func() error, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	if c.containerName != "" || c.mocks != nil {
		return nil, errors.New("checkout sync is already set up")
	}
	c.containerName = fmt.Sprintf("token-bench-mysql-%d-%d", c.databasePort, time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "run", "--detach", "--rm", "--name", c.containerName,
		"--env", "MYSQL_ROOT_PASSWORD="+databasePass,
		"--env", "MYSQL_DATABASE="+databaseName,
		"--env", "MYSQL_USER="+databaseUser,
		"--env", "MYSQL_PASSWORD="+databasePass,
		"--publish", fmt.Sprintf("127.0.0.1:%d:3306", c.databasePort), mysqlImage,
		"--default-authentication-plugin=mysql_native_password").CombinedOutput()
	if err != nil {
		c.containerName = ""
		return nil, fmt.Errorf("start MySQL container: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := c.connectAndCreateTable(ctx); err != nil {
		_ = c.cleanup()
		return nil, err
	}
	c.mocks, err = startMocks(c.hubspotPort, c.paypalPort)
	if err != nil {
		_ = c.cleanup()
		return nil, fmt.Errorf("start HubSpot and PayPal mocks: %w", err)
	}
	var once sync.Once
	var cleanupErr error
	return func() error {
		once.Do(func() { cleanupErr = c.cleanup() })
		return cleanupErr
	}, nil
}

func (c *checkoutSync) connectAndCreateTable(ctx context.Context) error {
	database, err := sql.Open("mysql", c.databaseDSN())
	if err != nil {
		return fmt.Errorf("open MySQL connection: %w", err)
	}
	c.database = database
	for {
		err = database.PingContext(ctx)
		if err == nil {
			// The image restarts its server after initialization; make sure the final one answers queries.
			_, err = database.ExecContext(ctx, "SELECT 1")
		}
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for MySQL: %w", errors.Join(err, ctx.Err()))
		case <-time.After(500 * time.Millisecond):
		}
	}
	_, err = database.ExecContext(ctx, createTableStatement)
	if err != nil {
		return fmt.Errorf("create payments table: %w", err)
	}
	return nil
}

const createTableStatement = `CREATE TABLE payments (
    order_id VARCHAR(64) PRIMARY KEY,
    contact_id VARCHAR(64),
    email VARCHAR(255),
    amount DECIMAL(12,2),
    currency VARCHAR(3),
    status VARCHAR(16)
)`

func (c *checkoutSync) cleanup() error {
	var failures []error
	if c.mocks != nil {
		failures = append(failures, c.mocks.stop())
	}
	if c.database != nil {
		if err := c.database.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close MySQL connection: %w", err))
		}
	}
	if c.containerName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, err := exec.CommandContext(ctx, "docker", "rm", "--force", c.containerName).CombinedOutput()
		cancel()
		if err != nil && !strings.Contains(string(output), "No such container") {
			failures = append(failures, fmt.Errorf("remove MySQL container: %w: %s", err, strings.TrimSpace(string(output))))
		}
	}
	c.mocks, c.database, c.containerName = nil, nil, ""
	return errors.Join(failures...)
}

func (c *checkoutSync) Prompt() (string, error) {
	if c.initErr != nil {
		return "", c.initErr
	}
	parsed, err := template.New("checkout-sync").Parse(promptTemplate)
	if err != nil {
		return "", fmt.Errorf("parse prompt template: %w", err)
	}
	data := struct {
		HubSpotURL    string
		PayPalURL     string
		DatabaseHost  string
		DatabasePort  int
		DatabaseName  string
		DatabaseUser  string
		DatabasePass  string
		DatabaseTable string
	}{
		HubSpotURL:    origin(c.hubspotPort),
		PayPalURL:     origin(c.paypalPort),
		DatabaseHost:  "127.0.0.1",
		DatabasePort:  c.databasePort,
		DatabaseName:  databaseName,
		DatabaseUser:  databaseUser,
		DatabasePass:  databasePass,
		DatabaseTable: paymentsTable,
	}
	var prompt bytes.Buffer
	if err := parsed.Execute(&prompt, data); err != nil {
		return "", fmt.Errorf("render prompt template: %w", err)
	}
	return prompt.String(), nil
}

func (c *checkoutSync) ready() error {
	if c.initErr != nil {
		return c.initErr
	}
	if c.database == nil || c.mocks == nil {
		return errors.New("checkout sync backends are not running")
	}
	return nil
}

func (c *checkoutSync) databaseDSN() string {
	return fmt.Sprintf("%s:%s@tcp(127.0.0.1:%d)/%s?parseTime=false", databaseUser, databasePass, c.databasePort, databaseName)
}

func origin(port int) string {
	return (&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}).String()
}

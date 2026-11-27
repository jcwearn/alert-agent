package store

import (
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres/v2"
)

// testDatabaseURL is the Postgres every test in this package uses. Each test
// still gets its own schema (see openTestStore).
var testDatabaseURL string

// TestMain starts a throwaway Postgres 16 -- the major the cluster's CNPG
// image runs -- unless ALERT_AGENT_TEST_DATABASE_URL points at one already.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if url := os.Getenv("ALERT_AGENT_TEST_DATABASE_URL"); url != "" {
		testDatabaseURL = url
		return m.Run()
	}

	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "free port:", err)
		return 1
	}
	dir, err := os.MkdirTemp("", "alert-agent-pg-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		RuntimePath(dir + "/runtime").
		DataPath(dir + "/data").
		StartTimeout(60 * time.Second).
		Logger(io.Discard))
	if err := pg.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start embedded postgres:", err)
		return 1
	}
	defer func() { _ = pg.Stop() }()

	testDatabaseURL = fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	return m.Run()
}

func freePort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}

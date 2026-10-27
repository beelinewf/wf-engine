package database_test

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/beelinewf/wf-engine/internal/workflow/testdb"
	"github.com/beelinewf/wf-engine/pkg/database"
)

func TestNewRejectsEmptyDSN(t *testing.T) {
	_, err := database.New(database.Options{})
	if err == nil {
		t.Fatal("New() error = nil, want error for empty DSN")
	}
	if !strings.Contains(err.Error(), "dsn") {
		t.Errorf("error = %q, want mention of dsn", err)
	}
}

func TestNewRejectsUnreachableDatabase(t *testing.T) {
	_, err := database.New(database.Options{DSN: "host=127.0.0.1 user=x password=x dbname=x port=1 sslmode=disable"})
	if err == nil {
		t.Fatal("New() error = nil, want error for unreachable database")
	}
}

func TestDefaultOptionsConnectTimeout(t *testing.T) {
	if got := database.DefaultOptions().ConnectTimeout; got != 15*time.Second {
		t.Errorf("DefaultOptions().ConnectTimeout = %v, want 15s", got)
	}
}

// TestNewRespectsConnectTimeout pins the boot hang fix: a server that
// accepts TCP but never answers the Postgres handshake must fail fast
// instead of hanging the boot.
func TestNewRespectsConnectTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = ln.Close() }()
	// Never Accept: the kernel completes the TCP handshake into the
	// backlog, so the dial succeeds but the Postgres startup exchange
	// never completes.
	dsn := "host=127.0.0.1 user=x password=x dbname=x port=" +
		strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:") + " sslmode=disable"

	opts := database.DefaultOptions()
	opts.DSN = dsn
	opts.ConnectTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err = database.New(opts)
	if err == nil {
		t.Fatal("New() error = nil, want timeout against a silent server")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("New() took %v, want failure within the connect timeout", elapsed)
	}
}

func TestNewConnectsAndAppliesPoolOptions(t *testing.T) {
	dsn := testdb.RequireDSN(t, "database")

	opts := database.DefaultOptions()
	opts.DSN = dsn
	opts.MaxOpenConns = 7
	opts.MaxIdleConns = 4
	opts.ConnMaxLifetime = 90 * time.Second
	opts.ConnMaxIdleTime = 30 * time.Second

	db, err := database.New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if got := sqlDB.Stats().MaxOpenConnections; got != opts.MaxOpenConns {
		t.Errorf("MaxOpenConnections = %d, want %d", got, opts.MaxOpenConns)
	}
}

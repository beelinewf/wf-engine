package objectstore_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/beelinewf/wf-engine/internal/workflow/objectstore"
)

// The constructor needs a complete connection; minio.New dials nothing, so
// these cases run without a server.
func TestNewMinioStoreRequiresConnection(t *testing.T) {
	for name, args := range map[string][5]any{
		"empty endpoint": {"", "us-east-1", "ak", "sk", false},
		"empty access":   {"rustfs:9000", "us-east-1", "", "sk", false},
		"empty secret":   {"rustfs:9000", "us-east-1", "ak", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := objectstore.NewMinioStore(
				args[0].(string), args[1].(string), args[2].(string), args[3].(string), args[4].(bool),
			)
			if err == nil {
				t.Fatal("NewMinioStore() error = nil, want the incomplete connection rejected")
			}
		})
	}
}

// An http(s):// prefix is tolerated for operator convenience, and an empty
// region falls back to us-east-1.
func TestNewMinioStoreBuildsClient(t *testing.T) {
	for _, endpoint := range []string{"rustfs:9000", "http://rustfs:9000", "https://rustfs:9000"} {
		st, err := objectstore.NewMinioStore(endpoint, "", "ak", "sk", false)
		if err != nil {
			t.Errorf("NewMinioStore(%q) error = %v", endpoint, err)
			continue
		}
		if st == nil {
			t.Errorf("NewMinioStore(%q) = nil, want a store", endpoint)
		}
	}
}

// Get opens a lazy reader: against a dead endpoint the open succeeds and
// the read fails, so callers must cap and handle read errors.
func TestMinioStoreGetReadsThroughClient(t *testing.T) {
	st, err := objectstore.NewMinioStore("127.0.0.1:1", "", "ak", "sk", false)
	if err != nil {
		t.Fatalf("NewMinioStore() error = %v", err)
	}
	rc, err := st.Get(context.Background(), "bucket", "key")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("read error = nil, want the dead endpoint to fail")
	}
}

func TestMinioStoreGetRejectsBadBucket(t *testing.T) {
	st, err := objectstore.NewMinioStore("127.0.0.1:1", "", "ak", "sk", false)
	if err != nil {
		t.Fatalf("NewMinioStore() error = %v", err)
	}
	if _, err := st.Get(context.Background(), "", "key"); err == nil || !strings.Contains(err.Error(), "objectstore: get") {
		t.Errorf("Get() error = %v, want wrapped get failure", err)
	}
}

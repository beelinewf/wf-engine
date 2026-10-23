package objectstore_test

import (
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

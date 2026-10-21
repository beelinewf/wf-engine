package envsnapshot

import "testing"

func TestSnapshotFiltersPrefix(t *testing.T) {
	t.Setenv("BEELINEWF_S3_ENDPOINT", "play.min.io:9000")
	t.Setenv("NOT_MINE", "x")
	got := Snapshot("BEELINEWF_")
	if got["BEELINEWF_S3_ENDPOINT"] != "play.min.io:9000" {
		t.Fatal("want endpoint")
	}
	if _, ok := got["NOT_MINE"]; ok {
		t.Fatal("want prefix filter")
	}
}

func TestSnapshotExcludesDenied(t *testing.T) {
	t.Setenv("BEELINEWF_INFRA_POSTGRESQL_DSN", "postgres://x")
	t.Setenv("BEELINEWF_API_TOKEN", "tok")
	t.Setenv("BEELINEWF_AUTH_ENABLED", "true")
	t.Setenv("BEELINEWF_SYSTEM_USER_ID", "u1")
	t.Setenv("BEELINEWF_S3_ENDPOINT", "play.min.io:9000")
	got := Snapshot(Prefix)
	for _, k := range []string{
		"BEELINEWF_INFRA_POSTGRESQL_DSN",
		"BEELINEWF_API_TOKEN",
		"BEELINEWF_AUTH_ENABLED",
		"BEELINEWF_SYSTEM_USER_ID",
	} {
		if _, ok := got[k]; ok {
			t.Fatalf("denied key %s snapshotted", k)
		}
	}
	if got["BEELINEWF_S3_ENDPOINT"] != "play.min.io:9000" {
		t.Fatal("allowed key missing")
	}
}

func TestMergeCannotSmuggleDenied(t *testing.T) {
	base := map[string]any{"BEELINEWF_API_TOKEN": "evil", "BEELINEWF_S3_ENDPOINT": "ok"}
	got := Merge(StripDenied(base), Snapshot("TEST_PREFIX_THAT_MATCHES_NOTHING_"))
	if _, ok := got["BEELINEWF_API_TOKEN"]; ok {
		t.Fatal("denied base key survived")
	}
	if got["BEELINEWF_S3_ENDPOINT"] != "ok" {
		t.Fatal("allowed base key dropped")
	}
}

func TestStripDeniedNonMap(t *testing.T) {
	if got := StripDenied("nope"); len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestIsDeniedOverride(t *testing.T) {
	SetOverride([]string{"BEELINEWF_OPENROUTER*"}, []string{"BEELINEWF_OPENROUTER_MODEL"})
	defer SetOverride(nil, nil)

	if !IsDenied("BEELINEWF_OPENROUTER_KEY") {
		t.Fatal("extra pattern must deny")
	}
	if IsDenied("BEELINEWF_OPENROUTER_MODEL") {
		t.Fatal("allow exception must win")
	}
	if IsDenied("BEELINEWF_S3_ENDPOINT") {
		t.Fatal("unrelated key must stay allowed")
	}
}

func TestMergeSnapshotWins(t *testing.T) {
	base := map[string]any{"A": "old", "B": "keep"}
	got := Merge(base, map[string]any{"A": "new"})
	if got["A"] != "new" || got["B"] != "keep" {
		t.Fatalf("want overlay, got %v", got)
	}
}

func TestMergeNilBase(t *testing.T) {
	got := Merge(nil, map[string]any{"A": "1"})
	if got["A"] != "1" {
		t.Fatalf("want snapshot on nil base, got %v", got)
	}
}

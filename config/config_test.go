package config

import (
	"path/filepath"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	cases := map[string]int64{
		"10GB":  10 << 30,
		"500MB": 500 << 20,
		"1KB":   1 << 10,
		"2TB":   2 << 40,
		"1024":  1024,
		"5B":    5,
	}
	for in, want := range cases {
		got, err := parseByteSize(in)
		if err != nil {
			t.Errorf("parseByteSize(%q): unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseByteSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseByteSizeInvalid(t *testing.T) {
	if _, err := parseByteSize("not-a-size"); err == nil {
		t.Error("expected an error for an invalid byte size")
	}
}

func TestLoadCreatesDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg, created, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a missing config file")
	}
	if cfg.Recording.EventIdleTimeoutSeconds != 10 {
		t.Errorf("default idle timeout = %d, want 10", cfg.Recording.EventIdleTimeoutSeconds)
	}
	if cfg.Recording.MaxStorageBytes.Int64() != 10<<30 {
		t.Errorf("default max storage = %d, want %d", cfg.Recording.MaxStorageBytes.Int64(), int64(10<<30))
	}

	// A second Load should read the file back rather than recreate it.
	_, created2, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if created2 {
		t.Error("expected created=false once the config file exists")
	}
}

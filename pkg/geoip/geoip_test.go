package geoip

import (
	"path/filepath"
	"testing"
)

// These tests assert the degraded mode only (no mmdb files in the local
// checkout). Real database lookups are exercised by the CI build, which
// downloads the DB-IP Lite databases before building the image.
func TestLookupWithoutReaders(t *testing.T) {
	if _, ok := Lookup("8.8.8.8"); ok {
		t.Fatal("expected ok=false when readers are uninitialized")
	}
}

func TestLookupBadIP(t *testing.T) {
	// Readers nil plus unparsable IP must be a no-op, never a panic.
	readers.Store(nil)
	if _, ok := Lookup("not-an-ip"); ok {
		t.Fatal("expected ok=false for unparsable IP")
	}
}

func TestInitMissingFilesDegrades(t *testing.T) {
	// Missing paths must not fail startup; lookups stay degraded.
	missing := filepath.Join(t.TempDir(), "does-not-exist.mmdb")
	if err := Init(missing, missing); err != nil {
		t.Fatalf("Init with missing files must not error, got %v", err)
	}
	if _, ok := Lookup("8.8.8.8"); ok {
		t.Fatal("expected ok=false after degraded Init")
	}
}

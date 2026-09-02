package geoip

import (
	"os"
	"path/filepath"
	"testing"
)

// Integration test against real databases. Skipped unless GEOIP_TEST_DB_DIR
// points at a directory containing dbip-city-lite.mmdb and
// dbip-asn-lite.mmdb (ip2region_v4.xdb optional). Run locally once with real
// data to validate the lookup chain; CI and the image build exercise the
// databases separately.
func TestLookupWithRealDatabases(t *testing.T) {
	dir := os.Getenv("GEOIP_TEST_DB_DIR")
	if dir == "" {
		t.Skip("GEOIP_TEST_DB_DIR not set; skipping real-database lookup")
	}
	if err := Init(
		filepath.Join(dir, "dbip-city-lite.mmdb"),
		filepath.Join(dir, "dbip-asn-lite.mmdb"),
		filepath.Join(dir, "ip2region_v4.xdb"),
	); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 8.8.8.8 is Google DNS (US, AS15169). These assertions are stable
	// properties of the public databases.
	info, ok := Lookup("8.8.8.8")
	if !ok {
		t.Fatal("expected lookup to succeed for 8.8.8.8")
	}
	if info.CountryCode != "US" {
		t.Errorf("expected country US, got %q", info.CountryCode)
	}
	if info.ASN != 15169 {
		t.Errorf("expected ASN 15169, got %d", info.ASN)
	}
}

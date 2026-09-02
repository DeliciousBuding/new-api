package geoip

import (
	"os"
	"path/filepath"
	"testing"
)

// Known-answer gate for the geo data supply chain. Skipped unless
// GEOIP_TEST_DB_DIR points at a directory containing dbip-city-lite.mmdb,
// dbip-asn-lite.mmdb and ip2region_v4.xdb. scripts/update-geoip-data.py runs
// this gate before promoting any data refresh, and CI runs it on image
// builds, so a data regression can never ship silently.
func TestKnownAnswers(t *testing.T) {
	dir := os.Getenv("GEOIP_TEST_DB_DIR")
	if dir == "" {
		t.Skip("GEOIP_TEST_DB_DIR not set; skipping known-answer gate")
	}
	if err := Init(
		filepath.Join(dir, "dbip-city-lite.mmdb"),
		filepath.Join(dir, "dbip-asn-lite.mmdb"),
		filepath.Join(dir, "ip2region_v4.xdb"),
	); err != nil {
		t.Fatalf("Init: %v", err)
	}

	for _, ka := range []struct {
		ip          string
		countryCode string
		province    string
		city        string
		asn         int
	}{
		// CN mobile ranges that DB-IP Lite alone misattributes (the
		// 223.104.132.0/24 maps to Qingyuan in 2026-07); the ip2region
		// overlay must win for these.
		{"223.104.132.182", "CN", "湖南", "长沙", 56047},
		{"113.92.157.29", "CN", "广东", "深圳", 0},
		// Stable global anchors.
		{"8.8.8.8", "US", "", "", 15169},
		{"1.2.3.4", "AU", "", "Brisbane", 0},
	} {
		info, ok := Lookup(ka.ip)
		if !ok {
			t.Fatalf("%s: lookup failed", ka.ip)
		}
		if info.CountryCode != ka.countryCode {
			t.Errorf("%s: country code = %q, want %q", ka.ip, info.CountryCode, ka.countryCode)
		}
		if ka.province != "" && info.Province != ka.province {
			t.Errorf("%s: province = %q, want %q", ka.ip, info.Province, ka.province)
		}
		if ka.city != "" && info.City != ka.city {
			t.Errorf("%s: city = %q, want %q", ka.ip, info.City, ka.city)
		}
		if ka.asn != 0 && info.ASN != ka.asn {
			t.Errorf("%s: asn = %d, want %d", ka.ip, info.ASN, ka.asn)
		}
	}
}

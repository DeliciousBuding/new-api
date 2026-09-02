package geoip

import (
	"path/filepath"
	"testing"
)

// These tests assert the degraded mode and the pure parsing helpers only
// (no database files in the local checkout). Real database lookups are
// exercised by the known-answer gate (known_answers_test.go) and the
// integration test, which run when GEOIP_TEST_DB_DIR points at real files.
func TestLookupWithoutReaders(t *testing.T) {
	readers.Store(nil)
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
	if err := Init(missing, missing, missing); err != nil {
		t.Fatalf("Init with missing files must not error, got %v", err)
	}
	if _, ok := Lookup("8.8.8.8"); ok {
		t.Fatal("expected ok=false after degraded Init")
	}
}

func TestApplyRegion(t *testing.T) {
	cases := []struct {
		in   string
		want GeoInfo
	}{
		{
			"中国|湖南省|长沙市|移动|CN",
			GeoInfo{Country: "中国", CountryCode: "CN", Province: "湖南", City: "长沙", ISP: "移动"},
		},
		{
			"中国|内蒙古自治区|呼和浩特市|联通|CN",
			GeoInfo{Country: "中国", CountryCode: "CN", Province: "内蒙古", City: "呼和浩特", ISP: "联通"},
		},
		{
			// "0" marks an empty field; non-CN names stay verbatim.
			"United States|California|0|Google LLC|US",
			GeoInfo{Country: "United States", CountryCode: "US", Province: "California", ISP: "Google LLC"},
		},
		{"garbage", GeoInfo{}},
		{"a|b", GeoInfo{}},
	}
	for _, c := range cases {
		var got GeoInfo
		applyRegion(&got, c.in)
		if got != c.want {
			t.Errorf("applyRegion(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestTrimCNSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"湖南省": "湖南", "长沙市": "长沙", "内蒙古自治区": "内蒙古",
		"广西壮族自治区": "广西", "香港特别行政区": "香港", "北京市": "北京",
		"California": "California", "": "",
	} {
		if got := trimCNSuffix(in); got != want {
			t.Errorf("trimCNSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

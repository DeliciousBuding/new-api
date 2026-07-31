package geoip

import (
	"log"
	"net"
	"strings"
	"sync/atomic"

	"github.com/oschwald/geoip2-golang"
)

// GeoIP lookup for log locality hints (admin-only display).
//
// The mmdb files are optional at runtime: when they are absent (local dev,
// images built without them) lookups degrade to ok=false and logs simply
// carry no geo fields. Data source: DB-IP Lite (CC-BY-4.0), loaded from
// GEOIP_DB_DIR (default /data/geoip).
//
// This is a display hint only — never used for auth, billing, or routing.

// GeoInfo is the locality hint attached to a log entry.
type GeoInfo struct {
	Country     string
	CountryCode string
	City        string
	ASN         int
	ASNOrg      string
}

var readers atomic.Pointer[geoipReaders]

type geoipReaders struct {
	city *geoip2.Reader
	asn  *geoip2.Reader
}

// Init loads the optional mmdb files. Missing or invalid files are skipped
// with a warning (degraded mode); an error is only returned when no lookup
// capability remains at all. Idempotent: calling again replaces the readers.
func Init(cityPath, asnPath string) error {
	city, err := geoip2.Open(cityPath)
	if err != nil {
		log.Printf("geoip: skip city db (%v)", err)
		city = nil
	}
	asn, err := geoip2.Open(asnPath)
	if err != nil {
		log.Printf("geoip: skip asn db (%v)", err)
		asn = nil
	}
	readers.Store(&geoipReaders{city: city, asn: asn})
	if city == nil && asn == nil {
		return nil // degraded mode: logs carry no geo fields
	}
	return nil
}

// Lookup resolves an IP to a locality hint. Returns ok=false when the
// readers are unavailable or the IP cannot be parsed.
func Lookup(ipStr string) (GeoInfo, bool) {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return GeoInfo{}, false
	}
	rs := readers.Load()
	if rs == nil {
		return GeoInfo{}, false
	}
	var info GeoInfo
	if rs.city != nil {
		if rec, err := rs.city.City(ip); err == nil {
			info.Country = rec.Country.Names["en"]
			info.CountryCode = rec.Country.IsoCode
			info.City = rec.City.Names["en"]
		}
	}
	if rs.asn != nil {
		if rec, err := rs.asn.ASN(ip); err == nil {
			info.ASN = int(rec.AutonomousSystemNumber)
			info.ASNOrg = rec.AutonomousSystemOrganization
		}
	}
	if info.Country == "" && info.ASN == 0 {
		return GeoInfo{}, false
	}
	return info, true
}

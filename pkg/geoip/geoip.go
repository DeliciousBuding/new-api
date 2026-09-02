package geoip

import (
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
	"github.com/oschwald/geoip2-golang"
)

// GeoIP lookup for log locality hints (admin-only display).
//
// Data sources (all optional at runtime; a missing file degrades only its
// own layer, see docs/dev/geoip.md for the supply chain):
//
//   - ip2region xdb (Apache-2.0 OR MIT): primary city source. City-level
//     accuracy is strong for CN ranges where DB-IP Lite is coarse; also
//     carries province and ISP. IPv4 only.
//   - DB-IP Lite city (CC-BY-4.0): fallback/complement city source, global
//     coverage including IPv6.
//   - DB-IP Lite ASN (CC-BY-4.0): autonomous-system number and org.
//
// Merge rule is field-wise: ip2region first, DB-IP fills the gaps.
//
// This is a display hint only — never used for auth, billing, or routing.

// GeoInfo is the locality hint attached to a log entry.
type GeoInfo struct {
	Country     string
	CountryCode string
	Province    string
	City        string
	ISP         string
	ASN         int
	ASNOrg      string
}

type geoipReaders struct {
	city   *geoip2.Reader
	asn    *geoip2.Reader
	region *xdb.Searcher // full-buffer v4 searcher; concurrency-safe
}

func (r *geoipReaders) close() {
	if r == nil {
		return
	}
	if r.city != nil {
		r.city.Close()
	}
	if r.asn != nil {
		r.asn.Close()
	}
	if r.region != nil {
		r.region.Close()
	}
}

type geoPaths struct {
	city, asn, region string
	mods              [3]time.Time
}

var (
	readers atomic.Pointer[geoipReaders]
	paths   atomic.Pointer[geoPaths]
)

// retireGrace delays closing swapped-out readers so in-flight lookups that
// already loaded the old pointer finish safely (maxminddb returns errors,
// never panics, after Close, but the grace keeps even those out of the log).
const retireGrace = 30 * time.Second

// Init loads the optional database files. Missing or invalid files are
// skipped with a warning (degraded mode); an error is never returned for a
// partial set so startup always succeeds. Idempotent: calling again replaces
// the readers atomically and retires the old ones after a grace period.
func Init(cityPath, asnPath, regionPath string) error {
	rs := &geoipReaders{}
	var mods [3]time.Time
	for i, p := range []string{cityPath, asnPath, regionPath} {
		if st, err := os.Stat(p); err == nil {
			mods[i] = st.ModTime()
		}
	}
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
	if buff, err := xdb.LoadContentFromFile(regionPath); err != nil {
		log.Printf("geoip: skip ip2region xdb (%v)", err)
	} else if searcher, err := xdb.NewWithBuffer(xdb.IPv4, buff); err != nil {
		log.Printf("geoip: skip ip2region xdb (%v)", err)
	} else {
		rs.region = searcher
	}
	rs.city = city
	rs.asn = asn

	old := readers.Swap(rs)
	time.AfterFunc(retireGrace, old.close)
	paths.Store(&geoPaths{city: cityPath, asn: asnPath, region: regionPath, mods: mods})
	if city == nil && asn == nil && rs.region == nil {
		return nil // fully degraded mode: logs carry no geo fields
	}
	return nil
}

// AutoReload polls the database files every interval and hot-swaps the
// readers when any file appears, changes, or disappears, so data refreshes
// need neither an image rebuild nor a container restart. interval <= 0
// disables polling.
func AutoReload(interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			p := paths.Load()
			if p == nil {
				continue
			}
			changed := false
			for i, f := range []string{p.city, p.asn, p.region} {
				st, err := os.Stat(f)
				if err != nil {
					if !p.mods[i].IsZero() {
						changed = true // file disappeared
					}
					continue
				}
				if !st.ModTime().Equal(p.mods[i]) {
					changed = true
				}
			}
			if changed {
				log.Printf("geoip: data change detected, reloading")
				if err := Init(p.city, p.asn, p.region); err != nil {
					log.Printf("geoip: reload failed: %v", err)
				}
			}
		}
	}()
}

// Lookup resolves an IP to a locality hint. Returns ok=false when the
// readers are unavailable or the IP cannot be parsed.
func Lookup(ipStr string) (GeoInfo, bool) {
	ipStr = strings.TrimSpace(ipStr)
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return GeoInfo{}, false
	}
	rs := readers.Load()
	if rs == nil {
		return GeoInfo{}, false
	}
	var info GeoInfo
	// Layer 1: ip2region (IPv4 only) is the primary city/province/ISP source.
	if rs.region != nil && ip.To4() != nil {
		if region, err := rs.region.Search(ipStr); err == nil {
			applyRegion(&info, region)
		}
	}
	// Layer 2: DB-IP city fills gaps (IPv6, xdb misses). City is only taken
	// when ip2region produced no locality at all, avoiding mixed-language
	// province/city pairs for CN addresses.
	if rs.city != nil {
		if rec, err := rs.city.City(ip); err == nil {
			if info.Country == "" {
				info.Country = rec.Country.Names["en"]
			}
			if info.CountryCode == "" {
				info.CountryCode = rec.Country.IsoCode
			}
			if info.City == "" && info.Province == "" {
				info.City = rec.City.Names["en"]
			}
		}
	}
	// Layer 3: DB-IP ASN is the only AS-number source.
	if rs.asn != nil {
		if rec, err := rs.asn.ASN(ip); err == nil {
			info.ASN = int(rec.AutonomousSystemNumber)
			info.ASNOrg = rec.AutonomousSystemOrganization
		}
	}
	if info.Country == "" && info.City == "" && info.ASN == 0 {
		return GeoInfo{}, false
	}
	return info, true
}

// applyRegion parses the ip2region record format
// "Country|Province|City|ISP|iso-alpha2-code" where "0" marks an empty
// field. CN subdivisions are normalized to short display names.
func applyRegion(info *GeoInfo, region string) {
	fields := strings.Split(region, "|")
	if len(fields) < 5 {
		return
	}
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "0" {
			return ""
		}
		return s
	}
	province := clean(fields[1])
	city := clean(fields[2])
	if strings.ToUpper(clean(fields[4])) == "CN" {
		province = trimCNSuffix(province)
		city = trimCNSuffix(city)
	}
	info.Country = clean(fields[0])
	info.CountryCode = strings.ToUpper(clean(fields[4]))
	info.Province = province
	info.City = city
	info.ISP = clean(fields[3])
}

// trimCNSuffix reduces CN subdivision names to their short form
// (湖南省 -> 湖南, 长沙市 -> 长沙, 内蒙古自治区 -> 内蒙古).
func trimCNSuffix(s string) string {
	for _, suffix := range []string{
		"维吾尔自治区", "壮族自治区", "回族自治区", "特别行政区", "自治区", "省", "市",
	} {
		if strings.HasSuffix(s, suffix) && len(s) > len(suffix) {
			return strings.TrimSuffix(s, suffix)
		}
	}
	return s
}

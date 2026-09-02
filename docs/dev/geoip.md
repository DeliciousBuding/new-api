# GeoIP locality hints

最后更新：2026-09-02

Log IP locality is an admin-only display hint — never used for auth, billing,
or routing. Resolution is offline at read time; the log table stores raw IPs
only and nothing here is cached between requests.

## Data sources

| File | Source | Role | License |
|---|---|---|---|
| `ip2region_v4.xdb` | [ip2region](https://github.com/lionsoul2014/ip2region) community xdb | primary city/province/ISP (IPv4; strong for CN) | Apache-2.0 OR MIT |
| `dbip-city-lite.mmdb` | [DB-IP Lite](https://db-ip.com/db/lite.php) monthly | fallback city (global, IPv6) | CC-BY-4.0 |
| `dbip-asn-lite.mmdb` | DB-IP Lite monthly | ASN number/org | CC-BY-4.0 |

Merge rule: field-wise, ip2region first, DB-IP fills the gaps. DB-IP Lite
alone maps many CN mobile /24s to a single wrong city (e.g. 223.104.132.0/24
→ Qingyuan in the 2026-07 snapshot while the addresses are Changsha); the
overlay fixes CN city accuracy. Attribution lives in NOTICE.

## Runtime layout

- Files live in `GEOIP_DB_DIR` (default `/opt/geoip`), baked into the image
  at pinned versions (Dockerfile `GEOIP_DB_MONTH`, `IP2REGION_XDB_REV`).
- Missing files degrade per-source; startup never fails.
- `GEOIP_RELOAD_INTERVAL` (seconds, default 300, 0 disables): mtime polling
  hot-swaps the readers atomically, so a data refresh needs neither an image
  rebuild nor a container restart. Mount a volume over `GEOIP_DB_DIR` in
  production to use live updates.

## Updating data

1. Live refresh (no rebuild): `python scripts/update-geoip-data.py --target
   /opt/geoip`. It downloads the latest DB-IP month plus the ip2region xdb
   into a staging dir, runs the known-answer gate (`go test ./pkg/geoip -run
   'TestKnownAnswers|TestLookupWithRealDatabases'` with
   `GEOIP_TEST_DB_DIR`), and promotes atomically only on pass; the hot
   reload picks the new files up within one interval.
2. Image bump: extend `pkg/geoip/known_answers_test.go` when new regression
   cases are known, run the gate on candidate files, then bump
   `GEOIP_DB_MONTH` / `IP2REGION_XDB_REV` in the Dockerfile.

The known-answer gate is the contract: data that misattributes the pinned
regression IPs can never ship silently.

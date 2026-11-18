package activitylog

// ── Country resolution for the geo reporting breakdown ─────────────────────
//
// activity.log carries a per-auction ISO-3166 alpha-2 `country` so the
// dashboard can break ad requests / bid requests down by geo (see
// dashboard/CLAUDE.md's "Geo & prompt-category breakdown"). Only the
// resolved country code is ever written — never the IP it came from — so
// activity.log keeps its "no IPs" guarantee (see the package doc); the IP
// stays in debug.log only, as before.
//
// Resolution order:
//  1. device.ip / device.ipv6 looked up in a local MaxMind GeoLite2-Country
//     database (config `geo_db_path`, mounted and refreshed by
//     pbs-settings/deploy.sh). PBS has already filled device.ip from
//     X-Forwarded-For by the time ProcessedAuctionRequest runs.
//  2. device.geo.country, but only if it's already a 2-letter code — OpenRTB
//     specifies alpha-3 there and nothing in PBS maps alpha-3 → alpha-2, so
//     an alpha-3 value is dropped rather than guessed at.
//
// Missing/unreadable DB is never fatal: the module keeps logging exactly as
// before, just without `country` (the ingest side treats that as unknown).
// The DB file is re-checked by mtime at most once per geoReloadInterval so
// the monthly GeoLite2 refresh cron takes effect without a PBS restart.

import (
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/oschwald/maxminddb-golang"
	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"golang.org/x/text/language"
)

const geoReloadInterval = 10 * time.Minute

// countryLookup is the one method the module needs from a GeoIP database —
// an interface so tests don't need a real .mmdb file.
type countryLookup interface {
	Country(ip net.IP) string
}

type mmdbRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// geoDB wraps a maxminddb.Reader that is reopened when the file on disk
// changes. Safe for concurrent use. A nil *geoDB is valid and resolves
// nothing.
type geoDB struct {
	path string

	mu          sync.RWMutex
	reader      *maxminddb.Reader
	loadedMtime time.Time
	lastCheck   time.Time
}

func newGeoDB(path string) *geoDB {
	if path == "" {
		return nil
	}
	g := &geoDB{path: path}
	g.reloadIfChanged(time.Now())
	return g
}

func (g *geoDB) reloadIfChanged(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastCheck = now

	info, err := os.Stat(g.path)
	if err != nil {
		if g.reader == nil {
			glog.Warningf("activitylog: geo DB %q not available, country will be omitted: %v", g.path, err)
		}
		return
	}
	if g.reader != nil && info.ModTime().Equal(g.loadedMtime) {
		return
	}
	reader, err := maxminddb.Open(g.path)
	if err != nil {
		glog.Warningf("activitylog: failed to open geo DB %q: %v", g.path, err)
		return
	}
	// The old reader is intentionally never Close()d: on Linux it's an
	// mmap, and a concurrent Country() may still be mid-Lookup on it —
	// unmapping under it would segfault. Leaking one ~8MB mapping per
	// monthly refresh (reset on every PBS deploy anyway) is the cheap,
	// safe trade.
	g.reader = reader
	g.loadedMtime = info.ModTime()
}

func (g *geoDB) Country(ip net.IP) string {
	if g == nil || ip == nil {
		return ""
	}
	now := time.Now()
	g.mu.RLock()
	stale := now.Sub(g.lastCheck) > geoReloadInterval
	g.mu.RUnlock()
	if stale {
		g.reloadIfChanged(now)
	}

	g.mu.RLock()
	reader := g.reader
	g.mu.RUnlock()
	if reader == nil {
		return ""
	}
	var rec mmdbRecord
	if err := reader.Lookup(ip, &rec); err != nil {
		return ""
	}
	return rec.Country.ISOCode
}

// resolveCountry applies the resolution order in the file doc above.
func resolveCountry(lookup countryLookup, device *openrtb2.Device) string {
	if device == nil {
		return ""
	}
	if lookup != nil {
		for _, raw := range []string{device.IP, device.IPv6} {
			if raw == "" {
				continue
			}
			if c := lookup.Country(net.ParseIP(raw)); c != "" {
				return strings.ToUpper(c)
			}
		}
	}
	if device.Geo != nil && len(device.Geo.Country) == 2 {
		return strings.ToUpper(device.Geo.Country)
	}
	return ""
}

// ── Country on the bid request (HOL-144) ───────────────────────────────────
//
// The same lookup also fills device.geo.country on the request bidders
// receive, when the caller sent none. Callers rarely send it, and a bidder
// that buys by country (Kontext) reaches only part of its demand without it.
// Only the country is added, never a region, city or coordinate.
//
// The value is ISO-3166-1 alpha-3, as OpenRTB specifies. This is separate
// from the alpha-2 code written to activity.log, which does not change.

// bidderCountry returns the alpha-3 country to put on the bid request, or ""
// to leave the request alone: no device, a country the caller already sent
// (never overwritten), or an IP the database does not resolve.
func bidderCountry(lookup countryLookup, device *openrtb2.Device) string {
	if lookup == nil || device == nil {
		return ""
	}
	if device.Geo != nil && device.Geo.Country != "" {
		return ""
	}
	for _, raw := range []string{device.IP, device.IPv6} {
		if raw == "" {
			continue
		}
		if c := lookup.Country(net.ParseIP(raw)); c != "" {
			return alpha3Country(c)
		}
	}
	return ""
}

// alpha3Country converts an alpha-2 code to alpha-3, or "" when it is not a
// real country (MaxMind also returns codes such as "EU" and "AP").
func alpha3Country(code string) string {
	region, err := language.ParseRegion(code)
	if err != nil || !region.IsCountry() {
		return ""
	}
	return region.ISO3()
}

// setDeviceCountry is the ChangeSet mutation that writes the country. It
// copies device and geo instead of writing through the shared pointers, and
// re-checks that no country appeared in the meantime.
func setDeviceCountry(country string) hookstage.MutationFunc[hookstage.ProcessedAuctionRequestPayload] {
	return func(p hookstage.ProcessedAuctionRequestPayload) (hookstage.ProcessedAuctionRequestPayload, error) {
		if p.Request == nil || p.Request.BidRequest == nil || p.Request.Device == nil {
			return p, nil
		}
		device := *p.Request.Device
		var geo openrtb2.Geo
		if device.Geo != nil {
			geo = *device.Geo
		}
		if geo.Country != "" {
			return p, nil
		}
		geo.Country = country
		device.Geo = &geo
		p.Request.Device = &device
		return p, nil
	}
}

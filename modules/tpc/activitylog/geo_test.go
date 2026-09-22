package activitylog

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/hooks/hookstage"
	"github.com/prebid/prebid-server/v4/modules/moduledeps"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLookup maps IP strings to country codes, standing in for a real
// GeoLite2 .mmdb (MaxMind's test DBs aren't shipped in the Go module).
type fakeLookup map[string]string

func (f fakeLookup) Country(ip net.IP) string { return f[ip.String()] }

func TestResolveCountryPrefersIPLookup(t *testing.T) {
	lookup := fakeLookup{"203.0.113.5": "us"}
	device := &openrtb2.Device{IP: "203.0.113.5", Geo: &openrtb2.Geo{Country: "DE"}}
	assert.Equal(t, "US", resolveCountry(lookup, device))
}

func TestResolveCountryFallsBackToIPv6(t *testing.T) {
	lookup := fakeLookup{"2001:db8::1": "IN"}
	device := &openrtb2.Device{IP: "198.51.100.1", IPv6: "2001:db8::1"}
	assert.Equal(t, "IN", resolveCountry(lookup, device))
}

func TestResolveCountryFallsBackToAlpha2DeviceGeo(t *testing.T) {
	device := &openrtb2.Device{IP: "198.51.100.1", Geo: &openrtb2.Geo{Country: "gb"}}
	assert.Equal(t, "GB", resolveCountry(fakeLookup{}, device))
	assert.Equal(t, "GB", resolveCountry(nil, device))
}

func TestResolveCountryDropsAlpha3DeviceGeo(t *testing.T) {
	// OpenRTB's device.geo.country is alpha-3; we don't guess a mapping.
	device := &openrtb2.Device{Geo: &openrtb2.Geo{Country: "USA"}}
	assert.Equal(t, "", resolveCountry(nil, device))
}

func TestResolveCountryNilDevice(t *testing.T) {
	assert.Equal(t, "", resolveCountry(fakeLookup{}, nil))
}

func TestNilGeoDBResolvesNothing(t *testing.T) {
	var g *geoDB
	assert.Equal(t, "", g.Country(net.ParseIP("203.0.113.5")))
	assert.Nil(t, newGeoDB(""))
}

func TestMissingGeoDBFileIsNotFatal(t *testing.T) {
	g := newGeoDB(filepath.Join(t.TempDir(), "absent.mmdb"))
	require.NotNil(t, g)
	assert.Equal(t, "", g.Country(net.ParseIP("203.0.113.5")))
}

func TestBuilderWithMissingGeoDBStillLogs(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"enabled": true, "log_dir": "` + dir + `", "geo_db_path": "` + filepath.Join(dir, "absent.mmdb") + `"}`
	built, err := Builder([]byte(cfg), moduledeps.ModuleDeps{})
	require.NoError(t, err)
	module := built.(Module)

	result := processRequest(t, module, []openrtb2.Imp{impWithBidders("imp1", "sayhola-9243e9b6", "thrad")})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{})

	lines := readLines(t, dir, "activity")
	require.Len(t, lines, 1)
	_, hasCountry := lines[0]["country"]
	assert.False(t, hasCountry, "country must be omitted, not empty-string, when unresolved")
}

func TestActivityLineCarriesCountryButNeverIP(t *testing.T) {
	dir := t.TempDir()
	module := buildModule(t, dir, "")
	module.geo = fakeLookup{"203.0.113.5": "ID"}

	result := processRequest(t, module, []openrtb2.Imp{impWithBidders("imp1", "sayhola-9243e9b6", "thrad")})
	processResponse(t, module, result.ModuleContext, &openrtb2.BidResponse{SeatBid: []openrtb2.SeatBid{seatBid("thrad", "imp1")}})

	lines := readLines(t, dir, "activity")
	require.Len(t, lines, 1)
	assert.Equal(t, "ID", lines[0]["country"])

	matches, _ := filepath.Glob(filepath.Join(dir, "activity.log-*"))
	raw, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), "203.0.113.5"), "activity.log must never contain the device IP")
}

func TestTestFlaggedRequestStillSkippedWithGeo(t *testing.T) {
	dir := t.TempDir()
	module := buildModule(t, dir, "")
	module.geo = fakeLookup{"203.0.113.5": "US"}
	payload := hookstage.ProcessedAuctionRequestPayload{Request: &openrtb_ext.RequestWrapper{BidRequest: &openrtb2.BidRequest{
		Test: 1, Imp: []openrtb2.Imp{impWithBidders("imp1", "x", "thrad")}, Device: &openrtb2.Device{IP: "203.0.113.5"},
	}}}
	result, err := module.HandleProcessedAuctionHook(context.Background(), hookstage.ModuleInvocationContext{}, payload)
	require.NoError(t, err)
	assert.Nil(t, result.ModuleContext)
}

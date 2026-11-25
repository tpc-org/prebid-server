package activitylog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeShip(t *testing.T, dir string, c shipConfig) {
	t.Helper()
	b, err := json.Marshal(c)
	require.NoError(t, err)
	p := filepath.Join(dir, shipFile)
	require.NoError(t, os.WriteFile(p, b, 0644))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(p, future, future))
}

// collector is a TLS test server that records every batch it receives.
type collector struct {
	mu      sync.Mutex
	lines   []map[string]interface{}
	headers []http.Header
	status  int
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	c := &collector{status: http.StatusNoContent}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.headers = append(c.headers, r.Header.Clone())
		zr, err := gzip.NewReader(r.Body)
		if err == nil {
			sc := bufio.NewScanner(zr)
			for sc.Scan() {
				var m map[string]interface{}
				if json.Unmarshal(sc.Bytes(), &m) == nil {
					c.lines = append(c.lines, m)
				}
			}
		}
		w.WriteHeader(c.status)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

func TestShipperIsInertWithoutShipJSON(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), writeIVTDir(t, true))
	require.NotNil(t, m.ship)
	runAuction(t, m, `{}`, homeUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: homeUser, UA: chromeUA}})
	assert.Zero(t, m.ship.stats.queued.Load())
	assert.Zero(t, len(m.ship.ch))
}

func TestShipperIsNilWithoutIVTDir(t *testing.T) {
	m := buildIVTModule(t, t.TempDir(), "")
	assert.Nil(t, m.ship)
	assert.NotPanics(t, func() { m.ship.enqueue([]byte("{}"), nil) })
}

func TestShipperSendsActivityLinesGzipped(t *testing.T) {
	col, srv := newCollector(t)
	dir := writeIVTDir(t, true)
	writeShip(t, dir, shipConfig{Enabled: true, URL: srv.URL + "/v1/events", Token: "tok-use1", Region: "use1"})
	m := buildIVTModule(t, t.TempDir(), dir)
	m.ship.client = srv.Client()

	line := runAuction(t, m, `{}`, homeUser, &openrtb2.BidRequest{Device: &openrtb2.Device{IP: homeUser, UA: chromeUA}})
	require.NotNil(t, line["u"])
	assert.Eventually(t, func() bool { return col.count() == 1 }, 3*shipFlushInterval, 50*time.Millisecond)

	col.mu.Lock()
	defer col.mu.Unlock()
	got := col.lines[0]
	assert.Equal(t, line["u"], got["u"], "same line as activity.log")
	assert.Equal(t, line["imps"], got["imps"])
	h := col.headers[0]
	assert.Equal(t, "Bearer tok-use1", h.Get("Authorization"))
	assert.Equal(t, "use1", h.Get("X-TPC-Region"))
	assert.Equal(t, "gzip", h.Get("Content-Encoding"))
	assert.Equal(t, "1", h.Get("X-TPC-Lines"))
	assert.Equal(t, uint64(1), m.ship.stats.sent.Load())
}

func TestShipConfigNeedsHTTPSTokenAndRegion(t *testing.T) {
	assert.False(t, shipConfig{Enabled: true, URL: "http://x/v1/events", Token: "t", Region: "use1"}.on())
	assert.False(t, shipConfig{Enabled: true, URL: "https://x/v1/events", Region: "use1"}.on())
	assert.False(t, shipConfig{Enabled: true, URL: "https://x/v1/events", Token: "t"}.on())
	assert.False(t, shipConfig{URL: "https://x/v1/events", Token: "t", Region: "use1"}.on())
	assert.True(t, shipConfig{Enabled: true, URL: "https://x/v1/events", Token: "t", Region: "use1"}.on())
}

func TestEnqueueNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	l := &ivtLists{ship: shipConfig{Enabled: true, URL: "https://x/v1/events", Token: "t", Region: "use1"}}
	s := &shipper{ch: make(chan []byte, 2)} // no worker draining it
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			s.enqueue([]byte(`{"u":{}}`), l)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}
	assert.Equal(t, uint64(2), s.stats.queued.Load())
	assert.Equal(t, uint64(8), s.stats.dropped.Load())
}

func TestFailedBatchIsCountedAndDropped(t *testing.T) {
	dir := writeIVTDir(t, true)
	writeShip(t, dir, shipConfig{Enabled: true, URL: "https://collector.invalid/v1/events", Token: "t", Region: "euc1"})
	store := newIVTStore(dir)
	calls := 0
	s := &shipper{store: store, maxBatch: shipMaxBatchBytes, post: func(context.Context, shipConfig, []byte, int) error {
		calls++
		return errors.New("connection refused")
	}}
	var buf bytes.Buffer
	buf.WriteString("{\"u\":{}}\n{\"u\":{}}\n")
	s.flush(&buf, 2)
	assert.Equal(t, 2, calls, "one retry")
	assert.Equal(t, uint64(2), s.stats.failed.Load())
	assert.Zero(t, buf.Len(), "a failed batch is not kept in memory")
}

func TestStateFilesReloadWithoutReparsingLists(t *testing.T) {
	d := writeIVTDir(t, true)
	s := newIVTStore(d)
	before := s.get(time.Now())
	require.False(t, before.ship.on())

	writeShip(t, d, shipConfig{Enabled: true, URL: "https://x/v1/events", Token: "t", Region: "use1"})
	require.NoError(t, os.WriteFile(filepath.Join(d, "confirmed.txt"), []byte("2026w46:abcd\n"), 0644))
	future := time.Now().Add(3 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(d, "confirmed.txt"), future, future))
	s.get(time.Now().Add(ivtReloadInterval + time.Second))
	assert.Eventually(t, func() bool { return s.get(time.Now()).ship.on() }, 2*time.Second, 20*time.Millisecond)

	after := s.get(time.Now())
	_, ok := after.confirmed["2026w46:abcd"]
	assert.True(t, ok)
	assert.Equal(t, &before.dc[0], &after.dc[0], "IP lists shared, not re-parsed")
	assert.Equal(t, "dc", after.ipClass(netip.MustParseAddr(dcUser)))
	assert.Equal(t, before.keyID, after.keyID)

	// A state file that disappears is cleared, not kept from the old copy.
	require.NoError(t, os.Remove(filepath.Join(d, "confirmed.txt")))
	s.get(time.Now().Add(2*ivtReloadInterval + 2*time.Second))
	assert.Eventually(t, func() bool { return len(s.get(time.Now()).confirmed) == 0 }, 2*time.Second, 20*time.Millisecond)
}

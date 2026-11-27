package activitylog

// ── Asynchronous IVT event shipping (off-host IVT pipeline) ─────────────────
//
// IVT analysis used to run as hourly cron jobs on the PBS hosts themselves,
// re-reading two days of activity.log in Python. That competes with PBS for
// the same 2 vCPUs / 2 GB, and at Virginia's ~2M auctions/day it no longer
// fits. The shipper moves the data off the host instead: every activity
// line that carries a `u` object is also queued here and POSTed, batched and
// gzipped, to the IVT pipeline instance (pbs-settings ivt-pipeline/), which
// runs the analysis and publishes the blocking state back into ivt_dir.
//
// Same idea as upstream's analytics/agma buffer (flush on size or interval),
// with one difference that matters on the auction path: enqueue NEVER
// blocks. The queue is bounded; when it is full (collector down or slow) the
// event is dropped and counted. Auctions never wait on the network, and
// memory is capped at queue_size lines (~300 B each) plus one batch.
//
// Configuration is a file, not pbs.yaml: ivt_dir/ship.json, written by the
// host agent (pbs-settings ivt-pipeline/agent) and reloaded with the other
// small state files. pbs.yaml is shared by both regions, the host knows its
// own region, and this way shipping can be switched on or off per host
// without a PBS restart or a settings release. A missing, unreadable or
// disabled ship.json means nothing is queued — the module behaves exactly as
// before. The activity.log file is still written either way; the reporting
// pipelines (ingest_activity.py) read it.
//
//	ship.json: {"enabled": true, "url": "https://ivt.tpcsrv.com/v1/events",
//	            "token": "<per-host bearer>", "region": "use1"}

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
)

const (
	shipFile          = "ship.json"
	shipQueueSize     = 50000           // lines; ~15 MB worst case
	shipMaxBatchBytes = 1 << 20         // uncompressed bytes per POST
	shipFlushInterval = 5 * time.Second // max delay before a partial batch goes
	shipTimeout       = 5 * time.Second // per POST attempt
	shipStatsInterval = 5 * time.Minute
)

type shipConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Token   string `json:"token"`
	Region  string `json:"region"`
}

func (c shipConfig) on() bool {
	return c.Enabled && strings.HasPrefix(c.URL, "https://") && c.Token != "" && c.Region != ""
}

// loadShip reads ship.json into l. Any problem leaves shipping off.
func loadShip(dir string, l *ivtLists) {
	l.ship = shipConfig{}
	data, err := os.ReadFile(filepath.Join(dir, shipFile))
	if err != nil {
		return
	}
	var c shipConfig
	if json.Unmarshal(data, &c) == nil {
		l.ship = c
	}
}

type shipStats struct {
	queued, sent, dropped, failed atomic.Uint64
}

type shipper struct {
	store    *ivtStore
	ch       chan []byte
	client   *http.Client
	host     string
	maxBatch int
	every    time.Duration
	stats    shipStats
	// post is the transport; a field so tests can replace it.
	post func(ctx context.Context, cfg shipConfig, body []byte, lines int) error
}

func newShipper(store *ivtStore) *shipper {
	if store == nil {
		return nil
	}
	host, _ := os.Hostname()
	s := &shipper{
		store:    store,
		ch:       make(chan []byte, shipQueueSize),
		client:   &http.Client{Timeout: shipTimeout},
		host:     host,
		maxBatch: shipMaxBatchBytes,
		every:    shipFlushInterval,
	}
	s.post = s.httpPost
	go s.run()
	return s
}

// enqueue queues one activity line if shipping is on. Never blocks.
func (s *shipper) enqueue(line []byte, l *ivtLists) {
	if s == nil || l == nil || !l.ship.on() {
		return
	}
	select {
	case s.ch <- line:
		s.stats.queued.Add(1)
	default:
		s.stats.dropped.Add(1)
	}
}

func (s *shipper) run() {
	flush := time.NewTicker(s.every)
	stats := time.NewTicker(shipStatsInterval)
	defer flush.Stop()
	defer stats.Stop()
	var buf bytes.Buffer
	lines := 0
	for {
		select {
		case line := <-s.ch:
			buf.Write(line)
			buf.WriteByte('\n')
			lines++
			if buf.Len() >= s.maxBatch {
				s.flush(&buf, lines)
				lines = 0
			}
		case <-flush.C:
			if lines > 0 {
				s.flush(&buf, lines)
				lines = 0
			}
		case <-stats.C:
			s.logStats()
		}
	}
}

// flush sends the batch (one retry), then resets buf whatever happened:
// the pipeline is best-effort and fail-open, so a lost batch is counted,
// never kept in memory.
func (s *shipper) flush(buf *bytes.Buffer, lines int) {
	defer buf.Reset()
	l := s.store.get(time.Now())
	if l == nil || !l.ship.on() {
		s.stats.dropped.Add(uint64(lines)) // switched off while queued
		return
	}
	body, err := gzipBytes(buf.Bytes())
	if err != nil {
		s.stats.failed.Add(uint64(lines))
		return
	}
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), shipTimeout)
		err = s.post(ctx, l.ship, body, lines)
		cancel()
		if err == nil {
			s.stats.sent.Add(uint64(lines))
			return
		}
		if attempt == 0 {
			time.Sleep(time.Second)
		}
	}
	s.stats.failed.Add(uint64(lines))
	glog.Warningf("activitylog: IVT ship of %d lines failed: %v", lines, err)
}

func (s *shipper) httpPost(ctx context.Context, cfg shipConfig, body []byte, lines int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-TPC-Region", cfg.Region)
	req.Header.Set("X-TPC-Host", s.host)
	req.Header.Set("X-TPC-Lines", fmt.Sprint(lines))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collector returned %d", resp.StatusCode)
	}
	return nil
}

func (s *shipper) logStats() {
	q, sent, d, f := s.stats.queued.Load(), s.stats.sent.Load(), s.stats.dropped.Load(), s.stats.failed.Load()
	if q == 0 && d == 0 {
		return
	}
	glog.Infof("activitylog: IVT ship totals queued=%d sent=%d dropped=%d failed=%d backlog=%d", q, sent, d, f, len(s.ch))
}

func gzipBytes(b []byte) ([]byte, error) {
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

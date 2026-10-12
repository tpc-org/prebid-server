// Package hol100diag provides a one-shot, sanitized HOL-100 observation.
// It never writes request IDs, message content, headers, bodies or error text.
package hol100diag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

type Stage uint8

const (
	Processed Stage = iota + 1
	AdapterInput
	Generated
	HTTPExecute
	HTTPWritten
	HTTPResult
	Parsed
	BidderHook
	ResponseHook
)

type Category uint8

const (
	OK Category = iota
	Unknown
	MissingField
	Deadline
	Transport
	HTTPRejected
	NoAd
	ParseFailure
	HookRejected
	Throttled
	RequestFailure
)

type Fields struct {
	Request        bool `json:"request"`
	Response       bool `json:"response"`
	Timestamp      bool `json:"timestamp"`
	DeviceContext  bool `json:"device_context"`
	SurfaceMatches bool `json:"surface_matches"`
	SlotMatches    bool `json:"slot_matches"`
	ValidJSON      bool `json:"valid_json"`
}
type Event struct {
	Stage      Stage    `json:"stage"`
	Category   Category `json:"category"`
	Count      int      `json:"count,omitempty"`
	HTTPStatus int      `json:"http_status,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms"`
}
type Record struct {
	Kind          string    `json:"kind"`
	Token         string    `json:"token,omitempty"`
	UTC           time.Time `json:"utc"`
	Merged        Fields    `json:"merged"`
	Adapter       Fields    `json:"adapter"`
	Configured    bool      `json:"configured"`
	TimeoutMS     int64     `json:"timeout_ms"`
	Events        []Event   `json:"events,omitempty"`
	EventsDropped int       `json:"events_dropped"`
	FinalBids     int       `json:"final_bids"`
	FinalReached  bool      `json:"final_reached"`
	Admitted      uint64    `json:"admitted,omitempty"`
	Written       uint64    `json:"written,omitempty"`
	Dropped       uint64    `json:"dropped"`
	WriteFailures uint64    `json:"write_failures"`
	Unfinished    uint64    `json:"unfinished"`
}
type Collector struct {
	until     time.Time
	limit     uint64
	admitted  atomic.Uint64
	completed atomic.Uint64
	dropped   atomic.Uint64
	queue     chan Record
	stop      chan struct{}
	done      chan struct{}
}
type Trace struct {
	mu        sync.Mutex
	collector *Collector
	start     time.Time
	impID     string // used in memory for final bid matching; never serialized
	record    Record
	finished  bool
}
type contextKey struct{}

// Activation requires both startup environment values. An absolute deadline
// cannot be extended by touching a file or restarting PBS. O_EXCL refuses reuse.
var defaultCollector = fromEnvironment()

func fromEnvironment() *Collector {
	path, until := os.Getenv("PBS_HOL100_DIAGNOSTICS_FILE"), os.Getenv("PBS_HOL100_DIAGNOSTICS_UNTIL")
	if path == "" && until == "" {
		return nil
	}
	deadline, err := time.Parse(time.RFC3339, until)
	if err == nil {
		var c *Collector
		c, err = newCollector(path, deadline, 1000)
		if err == nil {
			return c
		}
	}
	glog.Warning("HOL-100 diagnostics disabled: invalid activation or unavailable output")
	return nil
}
func newCollector(path string, until time.Time, limit uint64) (*Collector, error) {
	remaining := time.Until(until)
	if remaining <= 0 || remaining > 15*time.Minute || limit == 0 || limit > 1000 {
		return nil, errors.New("invalid diagnostic bound")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	c := &Collector{until: until, limit: limit, queue: make(chan Record, 64), stop: make(chan struct{}, 1), done: make(chan struct{})}
	go c.write(f, remaining)
	return c, nil
}
func (c *Collector) write(f *os.File, remaining time.Duration) {
	defer close(c.done)
	defer f.Close()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	enc := json.NewEncoder(f)
	var written, failures uint64
	write := func(r Record) {
		if enc.Encode(r) != nil {
			failures++
		} else {
			written++
		}
	}
	for {
		select {
		case r := <-c.queue:
			write(r)
		case <-timer.C:
			goto finish
		case <-c.stop:
			goto finish
		}
	}
finish:
	for {
		select {
		case r := <-c.queue:
			write(r)
		default:
			goto summary
		}
	}
summary:
	admitted, completed := c.admitted.Load(), c.completed.Load()
	r := Record{Kind: "summary", UTC: time.Now().UTC(), Admitted: admitted, Written: written, Dropped: c.dropped.Load(), WriteFailures: failures, Unfinished: admitted - completed}
	if enc.Encode(r) != nil || failures > 0 {
		glog.Warningf("HOL-100 diagnostics output failure: admitted=%d written=%d dropped=%d unfinished=%d", admitted, written, r.Dropped, r.Unfinished)
	}
}
func Begin(ctx context.Context, req *openrtb_ext.RequestWrapper) (context.Context, *Trace) {
	return defaultCollector.begin(ctx, req)
}
func (c *Collector) begin(ctx context.Context, req *openrtb_ext.RequestWrapper) (context.Context, *Trace) {
	if c == nil || req == nil || req.Test == 1 || len(req.Imp) != 1 || !time.Now().Before(c.until) {
		return ctx, nil
	}
	// Single-impression scope prevents misattributing a batched adapter response.
	var ext struct {
		Prebid struct {
			StoredRequest struct {
				ID string `json:"id"`
			} `json:"storedrequest"`
			Bidder map[string]json.RawMessage `json:"bidder"`
		} `json:"prebid"`
	}
	if json.Unmarshal(req.Imp[0].Ext, &ext) != nil || ext.Prebid.StoredRequest.ID != "motortalk-2a3f7609" {
		return ctx, nil
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return ctx, nil
	}
	for {
		n := c.admitted.Load()
		if n >= c.limit {
			return ctx, nil
		}
		if c.admitted.CompareAndSwap(n, n+1) {
			break
		}
	}
	tr := &Trace{collector: c, start: time.Now(), impID: req.Imp[0].ID, record: Record{Kind: "auction", Token: hex.EncodeToString(token[:]), UTC: time.Now().UTC(), Configured: ext.Prebid.Bidder["imprezia"] != nil, Merged: presence(ext.Prebid.Bidder["imprezia"])}}
	if deadline, ok := ctx.Deadline(); ok {
		tr.record.TimeoutMS = max(0, time.Until(deadline).Milliseconds())
	}
	return context.WithValue(ctx, contextKey{}, tr), tr
}
func FromContext(ctx context.Context) *Trace { tr, _ := ctx.Value(contextKey{}).(*Trace); return tr }
func presence(raw json.RawMessage) Fields {
	var p openrtb_ext.ExtImpImprezia
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return Fields{}
	}
	return Fields{Request: p.Request != "", Response: p.Response != "", Timestamp: p.Timestamp != "", DeviceContext: p.DeviceContext != nil, SurfaceMatches: p.SiteID == "27d0f05d-3dc0-4958-8842-2564fe96932a", SlotMatches: p.PlacementID == "motor-talk-main", ValidJSON: true}
}

// Input observes the rebuilt, bidder-specific request after privacy and hooks.
func (tr *Trace) Input(req *openrtb2.BidRequest, adapter bool) {
	if tr == nil || req == nil || len(req.Imp) != 1 {
		return
	}
	var ext struct {
		Bidder json.RawMessage `json:"bidder"`
		Prebid struct {
			Bidder map[string]json.RawMessage `json:"bidder"`
		} `json:"prebid"`
	}
	_ = json.Unmarshal(req.Imp[0].Ext, &ext)
	raw := ext.Bidder
	if !adapter {
		raw = ext.Prebid.Bidder["imprezia"]
	}
	f := presence(raw)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.finished || !time.Now().Before(tr.collector.until) {
		return
	}
	if adapter {
		tr.record.Adapter = f
	}
}
func (tr *Trace) Event(stage Stage, category Category, count, status int) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.finished || !time.Now().Before(tr.collector.until) {
		return
	}
	if stage < Processed || stage > ResponseHook || category > RequestFailure {
		return
	}
	if len(tr.record.Events) >= 32 {
		tr.record.EventsDropped++
		return
	}
	tr.record.Events = append(tr.record.Events, Event{Stage: stage, Category: category, Count: count, HTTPStatus: status, ElapsedMS: time.Since(tr.start).Milliseconds()})
}
func (tr *Trace) MissingRequired() bool {
	if tr == nil {
		return false
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	f := tr.record.Adapter
	return f.ValidJSON && (!f.Request || !f.Response || !f.Timestamp || !f.DeviceContext)
}
func (tr *Trace) Finish(response *openrtb2.BidResponse, reached bool) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.finished {
		return
	}
	tr.finished = true
	if !time.Now().Before(tr.collector.until) {
		return
	}
	tr.record.FinalReached = reached
	if response != nil {
		for _, seat := range response.SeatBid {
			if seat.Seat == "imprezia" {
				for _, bid := range seat.Bid {
					if bid.ImpID == tr.impID {
						tr.record.FinalBids++
					}
				}
			}
		}
	}
	select {
	case tr.collector.queue <- tr.record:
	default:
		tr.collector.dropped.Add(1)
	}
	if tr.collector.completed.Add(1) == tr.collector.limit {
		select {
		case tr.collector.stop <- struct{}{}:
		default:
		}
	}
}

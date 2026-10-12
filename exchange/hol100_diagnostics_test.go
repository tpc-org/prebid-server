package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/adapters/imprezia"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/currency"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/exchange/entities"
	"github.com/prebid/prebid-server/v4/experiment/adscert"
	"github.com/prebid/prebid-server/v4/hooks/hookexecution"
	"github.com/prebid/prebid-server/v4/internal/hol100diag"
	metricsConfig "github.com/prebid/prebid-server/v4/metrics/config"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/require"
)

type diagnosticTransport struct {
	calls  int
	status int
	body   string
	err    error
}

func (d *diagnosticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d.calls++
	if d.err != nil {
		return nil, d.err
	}
	return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader(d.body)), Header: http.Header{}}, nil
}

func TestHOL100DiagnosticPipeline(t *testing.T) {
	for _, tc := range []struct {
		name         string
		missing      string
		status       int
		body         string
		transportErr error
		wantCalls    int
		wantCategory hol100diag.Category
		wantParsed   int
		wantBids     int
		rejectHook   bool
	}{
		{name: "missing request", missing: "request", wantCategory: hol100diag.MissingField},
		{name: "missing response", missing: "response", wantCategory: hol100diag.MissingField},
		{name: "missing timestamp", missing: "timestamp", wantCategory: hol100diag.MissingField},
		{name: "missing device", missing: "deviceContext", wantCategory: hol100diag.MissingField},
		{name: "204", status: 204, wantCalls: 1, wantCategory: hol100diag.NoAd},
		{name: "no ad", status: 200, body: `{"ad":null}`, wantCalls: 1, wantCategory: hol100diag.NoAd},
		{name: "rejected", status: 403, body: `PRIVATE_PARTNER_ERROR`, wantCalls: 1, wantCategory: hol100diag.HTTPRejected},
		{name: "malformed", status: 200, body: `PRIVATE_BAD_BODY`, wantCalls: 1, wantCategory: hol100diag.ParseFailure},
		{name: "transport", transportErr: errors.New("PRIVATE_TRANSPORT_ERROR"), wantCalls: 1, wantCategory: hol100diag.Transport},
		{name: "deadline", transportErr: context.DeadlineExceeded, wantCalls: 1, wantCategory: hol100diag.Deadline},
		{name: "bid", status: 200, body: `{"ad":{"creative":{"title":"PRIVATE_CREATIVE"},"clickUrl":"https://example.invalid/click","impression":{"impressionUuid":"PRIVATE_CREATIVE_ID"}}}`, wantCalls: 1, wantCategory: hol100diag.OK, wantParsed: 1, wantBids: 1},
		{name: "validation rejection", status: 200, body: `{"ad":{"creative":{"title":"PRIVATE_CREATIVE"},"clickUrl":"https://example.invalid/click","impression":{}}}`, wantCalls: 1, wantCategory: hol100diag.BidRejected, wantParsed: 1},
		{name: "response hook rejection", status: 200, body: `{"ad":{"creative":{"title":"PRIVATE_CREATIVE"},"clickUrl":"https://example.invalid/click","impression":{"impressionUuid":"PRIVATE_CREATIVE_ID"}}}`, wantCalls: 1, wantCategory: hol100diag.HookRejected, wantParsed: 1, rejectHook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.jsonl")
			c, err := hol100diag.New(path, time.Now().Add(time.Second))
			require.NoError(t, err)
			params := map[string]any{"siteId": "27d0f05d-3dc0-4958-8842-2564fe96932a", "placementId": "motor-talk-main", "request": "PRIVATE_CHAT", "response": "PRIVATE_REPLY", "timestamp": "PRIVATE_TIMESTAMP", "deviceContext": map[string]any{}, "bidPrice": 14}
			if tc.missing != "" {
				delete(params, tc.missing)
			}
			mergedExt, _ := json.Marshal(map[string]any{"prebid": map[string]any{"storedrequest": map[string]string{"id": "motortalk-2a3f7609"}, "bidder": map[string]any{"imprezia": params}}})
			req := &openrtb2.BidRequest{ID: "PRIVATE_ID", Imp: []openrtb2.Imp{{ID: "PRIVATE_IMP", Ext: mergedExt}}}
			ctx, tr := c.Begin(context.Background(), &openrtb_ext.RequestWrapper{BidRequest: req})
			require.NotNil(t, tr)
			ext, _ := json.Marshal(map[string]any{"bidder": params})
			req.Imp[0].Ext = ext
			adapter, err := imprezia.Builder(openrtb_ext.BidderImprezia, config.Adapter{Endpoint: "https://example.invalid/ads", ExtraAdapterInfo: `{"apiKey":"PRIVATE_KEY","bidPrice":14}`}, config.Server{})
			require.NoError(t, err)
			transport := &diagnosticTransport{status: tc.status, body: tc.body, err: tc.transportErr}
			bidder := &BidderAdapter{Bidder: adapter, BidderName: openrtb_ext.BidderImprezia, Client: &http.Client{Transport: transport}, me: &metricsConfig.NilMetricsEngine{}}
			invoke := func(callCtx context.Context) ([]*entities.PbsOrtbSeatBid, []error) {
				var executor hookexecution.StageExecutor = &hookexecution.EmptyHookExecutor{}
				if tc.rejectHook {
					executor = &diagnosticRejectHook{}
				}
				seats, _, errs := addValidatedBidderMiddleware(bidder).requestBid(callCtx, BidderRequest{BidRequest: req, BidderName: openrtb_ext.BidderImprezia, BidderCoreName: openrtb_ext.BidderImprezia}, currency.NewConstantRates(), &adapters.ExtraRequestInfo{}, &adscert.NilSigner{}, bidRequestOptions{}, openrtb_ext.ExtAlternateBidderCodes{}, executor, nil)
				return seats, errs
			}
			seats, errs := invoke(ctx)
			require.Equal(t, tc.wantCalls, transport.calls)
			var final []openrtb2.Bid
			for _, seat := range seats {
				for _, b := range seat.Bids {
					final = append(final, *b.Bid)
				}
			}
			require.Len(t, final, tc.wantBids)
			tr.Finish(&openrtb2.BidResponse{SeatBid: []openrtb2.SeatBid{{Seat: "imprezia", Bid: final}}}, true)
			baseline, baselineErrs := invoke(context.Background())
			require.Equal(t, baseline, seats, "diagnostics must preserve bids")
			require.Len(t, errs, len(baselineErrs))
			for i := range errs {
				require.Equal(t, errortypes.ReadCode(baselineErrs[i]), errortypes.ReadCode(errs[i]))
			}
			require.Equal(t, 2*tc.wantCalls, transport.calls, "diagnostics must not add calls")
			<-c.Done()
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "PRIVATE_")
			var record hol100diag.Record
			require.NoError(t, json.Unmarshal([]byte(strings.Split(string(raw), "\n")[0]), &record))
			found := false
			parsed := 0
			for _, event := range record.Events {
				if event.Category == tc.wantCategory {
					found = true
				}
				if event.Stage == hol100diag.Parsed {
					parsed += event.Count
				}
			}
			require.Equal(t, tc.wantParsed, parsed)
			require.Equal(t, tc.wantBids, record.FinalBids)
			require.True(t, found, "missing expected diagnostic outcome: %s", string(raw))
		})
	}
}

type diagnosticRejectHook struct {
	hookexecution.EmptyHookExecutor
}

func (*diagnosticRejectHook) ExecuteRawBidderResponseStage(*adapters.BidderResponse, string) *hookexecution.RejectError {
	return &hookexecution.RejectError{}
}

func TestHOL100ActualHTTPWriteAndBidderScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	c, err := hol100diag.New(path, time.Now().Add(time.Second))
	require.NoError(t, err)
	request := &openrtb_ext.RequestWrapper{BidRequest: &openrtb2.BidRequest{Imp: []openrtb2.Imp{{ID: "private", Ext: json.RawMessage(`{"prebid":{"storedrequest":{"id":"motortalk-2a3f7609"}}}`)}}}}
	ctx, tr := c.Begin(context.Background(), request)
	require.NotNil(t, tr)
	bidder := &BidderAdapter{BidderName: openrtb_ext.BidderImprezia, Client: server.Client(), me: &metricsConfig.NilMetricsEngine{}}
	result := bidder.doRequestImpl(ctx, &adapters.RequestData{Method: "POST", Uri: server.URL}, func(string, ...interface{}) {}, time.Now(), nil)
	require.NoError(t, result.err)
	bidder.BidderName = openrtb_ext.BidderThrad
	result = bidder.doRequestImpl(ctx, &adapters.RequestData{Method: "POST", Uri: server.URL}, func(string, ...interface{}) {}, time.Now(), nil)
	require.NoError(t, result.err)
	tr.Finish(nil, true)
	<-c.Done()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var record hol100diag.Record
	require.NoError(t, json.Unmarshal([]byte(strings.Split(string(raw), "\n")[0]), &record))
	executes, writes := 0, 0
	for _, event := range record.Events {
		if event.Stage == hol100diag.HTTPExecute {
			executes++
		}
		if event.Stage == hol100diag.HTTPWritten && event.Category == hol100diag.OK {
			writes++
		}
	}
	require.Equal(t, 1, executes)
	require.Equal(t, 1, writes)
}

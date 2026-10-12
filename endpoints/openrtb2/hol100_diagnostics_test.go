package openrtb2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/analytics"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/hooks/hookexecution"
	"github.com/prebid/prebid-server/v4/internal/hol100diag"
	"github.com/prebid/prebid-server/v4/metrics"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/stretchr/testify/require"
)

type diagnosticResponseHooks struct {
	hookexecution.EmptyHookExecutor
	remove  bool
	replace bool
	unknown bool
}

func (h diagnosticResponseHooks) ExecuteAuctionResponseStage(response *openrtb2.BidResponse) {
	if h.remove {
		response.SeatBid = nil
	}
}
func (h diagnosticResponseHooks) ExecuteExitpointStage(response any, _ http.ResponseWriter) any {
	if h.replace {
		return &openrtb2.BidResponse{ID: "private-replacement"}
	}
	if h.unknown {
		return map[string]string{"private": "replacement"}
	}
	return response
}
func TestHOL100FinalResponseAfterHooks(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		hooks                  diagnosticResponseHooks
		auctionBids, finalBids int
		known                  bool
	}{
		{name: "bid survives", auctionBids: 1, finalBids: 1, known: true},
		{name: "auction response removes bid", hooks: diagnosticResponseHooks{remove: true}, known: true},
		{name: "exitpoint replaces response", hooks: diagnosticResponseHooks{replace: true}, auctionBids: 1, known: true},
		{name: "unclassified exitpoint object", hooks: diagnosticResponseHooks{unknown: true}, auctionBids: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.jsonl")
			c, err := hol100diag.New(path, time.Now().Add(time.Second))
			require.NoError(t, err)
			req := &openrtb2.BidRequest{Imp: []openrtb2.Imp{{ID: "private-imp", Ext: json.RawMessage(`{"prebid":{"storedrequest":{"id":"motortalk-2a3f7609"}}}`)}}}
			_, tr := c.Begin(context.Background(), &openrtb_ext.RequestWrapper{BidRequest: req})
			require.NotNil(t, tr)
			response := &openrtb2.BidResponse{SeatBid: []openrtb2.SeatBid{{Seat: "imprezia", Bid: []openrtb2.Bid{{ID: "private-bid", ImpID: "private-imp", Price: 14}}}}}
			writer := httptest.NewRecorder()
			_, ao := sendAuctionResponse(writer, tc.hooks, response, req, &config.Account{}, metrics.Labels{}, analytics.AuctionObject{Response: response}, tr)
			tr.Finish(ao.Response, true)
			<-c.Done()
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private")
			var record hol100diag.Record
			require.NoError(t, json.Unmarshal([]byte(strings.Split(string(raw), "\n")[0]), &record))
			require.Equal(t, tc.auctionBids, record.AuctionBids)
			require.Equal(t, tc.finalBids, record.FinalBids)
			require.Equal(t, tc.known, record.FinalKnown)
			require.True(t, record.Encoded)
			if !tc.hooks.unknown {
				var sent openrtb2.BidResponse
				require.NoError(t, json.Unmarshal(writer.Body.Bytes(), &sent))
				n := 0
				for _, seat := range sent.SeatBid {
					n += len(seat.Bid)
				}
				require.Equal(t, n, record.FinalBids)
			}
		})
	}
}

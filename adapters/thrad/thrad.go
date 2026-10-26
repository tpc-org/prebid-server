package thrad

// Package thrad implements a Prebid Server adapter for Thrad (thrad.ai).
//
// ── Two protocols, chosen per placement ──────────────────────────────────────
//
// Thrad retires bid request protocol v1 on 2026-11-04 in favour of v2. To roll
// v2 out one placement at a time, this adapter speaks both: v1.go and v2.go.
// Each imp picks its protocol from its own params (ExtImpThrads.Protocol, set
// statically in the Stored Imp as "protocol": 2). Imps without it use the
// adapter's default: v1 for "thrad", v2 for the "thradx" alias (the test-only
// Stored Imp sayhola-767d5762). Once every placement is on v2, flip the
// default, then delete v1.go and the Protocol param.
//
// Both protocols share key selection: same keys, same pbs.yaml extra_info
// layout (only the auth header differs — thrad-api-key vs Authorization:
// Bearer).
//
// ── Key selection (per-publisher, then staging vs production) ────────────────
//
// Keys are configured in pbs-settings/pbs.yaml under adapters.thrad.extra_info
// as a default pair plus a per-publisher override map, keyed by our own
// Publisher.slug (the Stored Imp's publisherId):
//
//	adapters:
//	  thrad:
//	    extra_info: '{"productionKey":"pk_live_default","stagingKey":"pk_staging_default","publishers":{"drawify":{"productionKey":"pk_live_drawify","stagingKey":"pk_staging_drawify"}}}'
//
// The top-level pair is the fallback for imps without a publisherId, or whose
// publisherId has no entry. Within the selected pair, test=1 (set by Prebid.js
// with ?pbjs_debug=true) picks stagingKey; real traffic uses productionKey,
// falling back to the shared default productionKey rather than a publisher's
// own staging key (see selectKey).
//
// ── Conversational context ───────────────────────────────────────────────────
//
// userId/chatId/messages are dynamic per auction. Client bundles read them
// from window.tpc.data into params.thrad, which tpcBidAdapter forwards as
// ext.prebid.bidder.thrad; server-side/mobile callers' generic block is
// expanded by tpc.paramfanout. PBS merges that with the Stored Imp's static
// params (publisherId, protocol) before calling this adapter.

import (
	"encoding/json"
	"fmt"
	"text/template"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
)

// v1Endpoint is fixed: bidder-info's endpoint is v2's, and v1 is temporary.
const v1Endpoint = "https://ssp.thrads.ai/api/v1/ssp/bid-request"

// publisherKeys is one Thrad Publisher's production/staging key pair.
type publisherKeys struct {
	ProductionKey string `json:"productionKey"`
	StagingKey    string `json:"stagingKey"`
}

// extraInfo is parsed from pbs.yaml adapters.thrad.extra_info (JSON string).
type extraInfo struct {
	publisherKeys
	Publishers map[string]publisherKeys `json:"publishers"`
}

type adapter struct {
	v2Endpoint      *template.Template
	v1Endpoint      string
	keys            extraInfo
	defaultProtocol int
}

// Builder is the constructor registered with PBS, for "thrad" and its
// "thradx" alias (bidderName tells them apart).
func Builder(bidderName openrtb_ext.BidderName, cfg config.Adapter, server config.Server) (adapters.Bidder, error) {
	tmpl, err := template.New("endpointTemplate").Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("thrad: unable to parse endpoint template: %w", err)
	}

	a := &adapter{v2Endpoint: tmpl, v1Endpoint: v1Endpoint, defaultProtocol: 1}
	if bidderName == "thradx" {
		a.defaultProtocol = 2
	}

	if cfg.ExtraAdapterInfo != "" {
		if err := json.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.keys); err != nil {
			return nil, fmt.Errorf("thrad: unable to parse extra_info: %w", err)
		}
	}
	if a.keys.ProductionKey == "" && a.keys.StagingKey == "" && len(a.keys.Publishers) == 0 {
		return nil, fmt.Errorf("thrad: extra_info must contain productionKey/stagingKey and/or a publishers map")
	}

	return a, nil
}

// selectKey returns the API key for a given request and publisher. Falls back
// to the default pair when publisherID is empty or unknown. test=1 uses the
// selected pair's stagingKey; real traffic uses its productionKey, else the
// shared default productionKey — never a publisher's own staging key, which
// isn't a safe stand-in for live traffic (2026-07-25: learnrithm's
// staging-only key returned 500s on real traffic).
func (a *adapter) selectKey(request *openrtb2.BidRequest, publisherID string) string {
	keys := a.keys.publisherKeys
	if publisherID != "" {
		if pk, ok := a.keys.Publishers[publisherID]; ok {
			keys = pk
		}
	}
	if request.Test == 1 && keys.StagingKey != "" {
		return keys.StagingKey
	}
	if keys.ProductionKey != "" {
		return keys.ProductionKey
	}
	if a.keys.publisherKeys.ProductionKey != "" {
		return a.keys.publisherKeys.ProductionKey
	}
	return keys.StagingKey
}

// MakeRequests makes one Thrad call per imp, in that imp's protocol.
func (a *adapter) MakeRequests(request *openrtb2.BidRequest, _ *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	var requests []*adapters.RequestData
	var errs []error

	for i := range request.Imp {
		imp := request.Imp[i]

		var bidderExt adapters.ExtImpBidder
		if err := json.Unmarshal(imp.Ext, &bidderExt); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrad: invalid imp.ext for imp %s: %s", imp.ID, err),
			})
			continue
		}
		var params openrtb_ext.ExtImpThrads
		if err := json.Unmarshal(bidderExt.Bidder, &params); err != nil {
			errs = append(errs, &errortypes.BadInput{
				Message: fmt.Sprintf("thrad: invalid imp.ext.bidder for imp %s: %s", imp.ID, err),
			})
			continue
		}

		var req *adapters.RequestData
		var err error
		if a.protocolFor(params) == 2 {
			req, err = a.makeRequestV2(request, imp, params)
		} else {
			req, err = a.makeRequestV1(request, imp, params)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		requests = append(requests, req)
	}

	return requests, errs
}

func (a *adapter) protocolFor(params openrtb_ext.ExtImpThrads) int {
	if params.Protocol == 1 || params.Protocol == 2 {
		return params.Protocol
	}
	return a.defaultProtocol
}

// MakeBids parses the response in the protocol its call was made with.
func (a *adapter) MakeBids(request *openrtb2.BidRequest, requestData *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	if requestData != nil && requestData.Uri == a.v1Endpoint {
		return a.makeBidsV1(request, requestData, response)
	}
	return a.makeBidsV2(request, requestData, response)
}

// callImpID is the imp a single-imp call was made for: the call's own ImpIDs
// (always set by MakeRequests), else the request's only imp.
func callImpID(request *openrtb2.BidRequest, requestData *adapters.RequestData) string {
	if requestData != nil && len(requestData.ImpIDs) == 1 {
		return requestData.ImpIDs[0]
	}
	if len(request.Imp) == 1 {
		return request.Imp[0].ID
	}
	return ""
}

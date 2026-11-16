// Package kontext implements the Prebid Server adapter for Kontext
// (rtb.kontext.ai), a standard OpenRTB 2.6 bidder (HOL-144).
//
// Contract: "Kontext Bidder — Integration Guide" v5.0 (November 2026).
//
// Configuration lives in pbs-settings/pbs.yaml:
//
//	adapters:
//	  kontext:
//	    extra_info: '{"partnerId":"...","token":"..."}'
//
// The bid URL is <endpoint>/<partnerId>, authenticated with
// "Authorization: Bearer <token>". Builder never fails on missing or
// unreadable extra_info: an unconfigured adapter sends nothing and reports a
// per-bidder error instead. A Builder error would stop PBS at boot, and the
// image is released before Kontext issues the credentials.
//
// What the request side does beyond passing the OpenRTB request through:
//   - sends only native impressions on web (site) requests, the format and
//     environment agreed with Kontext; anything else is a no-bid on their side
//   - never sends a child-directed request (regs.coppa = 1)
//   - declares a first-price auction in USD
//   - adds a complete supply chain ending in our node when the caller sent none
//   - sets imp.tagid from the Stored Imp's placementId
//
// User identifiers (user.id, user.eids with hashed emails) and privacy
// signals are passed through exactly as PBS hands them over.
package kontext

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/prebid/openrtb/v20/openrtb2"
	"github.com/prebid/prebid-server/v4/adapters"
	"github.com/prebid/prebid-server/v4/config"
	"github.com/prebid/prebid-server/v4/errortypes"
	"github.com/prebid/prebid-server/v4/openrtb_ext"
	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

// defaultASI is the domain of our sellers.json (https://sayhola.ai/sellers.json).
const defaultASI = "sayhola.ai"

type extraInfo struct {
	PartnerID string `json:"partnerId"`
	Token     string `json:"token"`
	// ASI overrides the supply-chain node's domain. Defaults to defaultASI.
	ASI string `json:"asi"`
}

type adapter struct {
	endpoint string
	info     extraInfo
}

// Builder never returns an error; see the package doc.
func Builder(_ openrtb_ext.BidderName, cfg config.Adapter, _ config.Server) (adapters.Bidder, error) {
	a := &adapter{endpoint: strings.TrimRight(cfg.Endpoint, "/")}
	if cfg.ExtraAdapterInfo != "" {
		// Unreadable extra_info leaves the adapter unconfigured.
		if err := jsonutil.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.info); err != nil {
			a.info = extraInfo{}
		}
	}
	if a.info.ASI == "" {
		a.info.ASI = defaultASI
	}
	return a, nil
}

func (a *adapter) configured() bool {
	return a.endpoint != "" && a.info.PartnerID != "" && a.info.Token != ""
}

func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	if !a.configured() {
		return nil, []error{&errortypes.BadInput{Message: "kontext: partnerId and token are not configured"}}
	}
	if request.Site == nil || request.App != nil {
		return nil, []error{&errortypes.BadInput{Message: "kontext: only web (site) requests are sent"}}
	}
	if request.Regs != nil && request.Regs.COPPA == 1 {
		return nil, []error{&errortypes.BadInput{Message: "kontext: child-directed requests are not sent"}}
	}

	var errs []error
	var sellerID string
	imps := make([]openrtb2.Imp, 0, len(request.Imp))
	for i := range request.Imp {
		imp, params, err := buildImp(request.Imp[i], requestInfo)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if sellerID == "" {
			sellerID = params.SellerID
		}
		imps = append(imps, imp)
	}
	if len(imps) == 0 {
		return nil, errs
	}

	out := *request
	out.Imp = imps
	out.AT = 1
	out.Cur = []string{"USD"}
	out.Source = sourceWithSupplyChain(request.Source, a.info.ASI, sellerID)

	body, err := jsonutil.Marshal(&out)
	if err != nil {
		return nil, append(errs, fmt.Errorf("kontext: failed to marshal request: %w", err))
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("X-OpenRTB-Version", "2.6")
	headers.Set("Authorization", "Bearer "+a.info.Token)

	return []*adapters.RequestData{{
		Method:  http.MethodPost,
		Uri:     a.endpoint + "/" + a.info.PartnerID,
		Body:    body,
		Headers: headers,
		ImpIDs:  openrtb_ext.GetImpIDs(imps),
	}}, errs
}

// buildImp returns the impression as Kontext should receive it: native only,
// floor in USD, tagid set, and no PBS-internal ext.
func buildImp(imp openrtb2.Imp, requestInfo *adapters.ExtraRequestInfo) (openrtb2.Imp, openrtb_ext.ExtImpKontext, error) {
	var params openrtb_ext.ExtImpKontext
	if imp.Native == nil {
		return imp, params, &errortypes.BadInput{Message: fmt.Sprintf("kontext: imp %s has no native object", imp.ID)}
	}

	var ext map[string]jsonutil.RawMessage
	if len(imp.Ext) > 0 {
		if err := jsonutil.Unmarshal(imp.Ext, &ext); err != nil {
			return imp, params, &errortypes.BadInput{Message: fmt.Sprintf("kontext: invalid imp.ext for imp %s: %s", imp.ID, err)}
		}
	}
	if raw, ok := ext["bidder"]; ok {
		if err := jsonutil.Unmarshal(raw, &params); err != nil {
			return imp, params, &errortypes.BadInput{Message: fmt.Sprintf("kontext: invalid imp.ext.bidder for imp %s: %s", imp.ID, err)}
		}
	}

	if imp.BidFloorCur != "" && !strings.EqualFold(imp.BidFloorCur, "USD") {
		if imp.BidFloor > 0 {
			floor, err := requestInfo.ConvertCurrency(imp.BidFloor, imp.BidFloorCur, "USD")
			if err != nil {
				return imp, params, &errortypes.BadInput{Message: fmt.Sprintf("kontext: cannot convert the floor of imp %s to USD: %s", imp.ID, err)}
			}
			imp.BidFloor = floor
		}
	}
	// Kontext accepts "USD" in upper case only, or no currency at all.
	imp.BidFloorCur = ""
	if imp.BidFloor > 0 {
		imp.BidFloorCur = "USD"
	}

	imp.Banner, imp.Video, imp.Audio = nil, nil, nil
	if imp.TagID == "" {
		imp.TagID = params.PlacementID
	}

	delete(ext, "bidder")
	delete(ext, "prebid")
	imp.Ext = nil
	if len(ext) > 0 {
		raw, err := jsonutil.Marshal(ext)
		if err != nil {
			return imp, params, fmt.Errorf("kontext: failed to marshal imp.ext for imp %s: %w", imp.ID, err)
		}
		imp.Ext = raw
	}
	return imp, params, nil
}

// sourceWithSupplyChain keeps a supply chain the caller already sent. When
// there is none and the Stored Imp names the publisher's seller id, it adds a
// complete chain whose only node is ours. Without a seller id nothing is
// added: a made-up node would be worse than the incomplete chain Kontext
// assumes.
func sourceWithSupplyChain(source *openrtb2.Source, asi, sellerID string) *openrtb2.Source {
	if sellerID == "" || (source != nil && source.SChain != nil) {
		return source
	}
	var out openrtb2.Source
	if source != nil {
		out = *source
	}
	hp := int8(1)
	out.SChain = &openrtb2.SupplyChain{
		Ver:      "1.0",
		Complete: 1,
		Nodes:    []openrtb2.SupplyChainNode{{ASI: asi, SID: sellerID, HP: &hp}},
	}
	return &out
}

func (a *adapter) MakeBids(request *openrtb2.BidRequest, _ *adapters.RequestData, response *adapters.ResponseData) (*adapters.BidderResponse, []error) {
	// 204 is Kontext's only no-bid signal and the normal outcome.
	if adapters.IsResponseStatusCodeNoContent(response) {
		return nil, nil
	}
	switch {
	case response.StatusCode == http.StatusBadRequest:
		return nil, []error{&errortypes.BadInput{Message: "kontext: request rejected as malformed (400)"}}
	case response.StatusCode != http.StatusOK:
		// 403 is a bad token or partner id; 5xx is transient. Neither is retried.
		return nil, []error{&errortypes.BadServerResponse{Message: fmt.Sprintf("kontext: unexpected status code %d", response.StatusCode)}}
	}

	var bidResp openrtb2.BidResponse
	if err := jsonutil.Unmarshal(response.Body, &bidResp); err != nil {
		return nil, []error{&errortypes.BadServerResponse{Message: fmt.Sprintf("kontext: failed to parse response: %s", err)}}
	}

	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(len(request.Imp))
	bidderResponse.Currency = "USD"
	for i := range bidResp.SeatBid {
		for j := range bidResp.SeatBid[i].Bid {
			bidderResponse.Bids = append(bidderResponse.Bids, &adapters.TypedBid{
				Bid:     &bidResp.SeatBid[i].Bid[j],
				BidType: openrtb_ext.BidTypeNative,
			})
		}
	}
	return bidderResponse, nil
}

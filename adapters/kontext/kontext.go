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
// "Authorization: Bearer <token>". Both values are host-specific, so the
// bidder is disabled in static/bidder-info/kontext.yaml and Builder is not
// called until pbs.yaml sets "disabled: false". Builder then fails, and PBS
// refuses to start, if extra_info is missing or incomplete: enable the bidder
// and add extra_info in the same settings release.
//
// What the request side does beyond passing the OpenRTB request through:
//   - sends only native impressions, the format agreed with Kontext, on web
//     (site) or in-app (app) requests. Exactly one of site and app must be
//     present: Kontext treats both or neither as a no-bid. Kontext also
//     no-bids an environment it has not agreed for our partner id, so app
//     requests earn nothing until Kontext enables app for us (HOL-164).
//   - never sends a child-directed request (regs.coppa = 1)
//   - declares a first-price auction in USD
//   - ends the supply chain in our node: appended to the chain a caller
//     sent (a reseller such as Velocity), or a complete one-node chain when
//     there is none (our own publishers)
//   - sets imp.tagid from the placementid param, over any tagid the caller
//     sent, so Kontext reports by our placement id (HOL-164)
//
// User identifiers (user.id, user.eids with hashed emails) and privacy
// signals are passed through exactly as PBS hands them over.
//
// The bid side is described on prepareBid.
package kontext

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

// Builder builds the Kontext adapter. It is only called when the bidder is
// enabled, and fails without an endpoint, a partner id and a token.
func Builder(_ openrtb_ext.BidderName, cfg config.Adapter, _ config.Server) (adapters.Bidder, error) {
	a := &adapter{endpoint: strings.TrimRight(cfg.Endpoint, "/")}
	if a.endpoint == "" {
		return nil, errors.New("kontext: endpoint is not configured")
	}
	if cfg.ExtraAdapterInfo == "" {
		return nil, errors.New("kontext: extra_info with partnerId and token is required")
	}
	if err := jsonutil.Unmarshal([]byte(cfg.ExtraAdapterInfo), &a.info); err != nil {
		return nil, fmt.Errorf("kontext: invalid extra_info: %w", err)
	}
	if a.info.PartnerID == "" || a.info.Token == "" {
		return nil, errors.New("kontext: extra_info must set partnerId and token")
	}
	if a.info.ASI == "" {
		a.info.ASI = defaultASI
	}
	return a, nil
}

func (a *adapter) MakeRequests(request *openrtb2.BidRequest, requestInfo *adapters.ExtraRequestInfo) ([]*adapters.RequestData, []error) {
	if (request.Site == nil) == (request.App == nil) {
		return nil, []error{&errortypes.BadInput{Message: "kontext: a request needs exactly one of site or app"}}
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
func buildImp(imp openrtb2.Imp, requestInfo *adapters.ExtraRequestInfo) (openrtb2.Imp, openrtb_ext.ImpExtKontext, error) {
	var params openrtb_ext.ImpExtKontext
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
	// Our placement id names the site and the path it reached us by, which is
	// what Kontext's revenue has to be traced back to. A reseller's own tagid
	// means nothing to us, so ours wins; a caller's tagid is kept only when
	// the placement id is not configured.
	if params.PlacementID != "" {
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

// sourceWithSupplyChain ends the supply chain in our node, so Kontext can
// match it against our listing in megabrain.co's sellers.json and our
// sellers.json. Without a seller id the request is left as it came: a made-up
// node would be worse than the incomplete chain Kontext then assumes.
//
//   - No chain from the caller (our own publishers): a complete chain whose
//     only node is ours.
//   - A chain from the caller (a reseller such as Velocity): our node is
//     appended, and the caller's "complete" flag is kept, since we add the
//     last hop and nothing else.
//   - A chain that already ends in our node is left alone.
//
// The caller's source and nodes are never modified in place: PBS shares the
// request between bidders.
func sourceWithSupplyChain(source *openrtb2.Source, asi, sellerID string) *openrtb2.Source {
	if sellerID == "" {
		return source
	}
	hp := int8(1)
	ours := openrtb2.SupplyChainNode{ASI: asi, SID: sellerID, HP: &hp}

	var out openrtb2.Source
	if source != nil {
		out = *source
	}
	if out.SChain == nil {
		out.SChain = &openrtb2.SupplyChain{Ver: "1.0", Complete: 1, Nodes: []openrtb2.SupplyChainNode{ours}}
		return &out
	}
	if n := len(out.SChain.Nodes); n > 0 {
		last := out.SChain.Nodes[n-1]
		if strings.EqualFold(last.ASI, asi) && last.SID == sellerID {
			return source
		}
	}
	chain := *out.SChain
	chain.Nodes = append(append(make([]openrtb2.SupplyChainNode, 0, len(chain.Nodes)+1), chain.Nodes...), ours)
	if chain.Ver == "" {
		chain.Ver = "1.0"
	}
	out.SChain = &chain
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
	// Kontext always answers in USD. Anything else cannot be priced.
	if bidResp.Cur != "" && bidResp.Cur != "USD" {
		return nil, []error{&errortypes.BadServerResponse{Message: fmt.Sprintf("kontext: unexpected response currency %q", bidResp.Cur)}}
	}

	offered := make(map[string]struct{}, len(request.Imp))
	for i := range request.Imp {
		if request.Imp[i].Native != nil {
			offered[request.Imp[i].ID] = struct{}{}
		}
	}

	var errs []error
	bidderResponse := adapters.NewBidderResponseWithBidsCapacity(len(request.Imp))
	bidderResponse.Currency = "USD"
	for i := range bidResp.SeatBid {
		seat := bidResp.SeatBid[i].Seat
		for j := range bidResp.SeatBid[i].Bid {
			bid := &bidResp.SeatBid[i].Bid[j]
			if err := prepareBid(bid, seat, offered); err != nil {
				errs = append(errs, err)
				continue
			}
			typed := &adapters.TypedBid{Bid: bid, BidType: openrtb_ext.BidTypeNative}
			if len(bid.ADomain) > 0 {
				typed.BidMeta = &openrtb_ext.ExtBidPrebidMeta{AdvertiserDomains: bid.ADomain}
			}
			bidderResponse.Bids = append(bidderResponse.Bids, typed)
		}
	}
	return bidderResponse, errs
}

// prepareBid checks one Kontext bid and makes it ready for the client:
//
//   - the bid must answer a native impression we sent and declare mtype 4
//   - ${AUCTION_PRICE} is expanded in nurl, burl and lurl. Nothing else in PBS
//     or Prebid.js expands it, and Kontext records an unexpanded price as
//     zero. Our auction is first price, so the clearing price is bid.price.
//     The value is a plain decimal, put in place; the rest of each signed URL
//     is untouched. ${AUCTION_LOSS} is left for whoever knows the loss reason.
//   - the native markup is normalised to a top-level Native 1.2 object (the
//     client's parser rejects the 1.1 {"native":{...}} wrapper) and gains
//     ext.kontext with nurl, lurl and the Kontext seat. Prebid.js fires burl
//     on render but nothing fires nurl or lurl for a native bid, so the
//     bundle reads them from there.
func prepareBid(bid *openrtb2.Bid, seat string, offered map[string]struct{}) error {
	if _, ok := offered[bid.ImpID]; !ok {
		return &errortypes.BadServerResponse{Message: fmt.Sprintf("kontext: bid %s answers unknown imp %q", bid.ID, bid.ImpID)}
	}
	if bid.MType != openrtb2.MarkupNative {
		return &errortypes.BadServerResponse{Message: fmt.Sprintf("kontext: bid %s has mtype %d, only native (4) was offered", bid.ID, bid.MType)}
	}

	price := strconv.FormatFloat(bid.Price, 'f', -1, 64)
	bid.NURL = strings.ReplaceAll(bid.NURL, priceMacro, price)
	bid.BURL = strings.ReplaceAll(bid.BURL, priceMacro, price)
	bid.LURL = strings.ReplaceAll(bid.LURL, priceMacro, price)

	adm, err := nativeAdm(bid.AdM, kontextExt{NURL: bid.NURL, LURL: bid.LURL, Seat: seat})
	if err != nil {
		return &errortypes.BadServerResponse{Message: fmt.Sprintf("kontext: bid %s: %s", bid.ID, err)}
	}
	bid.AdM = adm
	return nil
}

const priceMacro = "${AUCTION_PRICE}"

// kontextExt is what the native markup carries for the client under
// ext.kontext.
type kontextExt struct {
	LURL string `json:"lurl,omitempty"`
	NURL string `json:"nurl,omitempty"`
	Seat string `json:"seat,omitempty"`
}

// nativeAdm unwraps a Native 1.1 {"native":{...}} response and adds
// ext.kontext. Every other field is kept byte for byte.
func nativeAdm(adm string, ext kontextExt) (string, error) {
	var native map[string]jsonutil.RawMessage
	if err := jsonutil.Unmarshal([]byte(adm), &native); err != nil {
		return "", fmt.Errorf("native markup is not a JSON object: %s", err)
	}
	if _, hasAssets := native["assets"]; !hasAssets {
		if wrapped, ok := native["native"]; ok {
			native = nil
			if err := jsonutil.Unmarshal(wrapped, &native); err != nil {
				return "", fmt.Errorf("native markup wrapper is not a JSON object: %s", err)
			}
		}
	}
	if _, hasAssets := native["assets"]; !hasAssets {
		return "", fmt.Errorf("native markup has no assets")
	}

	admExt := map[string]jsonutil.RawMessage{}
	if raw, ok := native["ext"]; ok {
		if err := jsonutil.Unmarshal(raw, &admExt); err != nil {
			return "", fmt.Errorf("native markup ext is not a JSON object: %s", err)
		}
	}
	rawExt, err := marshalPlain(ext)
	if err != nil {
		return "", err
	}
	admExt["kontext"] = rawExt
	if native["ext"], err = marshalPlain(admExt); err != nil {
		return "", err
	}

	out, err := marshalPlain(native)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// marshalPlain encodes without HTML escaping, so "&" in a signed URL stays
// "&" instead of becoming "\u0026" inside the markup string.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

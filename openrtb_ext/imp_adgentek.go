package openrtb_ext

// ImpExtAdgentek defines the Adgentek imp params (HOL-157).
//
// SlotID is static and lives in the Stored Imp (or, for resold traffic, in
// dsp-connector's per-site params): it is the Adgentek placement id minted
// for one publisher, e.g. "ca10d818-1795354687823".
//
// Messages and SessionID are dynamic. Our own bundles do not send them as
// params: they send the conversation as bidder-scoped first-party data
// (site.ext.data.messages / site.ext.data.sessionId, applied by PBS to the
// adgentek bidder only), the same channel Kontext reads. dsp-connector, which
// builds params itself, sends them here instead. Params win when both exist.
//
// Nothing is "required" in static/bidder-params/adgentek.json, and nothing
// may be: PBS validates params before the adapter runs, and a required field
// that is absent rejects the whole imp, every other bidder on it included
// (the Imprezia incident, 2026-08-22). The adapter skips an imp it cannot
// send (no slot id, no conversation) with a soft error instead.
type ImpExtAdgentek struct {
	SlotID      string            `json:"slotId,omitempty"`
	Messages    []AdgentekMessage `json:"messages,omitempty"`
	SessionID   string            `json:"sessionId,omitempty"`
	SurfaceType string            `json:"surfaceType,omitempty"`
	Custom      map[string]string `json:"custom,omitempty"`
}

// AdgentekMessage is one conversation turn. Role is user, assistant or
// system.
type AdgentekMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

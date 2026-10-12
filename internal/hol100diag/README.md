# HOL-100 bounded observation

Motor-talk has configured Imprezia activity but zero final bids. Existing activity
logging counts configured bidders, not HTTP calls. This temporary observer answers:

1. Did merged input acquire required fields through paramfanout and reach the adapter?
2. Did the adapter generate a request, execute HTTP and write it to a connection?
3. Did Imprezia return no-ad, an error, or a parsed bid that PBS subsequently removed?

## Activation and bounds

Disabled unless both startup environment variables are supplied:

- `PBS_HOL100_DIAGNOSTICS_FILE`: absolute path on the existing analytics mount,
  using a new filename such as `/var/log/pbs-analytics/hol100-diagnostic-<release>.jsonl`.
- `PBS_HOL100_DIAGNOSTICS_UNTIL`: absolute UTC RFC3339 expiry, in the future and
  no more than 15 minutes from startup. Set it immediately before approved activation.

Hard ceiling: 1,000 admitted auctions or that deadline, whichever comes first.
An absolute expiry and exclusive file creation prevent accidental restart/reuse.
The file is mode 0600, owned by PBS's existing runtime user. Missing/invalid
activation or file-open failure disables capture without affecting auctions.

Scope: `/openrtb2/auction`, non-test, **single-impression** auctions referencing
`motortalk-2a3f7609`. Bidder and transport events observe direct Imprezia only.
Mixed auctions and alternate bidder aliases are excluded to avoid misattributing
batched responses. Report this sampling limitation; never extrapolate to excluded
traffic. The merged stage begins after stored request/imp resolution and request
validation. Requests rejected before that point are outside this observation.

Each trace has a generated internal token, field-presence/matching flags,
remaining auction/HTTP budgets, bidder tmax, fixed stage/category numbers,
relative elapsed times, HTTP status, bid counts and response-encoding success.
Request-scoped impression IDs exist only in memory for matching returned bids.
No content, client IDs, IPs, URLs, credentials, bodies, headers, or error text is
serialized. Nothing is added to partner requests or public responses.

The 64-record writer queue uses nonblocking submission. Each trace holds at most
32 events. Final records are emitted separately from `activity.log-*` and are
never ingested by reporting. A terminal summary records admitted/written/dropped,
write failures and unfinished traces; individual records count dropped events.
If the output fails, only fixed numeric loss counters are sent to the app log.
Do not call missing records successes. A stalled sink cannot block an auction;
it may delay the summary. Admission and event collection still expire on time.

## Reading records

| Stage | Meaning |
| --- | --- |
| 1 Processed | Processed-auction hooks completed or rejected |
| 2 AdapterInput | Bidder hook completed; adapter input reached |
| 3 Generated | Number of requests returned by MakeRequests |
| 4 HTTPExecute | Actual call to the context-aware HTTP client |
| 5 HTTPWritten | HTTP transport write callback; category 0 means successful write |
| 6 HTTPResult | HTTP status or fixed transport/pre-execution failure |
| 7 Parsed | Number of bids returned by MakeBids, before response hooks/validation |
| 8 BidderHook | Bidder request hook rejection |
| 9 ResponseHook | Parsed bidder-response hook rejection |
| 10 Validated | Bid count after generic bid validation |
| 11 Routed | Splitter included direct Imprezia with a live impression, or did not |

Categories: 0 OK, 1 unknown, 2 missing required field, 3 deadline,
4 transport, 5 HTTP rejection, 6 no-ad, 7 adapter parsing/conversion failure,
8 hook rejection, 9 throttled, 10 request generation/preparation failure,
11 bid rejection, 12 not routed.

`merged` is pre-hook presence; `mapped` is post-processed-hook presence (including
paramfanout); `adapter` is rebuilt bidder-specific input after privacy and hooks.
`configured` never means generated or sent. `http_budget_ms` is the remaining
context deadline at HTTP execution; zero can also mean no context deadline.

`auction_bids` counts the response after auction-response hooks, matching the
activity logger's phase. `final_bids` counts the exitpoint object passed to the
encoder. Require `final_known`, `final_reached` and `encoded` before treating it
as a known final response. Successful encoding does not prove client receipt or
rendering. Arbitrary exitpoint replacement types are explicitly unclassified.

A client execution without a successful write event is not a confirmed network
send. Writes without partner response are not acceptance. Explicit no-ad proves
no-fill for observed calls, not its demand/activation cause. Parsed/validated bids
lost before the final response prove downstream loss; an exact floor/currency/
hook reason remains unknown unless an observed stage identifies it. Not-routed
is generic and must not be relabeled as privacy rejection without more evidence.

## Verification and removal

Tests use the real Imprezia adapter, local/fake HTTP transports and response hooks.
They cover every required field, no-fill, HTTP rejection, malformed response,
deadline/transport failure, valid bid, validation/hook removal, successful wire
write, unrelated bidder exclusion, response replacement/unknown types, expiry,
quota, concurrency, queue saturation, sink failure, permissions and data exclusion.
Enabled/disabled calls produce the same bids/error codes and outbound call counts.
Activitylog and paramfanout implementation and reporting definitions are unchanged.

After evidence is accepted, remove this package and its narrow call sites. This is
temporary diagnostics, not a bid-path fix. Production activation requires its own
step approval and second-region rollout requires separate approval.

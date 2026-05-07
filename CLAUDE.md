# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

**Run all tests:**
```bash
./validate.sh
```

**Run a single adapter's tests:**
```bash
go test github.com/prebid/prebid-server/v4/adapters/<adapter> -bench=.
# or via make:
make test adapter=appnexus
```

**Run any specific package tests:**
```bash
go test github.com/prebid/prebid-server/v4/exchange
```

**Run with race condition detection (only tests named `TestRace.*`):**
```bash
./validate.sh --race 5
```

**Check coverage:**
```bash
./scripts/coverage.sh --html
```

**Format code:**
```bash
./scripts/format.sh -f true
# or:
make format
```

**Build:**
```bash
go build .
```

**Regenerate modules builder (after adding a new module):**
```bash
go generate modules/modules.go
```

**Run the server:**
```bash
go run .
```
Server starts on port 8000 by default. Config is in `pbs.yaml`. GDPR `default_value` must be set before running.

## Architecture

### High-level request flow

1. **`router/`** — wires HTTP routes to handler functions
2. **`endpoints/openrtb2/`** — parses and validates incoming OpenRTB2 auction/AMP/video requests
3. **`exchange/`** — runs the auction: calls adapters in parallel, applies floors/GDPR/privacy/currency conversion, selects winning bids
4. **`adapters/<bidder>/`** — translates OpenRTB2 to each SSP's HTTP API and back

### Adding a new bidder adapter

Each adapter requires exactly these files, following the pattern of existing ones:

| File | Purpose |
|---|---|
| `adapters/<bidder>/<bidder>.go` | Implements `adapters.Bidder` interface: `MakeRequests` and `MakeBids` |
| `adapters/<bidder>/<bidder>_test.go` | Calls `adapterstest.RunJSONBidderTest` plus any unit tests |
| `adapters/<bidder>/<bidder>test/exemplary/*.json` | Golden-file test cases showing ideal request/response pairs |
| `adapters/<bidder>/<bidder>test/supplemental/*.json` | Edge-case test scenarios |
| `static/bidder-info/<bidder>.yaml` | Bidder metadata: endpoint, media types, capabilities, user sync |
| `static/bidder-params/<bidder>.json` | JSON Schema for `imp.ext.bidder` params |
| `openrtb_ext/imp_<bidder>.go` | Go struct matching the bidder-params schema |

After creating these files, register the bidder in two places:
- `openrtb_ext/bidders.go` — add a `BidderXxx` constant to `coreBidderNames`
- `exchange/adapter_builders.go` — add an entry to `newAdapterBuilders()` map

### Key packages

- **`exchange/`** — auction engine. `exchange.go` contains `HoldAuction`. `adapter_builders.go` maps every bidder name to its `Builder` function.
- **`openrtb_ext/`** — OpenRTB extensions and bidder name registry. `bidders.go` is the canonical list of all supported bidders.
- **`config/`** — `Configuration` struct (loaded via Viper from `pbs.yaml` or env vars). Bidder-specific config lives under `cfg.Adapters[bidderName]`.
- **`static/bidder-info/`** — YAML files drive which media types/platforms each bidder supports; read at startup.
- **`static/bidder-params/`** — JSON Schema files validated against `imp.ext.bidder` at request time.
- **`hooks/`** — plugin stage system; `hookstage/` defines stages, `hookexecution/` runs them, `hooks/plan.go` sequences hook groups.
- **`modules/`** — long-lived hook implementations. `modules/modules.go` uses `//go:generate` to build `builder.go`, which wires all modules.
- **`endpoints/`** — HTTP handlers. `endpoints/openrtb2/auction.go` handles `/openrtb2/auction`; `endpoints/openrtb2/amp_auction.go` handles `/openrtb2/amp`.
- **`stored_requests/`** — fetches stored request configs from database/filesystem/cache.
- **`usersync/`** — cookie sync logic for `setuid` and `cookie_sync` endpoints.
- **`metrics/`** — Prometheus/InfluxDB metrics abstraction layer.

### Test data format

`adapterstest.RunJSONBidderTest` reads JSON files from `adapters/<bidder>/<bidder>test/`. Each file has this shape:
```json
{
  "mockBidRequest": { ... },
  "expectedMakeRequestsErrors": [],
  "expectedBidResponses": [
    {
      "currency": "USD",
      "httpCalls": [{ "expectedRequest": {...}, "mockResponse": {...} }],
      "expectedBids": [{ ... }]
    }
  ]
}
```

### Module system

Modules implement one or more hook interfaces from `hooks/hookstage/` and are registered via `modules/generator/buildergen.go` code generation. After adding a module directory under `modules/<vendor>/<module>/`, run `go generate modules/modules.go` to update `modules/builder.go`.

### Configuration

Prebid Server uses Viper; config keys map 1:1 to `pbs.yaml` keys or `PBS_*` environment variables (uppercase, underscores). The `config.Configuration` struct in `config/config.go` is the source of truth for all available settings.

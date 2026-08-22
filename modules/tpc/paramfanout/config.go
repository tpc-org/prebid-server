package paramfanout

import (
	"encoding/json"
	"fmt"

	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

// config is the module's startup config, read once from pbs.yaml's
// hooks.modules.tpc.paramfanout block — same free-form-JSON convention
// already used for profanityfilter/activitylog. There is no per-request
// or per-account config today; Enabled is the only field.
type config struct {
	Enabled bool `json:"enabled"`
}

func newConfig(data json.RawMessage) (config, error) {
	var cfg config
	if len(data) == 0 {
		return cfg, nil
	}
	if err := jsonutil.UnmarshalValid(data, &cfg); err != nil {
		return cfg, fmt.Errorf("paramfanout: failed to parse config: %s", err)
	}
	return cfg, nil
}

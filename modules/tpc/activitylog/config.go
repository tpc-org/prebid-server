package activitylog

import (
	"encoding/json"
	"fmt"

	"github.com/prebid/prebid-server/v4/util/jsonutil"
)

// config is the module's startup config, read once from pbs.yaml's
// hooks.modules.tpc.activitylog block — same free-form-JSON convention
// already used for profanityfilter and adapters.thrad.extra_info.
//
// LogDir is the directory both output files are written into (the same
// host-mounted directory the old stock analytics/filesystem module used
// — see pbs-settings/deploy.sh's analytics-logs bind mount). DebugFlagFile
// is checked (existence + mtime) on every auction response to decide
// whether to also write the sensitive debug log for that request — see
// module.go's isDebugActive. DebugWindowHours is how recently that flag
// file must have been touched to count as active; 0 means "unset", which
// newConfig defaults to 4.
type config struct {
	Enabled          bool   `json:"enabled"`
	LogDir           string `json:"log_dir"`
	DebugFlagFile    string `json:"debug_flag_file"`
	DebugWindowHours int    `json:"debug_window_hours"`
}

const defaultDebugWindowHours = 4

func newConfig(data json.RawMessage) (config, error) {
	var cfg config
	if len(data) == 0 {
		return cfg, nil
	}
	if err := jsonutil.UnmarshalValid(data, &cfg); err != nil {
		return cfg, fmt.Errorf("activitylog: failed to parse config: %s", err)
	}
	if cfg.DebugWindowHours == 0 {
		cfg.DebugWindowHours = defaultDebugWindowHours
	}
	return cfg, nil
}

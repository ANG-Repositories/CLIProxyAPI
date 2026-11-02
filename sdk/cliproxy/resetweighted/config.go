package resetweighted

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode is the plugin routing mode (plugins.configs.reset-weighted-scheduler.mode).
const (
	ModeOff     = "off"     // plugin declines every pick (Handled:false); no polling.
	ModeShadow  = "shadow"  // score + log the would-be pick; routing unchanged (Handled:false).
	ModeEnabled = "enabled" // plugin picks.
)

// yamlConfig is the YAML under plugins.configs.reset-weighted-scheduler.
type yamlConfig struct {
	Mode string `yaml:"mode"`

	WeightReclaim      *float64 `yaml:"weight_reclaim"`
	BalanceK           *float64 `yaml:"balance_k"`
	BaseFloor          *float64 `yaml:"base_floor"`
	HorizonFraction    *float64 `yaml:"horizon_fraction"`
	ShortGuardPct      *float64 `yaml:"short_guard_pct"`
	SnapshotMaxAgeS    *float64 `yaml:"snapshot_max_age_s"`
	ScoreEps           *float64 `yaml:"score_eps"`
	FableShare         *float64 `yaml:"fable_share"`
	FableReserveMargin *float64 `yaml:"fable_reserve_margin_pct"`
	FableReserveMode   string   `yaml:"fable_reserve_mode"`
	FableModelPrefixes []string `yaml:"fable_model_prefixes"`

	PollIntervalS *float64          `yaml:"poll_interval_s"`
	UsageAceURL   string            `yaml:"usage_ace_url"`
	ClaudeKeyMap  map[string]string `yaml:"claude_key_map"` // auth id or email -> usage.ace key
	AffinityPath  string            `yaml:"affinity_path"`
	AffinityMax   *int              `yaml:"affinity_max"`
	SessionHeader string            `yaml:"session_header"`
	Providers     []string          `yaml:"providers"` // providers the plugin scores; others -> Handled:false
	// Credit steering (credit.go): the shared rules file, the fleet collector's credits file, and the weight.
	CreditRulesPath     string   `yaml:"credit_rules_path"`
	CreditsPath         string   `yaml:"credits_path"`
	CreditUrgencyWeight *float64 `yaml:"credit_urgency_weight"`
	DisablePolling      bool     `yaml:"disable_polling"`
	// QuotaSeed is a bench aid: static quota per credential matcher (auth id,
	// label, name or email substring) applied at configure time and on every
	// poll before live data. Live polling overwrites matched entries.
	QuotaSeed map[string]quotaSeed `yaml:"quota_seed"`
}

type quotaSeed struct {
	LongUsedPct  float64  `yaml:"long_used_pct"`
	LongResetsIn string   `yaml:"long_resets_in"` // Go duration, e.g. 7h
	LongSpan     string   `yaml:"long_span"`      // Go duration, default 168h
	ShortUsedPct *float64 `yaml:"short_used_pct"`
	FableUsedPct *float64 `yaml:"fable_used_pct"`
}

// QuotaSeedEntry is the decoded static quota for one credential matcher.
type QuotaSeedEntry struct {
	Match string
	Quota Quota
}

type RuntimeConfig struct {
	Mode          string
	Score         ScoreConfig
	PollInterval  time.Duration
	UsageAceURL   string
	ClaudeKeyMap  map[string]string
	AffinityPath  string
	AffinityMax   int
	SessionHeader string
	Providers     map[string]bool
	Polling       bool
	QuotaSeeds    []QuotaSeedEntry
}

const (
	DefaultSessionHeader = "X-Rws-Session"
	MinPollInterval      = 5 * time.Minute
	defaultUsageAceURL   = "https://usage.ace/usage.json"
)

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		Mode:          ModeShadow,
		Score:         DefaultScoreConfig(),
		PollInterval:  MinPollInterval,
		UsageAceURL:   defaultUsageAceURL,
		AffinityMax:   10000,
		SessionHeader: DefaultSessionHeader,
		Providers:     map[string]bool{"codex": true, "claude": true, "xai": true, "kimi": true, "antigravity": true},
		Polling:       true,
	}
}

func DecodeRuntimeConfig(raw []byte) (RuntimeConfig, error) {
	rc := DefaultRuntimeConfig()
	if len(raw) == 0 {
		return rc, nil
	}
	var pc yamlConfig
	if errUnmarshal := yaml.Unmarshal(raw, &pc); errUnmarshal != nil {
		return rc, errUnmarshal
	}
	switch strings.ToLower(strings.TrimSpace(pc.Mode)) {
	case "", ModeShadow:
		rc.Mode = ModeShadow
	case ModeEnabled, "enable", "on", "true":
		rc.Mode = ModeEnabled
	case ModeOff, "disabled", "false":
		rc.Mode = ModeOff
	default:
		rc.Mode = ModeShadow // unknown value: never silently arm
	}
	setF := func(dst *float64, src *float64) {
		if src != nil && *src == *src {
			*dst = *src
		}
	}
	setF(&rc.Score.WeightReclaim, pc.WeightReclaim)
	setF(&rc.Score.BalanceK, pc.BalanceK)
	setF(&rc.Score.BaseFloor, pc.BaseFloor)
	setF(&rc.Score.HorizonFraction, pc.HorizonFraction)
	setF(&rc.Score.ShortGuardPct, pc.ShortGuardPct)
	setF(&rc.Score.ScoreEps, pc.ScoreEps)
	setF(&rc.Score.FableShare, pc.FableShare)
	setF(&rc.Score.FableReserveMargin, pc.FableReserveMargin)
	if pc.SnapshotMaxAgeS != nil && *pc.SnapshotMaxAgeS >= 0 {
		rc.Score.SnapshotMaxAge = time.Duration(*pc.SnapshotMaxAgeS * float64(time.Second))
	}
	switch strings.ToLower(strings.TrimSpace(pc.FableReserveMode)) {
	case "off", "shadow", "enforce":
		rc.Score.FableReserveMode = strings.ToLower(strings.TrimSpace(pc.FableReserveMode))
	}
	if len(pc.FableModelPrefixes) > 0 {
		rc.Score.FableModelPrefixes = pc.FableModelPrefixes
	}
	if pc.PollIntervalS != nil {
		d := time.Duration(*pc.PollIntervalS * float64(time.Second))
		if d < MinPollInterval {
			d = MinPollInterval // vendor rate limits: never below 5 minutes
		}
		rc.PollInterval = d
	}
	if u := strings.TrimSpace(pc.UsageAceURL); u != "" {
		rc.UsageAceURL = u
	}
	if len(pc.ClaudeKeyMap) > 0 {
		rc.ClaudeKeyMap = pc.ClaudeKeyMap
	}
	rc.AffinityPath = strings.TrimSpace(pc.AffinityPath)
	if pc.AffinityMax != nil && *pc.AffinityMax > 0 {
		rc.AffinityMax = *pc.AffinityMax
	}
	if h := strings.TrimSpace(pc.SessionHeader); h != "" {
		rc.SessionHeader = h
	}
	rc.Score.CreditRules = LoadCreditRules(pc.CreditRulesPath)
	rc.Score.CreditWeight = rc.Score.CreditRules.WeightDefault
	if pc.CreditUrgencyWeight != nil && *pc.CreditUrgencyWeight == *pc.CreditUrgencyWeight && *pc.CreditUrgencyWeight >= 0 {
		rc.Score.CreditWeight = *pc.CreditUrgencyWeight
	}
	if p := strings.TrimSpace(pc.CreditsPath); p != "" {
		rc.Score.Credits = NewCreditStore(p)
	}
	if len(pc.Providers) > 0 {
		rc.Providers = make(map[string]bool, len(pc.Providers))
		for _, p := range pc.Providers {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				rc.Providers[p] = true
			}
		}
	}
	rc.Polling = !pc.DisablePolling && rc.Mode != ModeOff
	for match, seed := range pc.QuotaSeed {
		q := Quota{Source: "quota_seed"}
		span := spanWeek
		if d, err := time.ParseDuration(strings.TrimSpace(seed.LongSpan)); err == nil && d > 0 {
			span = d
		}
		q.Long = Window{Used: seed.LongUsedPct, Known: true, Span: span, Rejected: seed.LongUsedPct >= 100}
		if d, err := time.ParseDuration(strings.TrimSpace(seed.LongResetsIn)); err == nil && d > 0 {
			q.Long.ResetsAt = time.Now().Add(d)
		}
		if seed.ShortUsedPct != nil {
			q.Short = Window{Used: *seed.ShortUsedPct, Known: true, Span: span5h, ResetsAt: time.Now().Add(2 * time.Hour), Rejected: *seed.ShortUsedPct >= 100}
		}
		if seed.FableUsedPct != nil {
			q.Fable = Window{Used: *seed.FableUsedPct, Known: true, Span: span, ResetsAt: q.Long.ResetsAt}
		}
		rc.QuotaSeeds = append(rc.QuotaSeeds, QuotaSeedEntry{Match: strings.TrimSpace(match), Quota: q})
	}
	return rc, nil
}

// PluginName is the plugin id (file name stem and plugins.configs key).
const PluginName = "reset-weighted-scheduler"

// Capabilities is the registration capability map. scheduler_across_priorities
// asks the host for every available tier so reclaim can pull a lower-priority
// seat forward for the reset reason (the relay's "mac climbs only for reset").
func Capabilities() map[string]any {
	return map[string]any{
		"scheduler":                   true,
		"scheduler_across_priorities": true,
		"request_interceptor":         true,
		"usage_plugin":                true,
		"management_api":              true,
	}
}

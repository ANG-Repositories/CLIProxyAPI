package resetweighted

// credit.go: the reset-credit steering term, a line-for-line port of the Claude
// relay's claude_pool_lib.credit_urgency_detail (ANG-Ventures/claude-pool #243,
// #246). ONE rule for both runtimes; the ruled numbers come from the shared
// rules file (~/.hermes/config/reset-credit-steering.json) and the golden
// vectors (reset-credit-steering.vectors.json) pin every reason string:
//
//   credit_urgency = 1 - expires_in_h / steer_window_h, clamped [0, 1]
//
// fail-closed to 0 with a named reason when the weight is off, the credit state
// is older than state_max_age_h, the credential holds no usable credit, the
// expiry is unknown or past, the natural reset is unknown, or the natural reset
// is closer than natural_reset_floor_h (a credit that cannot legally be spent must
// not attract load). Spec: plans/2026-11-02_cpa-openai-credit-steering-parity-SPEC.md.

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// CreditRules are the ruled numbers. Defaults equal the relay's literals.
type CreditRules struct {
	SteerWindowH       float64 `json:"steer_window_h"`
	NaturalResetFloorH float64 `json:"natural_reset_floor_h"`
	StateMaxAgeH       float64 `json:"state_max_age_h"`
	WeightDefault      float64 `json:"weight_default"`
}

// DefaultCreditRules mirror claude_pool_lib: 14 d window, 72 h floor, 3 h state age, weight 1.
func DefaultCreditRules() CreditRules {
	return CreditRules{SteerWindowH: 14 * 24, NaturalResetFloorH: 72, StateMaxAgeH: 3, WeightDefault: 1.0}
}

// LoadCreditRules reads the shared rules file; a missing or malformed file yields the defaults.
func LoadCreditRules(path string) CreditRules {
	rules := DefaultCreditRules()
	path = strings.TrimSpace(path)
	if path == "" {
		return rules
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return rules
	}
	var fromFile CreditRules
	if json.Unmarshal(raw, &fromFile) != nil {
		return rules
	}
	if fromFile.SteerWindowH > 0 {
		rules.SteerWindowH = fromFile.SteerWindowH
	}
	if fromFile.NaturalResetFloorH >= 0 && gjson.GetBytes(raw, "natural_reset_floor_h").Exists() {
		rules.NaturalResetFloorH = fromFile.NaturalResetFloorH
	}
	if fromFile.StateMaxAgeH > 0 {
		rules.StateMaxAgeH = fromFile.StateMaxAgeH
	}
	if gjson.GetBytes(raw, "weight_default").Exists() && fromFile.WeightDefault >= 0 {
		rules.WeightDefault = fromFile.WeightDefault
	}
	return rules
}

// Credit is one credential's banked reset credit as the fleet collector reports it.
type Credit struct {
	Usable    int
	ExpiresAt time.Time // zero = unknown
	Known     bool      // a row exists for this credential
}

// CreditDetail is the decision breakdown, same vocabulary as the relay's /health.
type CreditDetail struct {
	CreditUrgency float64
	Reason        string
	ExpiresInH    float64 // NaN when unknown
}

// CreditUrgency is the rule. hrsToReset < 0 or NaN means unknown. stateAge is
// the age of the credits read; a negative age means no read at all.
func CreditUrgency(rules CreditRules, credit Credit, hrsToReset float64, now time.Time, stateAge time.Duration, weight float64) CreditDetail {
	out := CreditDetail{Reason: "ok", ExpiresInH: math.NaN()}
	if weight != weight || weight <= 0 {
		out.Reason = "weight_off"
		return out
	}
	if stateAge < 0 || stateAge > time.Duration(rules.StateMaxAgeH*float64(time.Hour)) {
		out.Reason = "credit_state_stale"
		return out
	}
	if !credit.Known || credit.Usable < 1 {
		out.Reason = "no_credit"
		return out
	}
	if credit.ExpiresAt.IsZero() {
		out.Reason = "expiry_unknown"
		return out
	}
	expH := credit.ExpiresAt.Sub(now).Hours()
	out.ExpiresInH = math.Round(expH*100) / 100
	if expH <= 0 {
		out.Reason = "expired"
		return out
	}
	if hrsToReset != hrsToReset || hrsToReset < 0 {
		out.Reason = "natural_reset_unknown"
		return out
	}
	if hrsToReset < rules.NaturalResetFloorH {
		out.Reason = "natural_reset_lt_72h"
		return out
	}
	u := clamp01(1.0 - expH/rules.SteerWindowH)
	out.CreditUrgency = u
	if u <= 0 {
		out.Reason = "expiry_beyond_window"
	}
	return out
}

// CreditStore reads the fleet collector's state/reset-credits.json (mtime-cached) and
// answers per-account credits. The file shape: vendors.<vendor>.<account_id>.{count, usable, expires_at, error}.
type CreditStore struct {
	path string

	mu       sync.Mutex
	loadedAt time.Time
	mtime    time.Time
	genAt    time.Time // the collector's generated_at
	byKey    map[string]Credit
}

// NewCreditStore returns a store over path; an empty path is a store that never knows a credit.
func NewCreditStore(path string) *CreditStore {
	return &CreditStore{path: strings.TrimSpace(path), byKey: map[string]Credit{}}
}

// refresh re-reads the file when its mtime moved (cheap stat otherwise).
func (s *CreditStore) refresh(now time.Time) {
	if s.path == "" {
		return
	}
	st, err := os.Stat(s.path)
	if err != nil {
		return
	}
	if !s.loadedAt.IsZero() && st.ModTime().Equal(s.mtime) {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	next := map[string]Credit{}
	gjson.GetBytes(raw, "vendors").ForEach(func(vendor, accts gjson.Result) bool {
		accts.ForEach(func(key, row gjson.Result) bool {
			if row.Get("error").Exists() && row.Get("error").Type != gjson.Null && strings.TrimSpace(row.Get("error").String()) != "" {
				return true
			}
			c := Credit{Known: true}
			if u := row.Get("usable"); u.Exists() && u.Type == gjson.Number {
				c.Usable = int(u.Int())
			} else if n := row.Get("count"); n.Exists() && n.Type == gjson.Number {
				c.Usable = int(n.Int()) // OpenAI rows carry count only (usable null); count is the usable number there
			}
			c.ExpiresAt = parseTime(row.Get("expires_at"))
			next[vendor.String()+":"+key.String()] = c
			return true
		})
		return true
	})
	s.byKey = next
	s.mtime = st.ModTime()
	s.loadedAt = now
	s.genAt = parseTime(gjson.GetBytes(raw, "generated_at"))
}

// Lookup returns the credit for vendor:accountID and the age of the collector read
// (negative when there was no read).
func (s *CreditStore) Lookup(vendor, accountID string, now time.Time) (Credit, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(now)
	if s.loadedAt.IsZero() {
		return Credit{}, -1
	}
	age := now.Sub(s.genAt)
	if s.genAt.IsZero() {
		age = now.Sub(s.mtime)
	}
	c, ok := s.byKey[vendor+":"+strings.TrimSpace(accountID)]
	if !ok {
		return Credit{}, age
	}
	return c, age
}

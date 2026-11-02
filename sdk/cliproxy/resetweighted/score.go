// Package resetweighted is the routing brain of the reset-weighted account scheduler plugin.
//
// score.go is the pure routing brain, ported from the Claude Relay Pool's
// claude_pool_lib.py (Router.rank / reclaim / fable_reserve_state / AffinityMap)
// and parameterized by the quota window span so one formula serves the Codex,
// xAI and Antigravity weekly windows and the Kimi 30-day window.
//
// Rules (Ace, 2026-07-05 + 2026-10-03):
//   - reclaim = long_window_headroom x reset_urgency; the long window ONLY.
//   - urgency ramps over a horizon that scales with the window span (3 days for
//     a 7-day window, 3/7 of the span in general).
//   - the short (5-hour) window is a new-pick guardrail (excluded at >= guard
//     pct when another candidate exists), never a reason to evict a binding.
//   - equal scores tie-break on the most-limiting-window headroom, then host
//     priority, then auth id.
//   - unknown/stale quota -> reclaim 0 (never "empty and safe to slam").
//   - Fable (7d overage-included) bucket is the precious one: a seat whose total
//     weekly headroom is down to its Fable headroom is reserved for Fable work,
//     and a Fable request ranks reserved seats first so each sub's Fable
//     headroom drains before its own weekly reset.
package resetweighted

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Window is one quota window observed for a credential.
type Window struct {
	// Used is the utilization in percent (0..100+). Negative = unknown.
	Used float64
	// ResetsAt is when the window resets. Zero = unknown.
	ResetsAt time.Time
	// Span is the window length. Zero = unknown.
	Span time.Duration
	// Rejected reports a vendor "rejected"/limit_reached flag for the window.
	Rejected bool
	// Known reports whether Used carries a measurement.
	Known bool
}

// Quota is the per-credential snapshot the scorer reads.
type Quota struct {
	// Long is the reclaim window (7-day, or 30-day for Kimi).
	Long Window
	// Short is the burst guardrail window (5-hour) when the vendor has one.
	Short Window
	// Fable is the Claude 7d overage-included (Fable allowance) window when present.
	Fable Window
	// ObservedAt is when the snapshot was captured. Zero = unknown.
	ObservedAt time.Time
	// Source names where the snapshot came from (log only).
	Source string
	// Vendor and AccountID key the credential's banked reset credit in the fleet
	// collector's state file (credit.go); empty = no credit lookup.
	Vendor    string
	AccountID string
}

// ScoreConfig holds the tunables (see README for the YAML keys).
type ScoreConfig struct {
	WeightReclaim      float64
	BalanceK           float64
	BaseFloor          float64
	HorizonFraction    float64 // urgency horizon = span * HorizonFraction (3/7 => 72h on a 7d window)
	ShortGuardPct      float64
	SnapshotMaxAge     time.Duration
	ScoreEps           float64
	FableShare         float64
	FableReserveMargin float64 // percent of the total window
	FableReserveMode   string  // off | shadow | enforce
	FableModelPrefixes []string

	// Credit steering (credit.go): reclaim = headroom x max(reset_urgency, credit_urgency x CreditWeight).
	// CreditWeight 0 = off. Credits nil = no store (credit_urgency 0, reason no_credit).
	CreditRules  CreditRules
	CreditWeight float64
	Credits      *CreditStore
}

// DefaultScoreConfig mirrors ~/.hermes/config/claude-router.json and the relay ROUTER_DEFAULTS.
func DefaultScoreConfig() ScoreConfig {
	return ScoreConfig{
		WeightReclaim:      1.0,
		BalanceK:           0.5,
		BaseFloor:          0.05,
		HorizonFraction:    3.0 / 7.0,
		ShortGuardPct:      85.0,
		SnapshotMaxAge:     15 * time.Minute,
		ScoreEps:           0.02,
		FableShare:         0.5,
		FableReserveMargin: 5.0,
		FableReserveMode:   "enforce",
		FableModelPrefixes: []string{"claude-fable"},
		CreditRules:        DefaultCreditRules(),
		CreditWeight:       1.0,
	}
}

// Candidate is one schedulable credential.
type Candidate struct {
	ID       string
	Provider string
	Priority int
	Quota    *Quota // nil = no quota known
	Inflight int
}

// Scored is a ranked candidate with its decision breakdown.
type Scored struct {
	ID            string
	Score         float64
	Headroom      float64
	Urgency       float64
	Reclaim       float64
	BalanceDamp   float64
	ShortGuard    bool
	FableReserved bool
	Fresh         bool
	Reason        string
	CreditUrgency float64
	CreditReason  string
}

// IsFableModel reports whether the requested model belongs to the Fable family.
func IsFableModel(model string, prefixes []string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if len(prefixes) == 0 {
		prefixes = []string{"claude-fable"}
	}
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix != "" && strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func clamp01(v float64) float64 {
	if v != v { // NaN
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

// headroom returns the unspent fraction of a window; unknown -> 0 (never "empty and safe").
func headroom(w Window) float64 {
	if !w.Known || w.Used < 0 {
		return 0
	}
	if w.Rejected {
		return 0
	}
	return clamp01((100 - w.Used) / 100)
}

// Urgency is 0 at >= horizon before the reset and ramps to 1 at the reset.
// An unknown reset or horizon yields 0 (don't over-drain a guess).
func Urgency(resetsAt, now time.Time, horizon time.Duration) float64 {
	if resetsAt.IsZero() || horizon <= 0 {
		return 0
	}
	remaining := resetsAt.Sub(now)
	if remaining <= 0 {
		// The window reset already; the snapshot predates it, so headroom is unknown-good.
		// Treat as not urgent: the fresh snapshot will carry the truth.
		return 0
	}
	return clamp01(float64(horizon-remaining) / float64(horizon))
}

func horizonFor(cfg ScoreConfig, span time.Duration) time.Duration {
	if span <= 0 {
		span = 7 * 24 * time.Hour
	}
	frac := cfg.HorizonFraction
	if frac <= 0 || frac > 1 {
		frac = 3.0 / 7.0
	}
	return time.Duration(float64(span) * frac)
}

// Fresh reports whether a quota snapshot is usable for reclaim.
func (cfg ScoreConfig) Fresh(q *Quota, now time.Time) bool {
	if q == nil || !q.Long.Known {
		return false
	}
	if cfg.SnapshotMaxAge > 0 && !q.ObservedAt.IsZero() && now.Sub(q.ObservedAt) > cfg.SnapshotMaxAge {
		return false
	}
	return true
}

// Reclaim is headroom x urgency on the long window, 0 when the snapshot is unusable.
// urg is max(reset_urgency, credit_urgency x CreditWeight): the relay's shape (claude_pool_lib #243).
func (cfg ScoreConfig) Reclaim(q *Quota, now time.Time) (hr, urg, reclaim float64) {
	hr, urg, reclaim, _ = cfg.ReclaimDetail(q, now)
	return hr, urg, reclaim
}

// ReclaimDetail is Reclaim plus the credit decision (for status and the pick log).
func (cfg ScoreConfig) ReclaimDetail(q *Quota, now time.Time) (hr, urg, reclaim float64, credit CreditDetail) {
	credit = CreditDetail{Reason: "no_credit"}
	if !cfg.Fresh(q, now) {
		return 0, 0, 0, credit
	}
	hr = headroom(q.Long)
	reset := Urgency(q.Long.ResetsAt, now, horizonFor(cfg, q.Long.Span))
	credit = cfg.creditFor(q, now)
	urg = math.Max(reset, credit.CreditUrgency*math.Max(0, cfg.CreditWeight))
	return hr, urg, hr * urg, credit
}

// creditFor evaluates the credit rule for the credential behind q. The natural-reset
// hours come from the same long window the reset urgency reads, so "a credit that
// cannot legally be spent" (reset < floor) is judged on the live quota, never a guess.
func (cfg ScoreConfig) creditFor(q *Quota, now time.Time) CreditDetail {
	if cfg.Credits == nil || q == nil || q.AccountID == "" {
		return CreditDetail{Reason: "no_credit", ExpiresInH: math.NaN()}
	}
	c, age := cfg.Credits.Lookup(q.Vendor, q.AccountID, now)
	hrs := math.NaN()
	if !q.Long.ResetsAt.IsZero() {
		hrs = q.Long.ResetsAt.Sub(now).Hours()
	}
	return CreditUrgency(cfg.CreditRules, c, hrs, now, age, cfg.CreditWeight)
}

// OverShortGuard reports whether the 5-hour (short) window is at/over the guard percentage.
func (cfg ScoreConfig) OverShortGuard(q *Quota) bool {
	if q == nil || !q.Short.Known {
		return false
	}
	return q.Short.Rejected || q.Short.Used >= cfg.ShortGuardPct
}

// Exhausted reports a hard vendor cap on either window that has not reset yet.
func Exhausted(q *Quota, now time.Time) bool {
	if q == nil {
		return false
	}
	for _, w := range []Window{q.Long, q.Short} {
		if w.Rejected && (w.ResetsAt.IsZero() || w.ResetsAt.After(now)) {
			return true
		}
	}
	return false
}

// FableReserved ports fable_reserve_state: reserved iff the Fable row is
// measured inside its own week, has headroom, and the seat's TOTAL weekly
// headroom is down to (Fable headroom + margin). Fractions are of the total window.
func (cfg ScoreConfig) FableReserved(q *Quota, now time.Time) bool {
	if cfg.FableReserveMode == "off" || cfg.FableReserveMode == "" {
		return false
	}
	if q == nil || !q.Long.Known || !q.Fable.Known {
		return false
	}
	if q.Fable.ResetsAt.IsZero() || !q.Fable.ResetsAt.After(now) {
		return false
	}
	share := cfg.FableShare
	if share <= 0 || share > 1 {
		return false
	}
	total := headroom(q.Long)
	if q.Long.Rejected && !q.Long.ResetsAt.IsZero() && !q.Long.ResetsAt.After(now) {
		total = 1 // the week reset since the rejected observation
	}
	fableHeadroom := clamp01((100 - q.Fable.Used) / 100)
	if q.Fable.Rejected {
		fableHeadroom = 0
	}
	units := share * fableHeadroom
	if units <= 1e-12 {
		return false
	}
	margin := math.Max(0, cfg.FableReserveMargin) / 100
	return total <= units+margin+1e-9
}

// limitingHeadroom is the tie-break key: the SMALLEST headroom across known windows.
// Unknown -> 0 so an unknown seat never outranks a measured healthy one.
func limitingHeadroom(q *Quota) float64 {
	if q == nil || !q.Long.Known {
		return 0
	}
	hr := headroom(q.Long)
	if q.Short.Known {
		hr = math.Min(hr, headroom(q.Short))
	}
	return hr
}

// Rank scores the candidates and returns them best-first. fableRequest selects the
// Fable tier ordering (reserved seats first); when false and the mode is enforce,
// reserved seats are WITHHELD from non-Fable work unless they are all that is left.
func Rank(cfg ScoreConfig, cands []Candidate, now time.Time, fableRequest bool) []Scored {
	if len(cands) == 0 {
		return nil
	}
	pool := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if Exhausted(c.Quota, now) {
			continue
		}
		pool = append(pool, c)
	}
	if len(pool) == 0 {
		pool = append(pool, cands...) // INV-1: never wedge; the host already filtered availability
	}

	// 5-hour guardrail: drop over-guard seats unless that empties the pool.
	kept := pool[:0:0]
	for _, c := range pool {
		if !cfg.OverShortGuard(c.Quota) {
			kept = append(kept, c)
		}
	}
	if len(kept) > 0 {
		pool = kept
	}

	// Fable withhold: in enforce mode a non-Fable request may not take a reserved seat
	// while an unreserved one exists.
	reserved := make(map[string]bool, len(pool))
	for _, c := range pool {
		if cfg.FableReserved(c.Quota, now) {
			reserved[c.ID] = true
		}
	}
	if !fableRequest && cfg.FableReserveMode == "enforce" && len(reserved) > 0 {
		free := pool[:0:0]
		for _, c := range pool {
			if !reserved[c.ID] {
				free = append(free, c)
			}
		}
		if len(free) > 0 {
			pool = free
		}
	}

	out := make([]Scored, 0, len(pool))
	for _, c := range pool {
		hr, urg, rc, credit := cfg.ReclaimDetail(c.Quota, now)
		damp := 1.0 / (1.0 + math.Max(0, cfg.BalanceK)*float64(max(0, c.Inflight)))
		score := (cfg.WeightReclaim*rc + cfg.BaseFloor) * damp
		s := Scored{
			ID: c.ID, Score: score, Headroom: hr, Urgency: urg, Reclaim: rc,
			BalanceDamp: damp, ShortGuard: cfg.OverShortGuard(c.Quota),
			FableReserved: reserved[c.ID], Fresh: cfg.Fresh(c.Quota, now),
			CreditUrgency: credit.CreditUrgency, CreditReason: credit.Reason,
		}
		switch {
		case !s.Fresh:
			s.Reason = "unknown_quota"
		case rc > 1e-9 && credit.CreditUrgency*math.Max(0, cfg.CreditWeight) > Urgency(c.Quota.Long.ResetsAt, now, horizonFor(cfg, c.Quota.Long.Span)):
			s.Reason = "credit"
		case rc > 1e-9:
			s.Reason = "reset"
		case damp < 1:
			s.Reason = "balance"
		default:
			s.Reason = "priority"
		}
		out = append(out, s)
	}

	byID := make(map[string]Candidate, len(pool))
	for _, c := range pool {
		byID[c.ID] = c
	}
	eps := cfg.ScoreEps
	bucket := func(s float64) float64 {
		if eps > 0 {
			return math.Round(s / eps)
		}
		return s
	}
	tier := func(s Scored) int {
		if fableRequest && cfg.FableReserveMode == "enforce" && s.FableReserved {
			return 0
		}
		return 1
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ta, tb := tier(a), tier(b); ta != tb {
			return ta < tb
		}
		if ba, bb := bucket(a.Score), bucket(b.Score); ba != bb {
			return ba > bb
		}
		// Most limiting-window headroom wins the tie.
		if ha, hb := limitingHeadroom(byID[a.ID].Quota), limitingHeadroom(byID[b.ID].Quota); ha != hb {
			return ha > hb
		}
		if pa, pb := byID[a.ID].Priority, byID[b.ID].Priority; pa != pb {
			return pa > pb // cpa: higher priority int wins
		}
		return a.ID < b.ID
	})
	return out
}

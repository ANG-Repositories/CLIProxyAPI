package resetweighted

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// Engine is the plugin's in-process state: config, quota cache, affinity, counters.
// Every method is panic-safe for the host: Pick recovers and declines.
type Engine struct {
	mu       sync.RWMutex
	cfg      RuntimeConfig
	quota    map[string]*Quota // by auth id (candidate ID)
	inflight map[string]int
	affinity *AffinityMap
	logFn    func(level, msg string, fields map[string]any)
	now      func() time.Time

	// counters
	picks        atomic.Int64
	shadowPicks  atomic.Int64
	affinityHits atomic.Int64
	declines     atomic.Int64
	differ       atomic.Int64 // shadow: would-be pick != first candidate (host's default order)
	lastPoll     atomic.Int64
	lastPollErr  atomic.Value
}

func NewEngine(cfg RuntimeConfig, logFn func(level, msg string, fields map[string]any)) *Engine {
	if logFn == nil {
		logFn = func(string, string, map[string]any) {}
	}
	e := &Engine{
		cfg:      cfg,
		quota:    make(map[string]*Quota),
		inflight: make(map[string]int),
		affinity: NewAffinityMap(cfg.AffinityMax, cfg.AffinityPath),
		logFn:    logFn,
		now:      time.Now,
	}
	e.lastPollErr.Store("")
	return e
}

// Reconfigure swaps the config; the affinity map is kept unless its path changed.
func (e *Engine) Reconfigure(cfg RuntimeConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cfg.AffinityPath != e.cfg.AffinityPath || cfg.AffinityMax != e.cfg.AffinityMax {
		_ = e.affinity.Flush()
		e.affinity = NewAffinityMap(cfg.AffinityMax, cfg.AffinityPath)
	}
	e.cfg = cfg
}

// ApplyQuotaSeeds stores the config's static quota for every listed host
// credential whose id, label, name or email contains the seed's match string.
func (e *Engine) ApplyQuotaSeeds(entries []pluginapi.HostAuthFileEntry) int {
	cfg := e.config()
	applied := 0
	for _, seed := range cfg.QuotaSeeds {
		if seed.Match == "" {
			continue
		}
		for _, entry := range entries {
			hay := strings.ToLower(entry.ID + " " + entry.Label + " " + entry.Name + " " + entry.Email)
			if strings.Contains(hay, strings.ToLower(seed.Match)) {
				q := seed.Quota
				q.ObservedAt = e.now()
				e.SetQuota(entry.ID, &q)
				applied++
			}
		}
	}
	return applied
}

func (e *Engine) config() RuntimeConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

// SetQuota stores a snapshot for an auth id (poller / tests).
func (e *Engine) SetQuota(authID string, q *Quota) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if q == nil {
		delete(e.quota, authID)
		return
	}
	e.quota[authID] = q
}

func (e *Engine) quotaFor(authID string) *Quota {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.quota[authID]
}

// Session key bridge -------------------------------------------------------

// SessionKeyFromBody derives the conversation id a Claude Code / Codex client
// carries in the body (metadata.user_id session, prompt_cache_key,
// conversation_id). Empty when none is present.
func SessionKeyFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if userID := gjson.GetBytes(body, "metadata.user_id").String(); userID != "" {
		if idx := strings.LastIndex(userID, "_session_"); idx >= 0 && idx+9 < len(userID) {
			return "claude:" + userID[idx+9:]
		}
		if strings.HasPrefix(userID, "{") {
			if sid := gjson.Get(userID, "session_id").String(); sid != "" {
				return "claude:" + sid
			}
		}
		return "user:" + userID
	}
	if key := gjson.GetBytes(body, "prompt_cache_key").String(); key != "" {
		return "pck:" + key
	}
	if conv := gjson.GetBytes(body, "conversation_id").String(); conv != "" {
		return "conv:" + conv
	}
	return ""
}

// SessionKeyFromHeaders reads the id intercept_before injected, else the client
// session headers cpa's own affinity recognizes.
func SessionKeyFromHeaders(headers map[string][]string, injected string) string {
	h := http.Header(headers)
	if h == nil {
		return ""
	}
	if v := strings.TrimSpace(h.Get(injected)); v != "" {
		return v
	}
	if v := strings.TrimSpace(h.Get("X-Session-ID")); v != "" {
		return "header:" + v
	}
	for _, name := range []string{"Session-Id", "Session_id"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return "codex:" + v
		}
	}
	return ""
}

// InterceptBefore computes the header the scheduler reads for affinity.
// Returns nil when the body carries no session id (nothing to inject).
func (e *Engine) InterceptBefore(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	defer func() { _ = recover() }()
	cfg := e.config()
	if cfg.Mode == ModeOff {
		return pluginapi.RequestInterceptResponse{}
	}
	key := SessionKeyFromBody(req.Body)
	if key == "" {
		return pluginapi.RequestInterceptResponse{}
	}
	return pluginapi.RequestInterceptResponse{Headers: http.Header{cfg.SessionHeader: []string{key}}}
}

// Pick ----------------------------------------------------------------------

// Decision is the full pick record (also the JSON log line).
type Decision struct {
	Event       string    `json:"event"`
	Mode        string    `json:"mode"`
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Chosen      string    `json:"chosen,omitempty"`
	Stage       string    `json:"stage"` // affinity|fallback|return_home|hold_fallback|score|decline
	Reason      string    `json:"reason,omitempty"`
	AffinityKey string    `json:"affinity_key,omitempty"`
	Bound       bool      `json:"bound"`
	Home        string    `json:"home,omitempty"`
	HostFirst   string    `json:"host_first,omitempty"` // first candidate in host order (round-robin would pick)
	Differs     bool      `json:"differs"`
	IsFable     bool      `json:"is_fable"`
	Reserved    []string  `json:"reserved_keys,omitempty"`
	Ranked      []Scored  `json:"ranked,omitempty"`
	Candidates  int       `json:"candidates"`
	At          time.Time `json:"at"`
}

// Pick evaluates the request. It NEVER returns an error envelope: an error from
// pick hard-fails the request at the host, so every abnormal path declines
// with Handled:false and the built-in scheduler takes over.
func (e *Engine) Pick(req pluginapi.SchedulerPickRequest) (resp pluginapi.SchedulerPickResponse) {
	defer func() {
		if r := recover(); r != nil {
			e.declines.Add(1)
			e.logFn("warn", "reset-weighted-scheduler: pick panic recovered; declined", map[string]any{"panic": fmt.Sprint(r)})
			resp = pluginapi.SchedulerPickResponse{Handled: false}
		}
	}()
	cfg := e.config()
	if cfg.Mode == ModeOff || len(req.Candidates) == 0 {
		e.declines.Add(1)
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" && len(req.Providers) == 1 {
		provider = strings.ToLower(req.Providers[0])
	}
	// Every candidate must be a provider we score; a mixed set declines.
	for _, c := range req.Candidates {
		p := strings.ToLower(strings.TrimSpace(c.Provider))
		if p == "" {
			p = provider
		}
		if !cfg.Providers[p] {
			e.declines.Add(1)
			return pluginapi.SchedulerPickResponse{Handled: false}
		}
	}

	now := e.now()
	decision := e.decide(cfg, req, provider, now)
	if cfg.Mode == ModeShadow {
		decision.Event = "pick_shadow"
		e.shadowPicks.Add(1)
		if decision.Differs {
			e.differ.Add(1)
		}
		e.emit(decision)
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	if decision.Chosen == "" {
		decision.Event = "pick_decline"
		e.declines.Add(1)
		e.emit(decision)
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	e.picks.Add(1)
	if decision.Bound {
		e.affinityHits.Add(1)
	}
	if decision.AffinityKey != "" {
		e.affinityNote(decision.AffinityKey, decision.Chosen, decision.Stage, now)
	}
	e.emit(decision)
	return pluginapi.SchedulerPickResponse{AuthID: decision.Chosen, Handled: true}
}

func (e *Engine) affinityNote(key, chosen, stage string, now time.Time) {
	e.mu.RLock()
	aff := e.affinity
	e.mu.RUnlock()
	switch stage {
	case "score":
		aff.NoteServed(key, chosen, now)
	}
}

func (e *Engine) decide(cfg RuntimeConfig, req pluginapi.SchedulerPickRequest, provider string, now time.Time) Decision {
	e.mu.RLock()
	aff := e.affinity
	inflight := make(map[string]int, len(e.inflight))
	for k, v := range e.inflight {
		inflight[k] = v
	}
	e.mu.RUnlock()

	fable := IsFableModel(req.Model, cfg.Score.FableModelPrefixes)
	d := Decision{Event: "pick", Mode: cfg.Mode, Provider: provider, Model: req.Model, IsFable: fable, At: now, Candidates: len(req.Candidates)}
	d.HostFirst = req.Candidates[0].ID

	key := SessionKeyFromHeaders(req.Options.Headers, cfg.SessionHeader)
	if key != "" && fable && cfg.Score.FableReserveMode == "enforce" {
		key += "|fable" // D7: a Fable session binds under its own key
	}
	d.AffinityKey = key

	cands := make([]Candidate, 0, len(req.Candidates))
	eligible := make(map[string]bool, len(req.Candidates))
	for _, c := range req.Candidates {
		q := e.quotaFor(c.ID)
		if Exhausted(q, now) {
			continue // a quota-capped seat never takes a pick; a binding to it is dropped below
		}
		eligible[c.ID] = true
		cands = append(cands, Candidate{ID: c.ID, Provider: c.Provider, Priority: c.Priority, Quota: q, Inflight: inflight[c.ID]})
	}
	if len(cands) == 0 {
		d.Stage, d.Reason = "decline", "all_exhausted"
		return d
	}
	for _, c := range cands {
		if cfg.Score.FableReserved(c.Quota, now) {
			d.Reserved = append(d.Reserved, c.ID)
		}
	}
	sort.Strings(d.Reserved)

	// Stage 1: affinity gate. Exhaustion is the ONLY thing that ends a binding.
	if key != "" {
		if home, ok := aff.Peek(key); ok && !eligible[home] {
			if Exhausted(e.quotaFor(home), now) {
				aff.Drop(key)
				e.logFn("info", "reset-weighted-scheduler: affinity dropped", map[string]any{"event": "affinity_dropped", "affinity_key": key, "home": home, "reason": "exhausted_prepick"})
			}
		}
		res := aff.Resolve(key, eligible, now)
		d.Home = res.Home
		if res.Target != "" {
			d.Chosen, d.Stage, d.Reason = res.Target, res.Stage, "pin"
			d.Bound = res.Stage == "affinity" || res.Stage == "return_home"
			d.Differs = d.Chosen != d.HostFirst
			return d
		}
	}

	// Stage 2: score.
	ranked := Rank(cfg.Score, cands, now, fable)
	d.Ranked = ranked
	if len(ranked) == 0 {
		d.Stage, d.Reason = "decline", "no_rank"
		return d
	}
	d.Chosen, d.Stage, d.Reason = ranked[0].ID, "score", ranked[0].Reason
	d.Differs = d.Chosen != d.HostFirst
	return d
}

// emit writes ONE JSON line per selection. The host's log formatter prints only
// an allowlisted field set, so the JSON document IS the message; the structured
// fields ride along for sinks that keep them.
func (e *Engine) emit(d Decision) {
	raw, errMarshal := json.Marshal(d)
	if errMarshal != nil {
		return
	}
	fields := map[string]any{"event": d.Event, "chosen": d.Chosen, "stage": d.Stage, "reason": d.Reason,
		"affinity_key": d.AffinityKey, "bound": d.Bound, "host_first": d.HostFirst, "differs": d.Differs,
		"model": d.Model, "provider": d.Provider, "is_fable": d.IsFable, "decision": string(raw)}
	e.logFn("info", string(raw), fields)
}

// Usage / inflight -------------------------------------------------------------

// HandleUsage closes the loop on a completed request: a 429 on the long window is
// a quota signal that releases the binding so the session rebinds at its next
// pick (rebind-after-cap); a success on a non-home seat is noted as the sticky fallback.
func (e *Engine) HandleUsage(record pluginapi.UsageRecord) {
	defer func() { _ = recover() }()
	if record.AuthID == "" {
		return
	}
	now := e.now()
	e.mu.RLock()
	aff := e.affinity
	e.mu.RUnlock()
	if record.Failed && record.Failure.StatusCode == http.StatusTooManyRequests {
		e.mu.Lock()
		if q := e.quota[record.AuthID]; q != nil {
			q.Long.Rejected = true // conservative until the next poll says otherwise
		}
		e.mu.Unlock()
		released := aff.DropCredential(record.AuthID)
		e.logFn("info", "reset-weighted-scheduler: 429 released bindings", map[string]any{"event": "rebind", "auth_id": record.AuthID, "released": released})
		return
	}
	_ = now
}

// Status ---------------------------------------------------------------------

type credentialStatus struct {
	AuthID        string    `json:"auth_id"`
	Headroom      float64   `json:"headroom"`
	Urgency       float64   `json:"urgency"`
	Reclaim       float64   `json:"reclaim"`
	ShortUsed     float64   `json:"short_used_pct"`
	LongUsed      float64   `json:"long_used_pct"`
	FableUsed     float64   `json:"fable_used_pct,omitempty"`
	FableReserved bool      `json:"fable_reserved"`
	Fresh         bool      `json:"fresh"`
	Exhausted     bool      `json:"exhausted"`
	ResetsAt      time.Time `json:"resets_at,omitempty"`
	ObservedAt    time.Time `json:"observed_at,omitempty"`
	CreditUrgency float64   `json:"credit_urgency"`
	CreditReason  string    `json:"credit_reason,omitempty"`
	CreditExpH    float64   `json:"credit_expires_in_h,omitempty"`
	Source        string    `json:"source,omitempty"`
	Bound         int       `json:"bound_sessions"`
}

// Status renders the per-credential view for the management endpoint.
func (e *Engine) Status() map[string]any {
	cfg := e.config()
	now := e.now()
	e.mu.RLock()
	ids := make([]string, 0, len(e.quota))
	for id := range e.quota {
		ids = append(ids, id)
	}
	aff := e.affinity
	e.mu.RUnlock()
	sort.Strings(ids)
	bound := aff.BoundCounts()
	creds := make([]credentialStatus, 0, len(ids))
	for _, id := range ids {
		q := e.quotaFor(id)
		hr, urg, rc, credit := cfg.Score.ReclaimDetail(q, now)
		cs := credentialStatus{AuthID: id, Headroom: hr, Urgency: urg, Reclaim: rc, Fresh: cfg.Score.Fresh(q, now),
			Exhausted: Exhausted(q, now), FableReserved: cfg.Score.FableReserved(q, now), Bound: bound[id],
			CreditUrgency: credit.CreditUrgency, CreditReason: credit.Reason}
		if credit.ExpiresInH == credit.ExpiresInH {
			cs.CreditExpH = credit.ExpiresInH
		}
		if q != nil {
			cs.LongUsed, cs.ShortUsed, cs.ResetsAt, cs.ObservedAt, cs.Source = q.Long.Used, q.Short.Used, q.Long.ResetsAt, q.ObservedAt, q.Source
			if q.Fable.Known {
				cs.FableUsed = q.Fable.Used
			}
		}
		creds = append(creds, cs)
	}
	lastErr, _ := e.lastPollErr.Load().(string)
	return map[string]any{
		"mode":               cfg.Mode,
		"fable_reserve_mode": cfg.Score.FableReserveMode,
		"picks":              e.picks.Load(),
		"shadow_picks":       e.shadowPicks.Load(),
		"shadow_differs":     e.differ.Load(),
		"affinity_hits":      e.affinityHits.Load(),
		"declines":           e.declines.Load(),
		"affinity_bindings":  aff.Len(),
		"last_poll_at":       time.Unix(e.lastPoll.Load(), 0).UTC(),
		"last_poll_error":    lastErr,
		"credentials":        creds,
	}
}

// Now returns the engine clock (tests inject a fixed clock).
func (e *Engine) Now() time.Time { return e.now() }

// FlushAffinity persists the affinity map when a path is configured.
func (e *Engine) FlushAffinity() error {
	e.mu.RLock()
	aff := e.affinity
	e.mu.RUnlock()
	return aff.Flush()
}

// Config returns the current runtime config.
func (e *Engine) Config() RuntimeConfig { return e.config() }

func (e *Engine) notePoll(at time.Time, errMsg string) {
	e.lastPoll.Store(at.Unix())
	e.lastPollErr.Store(errMsg)
}

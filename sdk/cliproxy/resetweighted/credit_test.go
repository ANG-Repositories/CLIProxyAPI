package resetweighted

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The golden vectors are the ONE definition shared with the Claude relay
// (claude_pool_lib.credit_urgency_detail). A copy ships beside this test so the Go
// suite runs anywhere; the fleet copy is ~/.hermes/config/reset-credit-steering.vectors.json
// and tests/test_credit_steer_vectors.py in hermes-home asserts the two files are byte-equal.
type vectorFile struct {
	Rules CreditRules `json:"rules"`
	Now   string      `json:"now"`
	Cases []struct {
		Name          string          `json:"name"`
		Credit        json.RawMessage `json:"credit"`
		HrsToReset    *float64        `json:"hrs_to_reset"`
		StateAgeH     float64         `json:"state_age_h"`
		Weight        float64         `json:"weight"`
		CreditUrgency float64         `json:"credit_urgency"`
		Reason        string          `json:"reason"`
	} `json:"cases"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "reset-credit-steering.vectors.json"))
	if err != nil {
		t.Fatalf("vectors: %v", err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("vectors json: %v", err)
	}
	return v
}

func creditFromRaw(t *testing.T, raw json.RawMessage) Credit {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return Credit{}
	}
	var row struct {
		Usable    *float64 `json:"usable"`
		ExpiresAt *string  `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("credit row: %v", err)
	}
	c := Credit{Known: true}
	if row.Usable != nil {
		c.Usable = int(*row.Usable)
	}
	if row.ExpiresAt != nil {
		ts, err := time.Parse(time.RFC3339, *row.ExpiresAt)
		if err != nil {
			t.Fatalf("expires_at: %v", err)
		}
		c.ExpiresAt = ts
	}
	return c
}

func TestCreditUrgencyGoldenVectors(t *testing.T) {
	v := loadVectors(t)
	now, err := time.Parse(time.RFC3339, v.Now)
	if err != nil {
		t.Fatalf("now: %v", err)
	}
	for _, c := range v.Cases {
		hrs := math.NaN()
		if c.HrsToReset != nil {
			hrs = *c.HrsToReset
		}
		got := CreditUrgency(v.Rules, creditFromRaw(t, c.Credit), hrs, now, time.Duration(c.StateAgeH*float64(time.Hour)), c.Weight)
		if math.Abs(got.CreditUrgency-c.CreditUrgency) > 1e-5 || got.Reason != c.Reason {
			t.Errorf("%s: got (%.6f, %s) want (%.6f, %s)", c.Name, got.CreditUrgency, got.Reason, c.CreditUrgency, c.Reason)
		}
	}
}

func TestLoadCreditRulesFallsBackToDefaults(t *testing.T) {
	if got := LoadCreditRules(""); got != DefaultCreditRules() {
		t.Fatalf("empty path: %+v", got)
	}
	if got := LoadCreditRules(filepath.Join(t.TempDir(), "missing.json")); got != DefaultCreditRules() {
		t.Fatalf("missing file: %+v", got)
	}
	p := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(p, []byte(`{"steer_window_h": 240, "natural_reset_floor_h": 48}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadCreditRules(p)
	if got.SteerWindowH != 240 || got.NaturalResetFloorH != 48 || got.StateMaxAgeH != 3 || got.WeightDefault != 1 {
		t.Fatalf("partial file: %+v", got)
	}
}

// Two credentials with the SAME reset instant (the post-Tuesday case): only the credit
// term separates them, and the credit holder must win every pick.
func creditStoreWith(t *testing.T, accountID string, expires time.Time, generated time.Time) *CreditStore {
	t.Helper()
	p := filepath.Join(t.TempDir(), "reset-credits.json")
	doc := map[string]any{
		"generated_at": generated.UTC().Format(time.RFC3339),
		"vendors": map[string]any{"openai": map[string]any{
			accountID: map[string]any{"count": 1, "usable": nil, "expires_at": expires.UTC().Format(time.RFC3339), "error": nil},
			"other":   map[string]any{"count": 0, "usable": 0, "expires_at": nil, "error": nil},
		}},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return NewCreditStore(p)
}

func equalResetQuota(now time.Time, used float64, accountID string) *Quota {
	return &Quota{
		Long:       Window{Used: used, Known: true, Span: 7 * 24 * time.Hour, ResetsAt: now.Add(100 * time.Hour)},
		ObservedAt: now, Vendor: "openai", AccountID: accountID,
	}
}

func TestCreditHolderWinsWithEqualResets(t *testing.T) {
	now := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	cfg := DefaultScoreConfig()
	cfg.FableReserveMode = "off"
	cfg.Credits = creditStoreWith(t, "acct-credit", now.Add(48*time.Hour), now.Add(-10*time.Minute))
	// Both reset in 100 h (urgency 0 on a 72 h horizon); the credit holder is even MORE used.
	cands := []Candidate{
		{ID: "a-credit", Provider: "codex", Quota: equalResetQuota(now, 40, "acct-credit")},
		{ID: "b-plain", Provider: "codex", Quota: equalResetQuota(now, 20, "other")},
	}
	for i := 0; i < 20; i++ {
		ranked := Rank(cfg, cands, now, false)
		if ranked[0].ID != "a-credit" {
			t.Fatalf("pick %d: got %s (credit %.3f/%s), want a-credit", i, ranked[0].ID, ranked[0].CreditUrgency, ranked[0].CreditReason)
		}
		if ranked[0].Reason != "credit" || ranked[0].CreditReason != "ok" {
			t.Fatalf("reason: %s / %s", ranked[0].Reason, ranked[0].CreditReason)
		}
		if ranked[1].CreditReason != "no_credit" {
			t.Fatalf("plain seat credit reason: %s", ranked[1].CreditReason)
		}
	}
}

func TestCreditWeightZeroMatchesPreChangeRanking(t *testing.T) {
	now := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	cfg := DefaultScoreConfig()
	cfg.FableReserveMode = "off"
	cfg.Credits = creditStoreWith(t, "acct-credit", now.Add(48*time.Hour), now.Add(-10*time.Minute))
	cands := []Candidate{
		{ID: "a-credit", Provider: "codex", Quota: equalResetQuota(now, 40, "acct-credit")},
		{ID: "b-plain", Provider: "codex", Quota: equalResetQuota(now, 20, "other")},
	}
	cfg.CreditWeight = 0
	off := Rank(cfg, cands, now, false)
	noStore := cfg
	noStore.Credits = nil
	noStore.CreditWeight = 1
	base := Rank(noStore, cands, now, false)
	if len(off) != len(base) {
		t.Fatal("length")
	}
	for i := range off {
		if off[i].ID != base[i].ID || math.Abs(off[i].Score-base[i].Score) > 1e-12 {
			t.Fatalf("weight 0 differs from pre-change at %d: %+v vs %+v", i, off[i], base[i])
		}
		if off[i].CreditReason != "weight_off" {
			t.Fatalf("reason at weight 0: %s", off[i].CreditReason)
		}
	}
}

func TestCreditNeverMakesACredentialEligible(t *testing.T) {
	now := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	cfg := DefaultScoreConfig()
	cfg.FableReserveMode = "off"
	cfg.Credits = creditStoreWith(t, "acct-credit", now.Add(48*time.Hour), now.Add(-10*time.Minute))
	over := equalResetQuota(now, 40, "acct-credit")
	over.Short = Window{Used: 90, Known: true, Span: 5 * time.Hour, ResetsAt: now.Add(time.Hour)} // over the 85 % guardrail
	cands := []Candidate{
		{ID: "a-credit-guarded", Provider: "codex", Quota: over},
		{ID: "b-plain", Provider: "codex", Quota: equalResetQuota(now, 20, "other")},
	}
	ranked := Rank(cfg, cands, now, false)
	if ranked[0].ID != "b-plain" {
		t.Fatalf("guardrail bypassed by credit: %s", ranked[0].ID)
	}
	capped := equalResetQuota(now, 100, "acct-credit")
	capped.Long.Rejected = true
	cands[0].Quota = capped
	ranked = Rank(cfg, cands, now, false)
	if ranked[0].ID != "b-plain" {
		t.Fatalf("exhausted seat picked because of a credit: %s", ranked[0].ID)
	}
}

func TestCreditStoreStaleReadGivesZero(t *testing.T) {
	now := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	cfg := DefaultScoreConfig()
	cfg.FableReserveMode = "off"
	cfg.Credits = creditStoreWith(t, "acct-credit", now.Add(48*time.Hour), now.Add(-4*time.Hour))
	q := equalResetQuota(now, 40, "acct-credit")
	_, _, _, credit := cfg.ReclaimDetail(q, now)
	if credit.CreditUrgency != 0 || credit.Reason != "credit_state_stale" {
		t.Fatalf("stale: %+v", credit)
	}
}

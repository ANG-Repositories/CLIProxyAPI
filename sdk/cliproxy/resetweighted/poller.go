package resetweighted

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// HostCaller performs one host callback (host.auth.list, host.auth.get, host.http.do, host.log).
type HostCaller func(method string, payload []byte) ([]byte, error)

// Poller refreshes the quota cache on a cadence via host callbacks. It never
// polls cloud Claude subs directly (standing rule): Claude quota comes from the
// usage.ace feed, keyed by the fleet sub key.
type Poller struct {
	engine *Engine
	call   HostCaller
	stop   chan struct{}
}

func NewPoller(engine *Engine, call HostCaller) *Poller {
	return &Poller{engine: engine, call: call, stop: make(chan struct{})}
}

// Start runs the loop until Stop. Safe to call once.
func (p *Poller) Start() {
	go func() {
		defer func() { _ = recover() }()
		p.tick()
		for {
			interval := p.engine.Config().PollInterval
			if interval < MinPollInterval {
				interval = MinPollInterval
			}
			select {
			case <-p.stop:
				return
			case <-time.After(interval):
				p.tick()
			}
		}
	}()
}

func (p *Poller) Stop() {
	defer func() { _ = recover() }()
	close(p.stop)
}

func (p *Poller) tick() {
	defer func() {
		if r := recover(); r != nil {
			p.engine.notePoll(p.engine.Now(), fmt.Sprint("panic: ", r))
		}
	}()
	cfg := p.engine.Config()
	if !cfg.Polling {
		return
	}
	errs := p.Refresh(cfg)
	msg := ""
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, err := range errs {
			msgs = append(msgs, err.Error())
		}
		msg = strings.Join(msgs, "; ")
	}
	p.engine.notePoll(p.engine.Now(), msg)
	_ = p.engine.FlushAffinity()
}

type hostAuthEntry = pluginapi.HostAuthFileEntry

func (p *Poller) listAuths() ([]hostAuthEntry, error) {
	raw, errCall := p.call(pluginabi.MethodHostAuthList, []byte(`{}`))
	if errCall != nil {
		return nil, errCall
	}
	var resp struct {
		Files []hostAuthEntry `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return resp.Files, nil
}

// authJSON reads the physical auth file (tokens included) for one credential.
// The token is used for ONE GET against the vendor's usage endpoint and never logged.
func (p *Poller) authJSON(authIndex string) ([]byte, error) {
	payload, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, errCall := p.call(pluginabi.MethodHostAuthGet, payload)
	if errCall != nil {
		return nil, errCall
	}
	var resp struct {
		JSON json.RawMessage `json:"json"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return resp.JSON, nil
}

func (p *Poller) httpGet(url string, headers http.Header) ([]byte, int, error) {
	req := pluginapi.HTTPRequest{Method: http.MethodGet, URL: url, Headers: headers}
	payload, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		return nil, 0, errMarshal
	}
	raw, errCall := p.call(pluginabi.MethodHostHTTPDo, payload)
	if errCall != nil {
		return nil, 0, errCall
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, 0, errUnmarshal
	}
	return resp.Body, resp.StatusCode, nil
}

// Refresh polls every supported credential once. Each failure is isolated: a
// provider whose quota cannot be read keeps its previous snapshot until it ages
// out (SnapshotMaxAge), after which the scorer treats it as unknown (fail-open).
func (p *Poller) Refresh(cfg RuntimeConfig) []error {
	var errs []error
	now := p.engine.Now()
	entries, errList := p.listAuths()
	if errList != nil {
		return []error{fmt.Errorf("host.auth.list: %w", errList)}
	}
	if len(cfg.QuotaSeeds) > 0 {
		p.engine.ApplyQuotaSeeds(entries)
	}
	var claudeEntries []hostAuthEntry
	for _, entry := range entries {
		if entry.Disabled {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if !cfg.Providers[provider] {
			continue
		}
		switch provider {
		case "claude":
			claudeEntries = append(claudeEntries, entry)
			continue
		case "codex", "xai", "kimi", "antigravity":
		default:
			continue
		}
		q, errPoll := p.pollVendor(provider, entry, now)
		if errPoll != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", provider, entry.ID, errPoll))
			continue
		}
		if q != nil {
			p.engine.SetQuota(entry.ID, q)
		}
	}
	if len(claudeEntries) > 0 {
		if errClaude := p.pollUsageAce(cfg, claudeEntries, now); errClaude != nil {
			errs = append(errs, fmt.Errorf("usage.ace: %w", errClaude))
		}
	}
	return errs
}

var errNoToken = errors.New("no access token")

func (p *Poller) pollVendor(provider string, entry hostAuthEntry, now time.Time) (*Quota, error) {
	raw, errAuth := p.authJSON(entry.AuthIndex)
	if errAuth != nil {
		return nil, errAuth
	}
	token := strings.TrimSpace(gjson.GetBytes(raw, "access_token").String())
	if token == "" {
		return nil, errNoToken
	}
	headers := http.Header{"Authorization": []string{"Bearer " + token}, "Accept": []string{"application/json"}}
	var url string
	switch provider {
	case "codex":
		url = "https://chatgpt.com/backend-api/wham/usage"
		headers.Set("User-Agent", "codex-cli")
		if acct := gjson.GetBytes(raw, "account_id").String(); acct != "" {
			headers.Set("ChatGPT-Account-Id", acct)
		}
	case "xai":
		url = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	case "kimi":
		url = "https://api.kimi.com/coding/v1/usages"
		headers.Set("X-Msh-Platform", "kimi_cli")
		if dev := gjson.GetBytes(raw, "device_id").String(); dev != "" {
			headers.Set("X-Msh-Device-Id", dev)
		}
	case "antigravity":
		// The Antigravity quota summary needs the language-server Connect
		// protocol the fleet's gemini-usage-probe drives; cpa reads the probe's
		// published snapshot through usage.ace instead (same feed as Claude).
		return nil, nil
	}
	body, status, errGet := p.httpGet(url, headers)
	if errGet != nil {
		return nil, errGet
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("http %d", status)
	}
	var q *Quota
	var ok bool
	switch provider {
	case "codex":
		q, ok = ParseCodexUsage(body, now)
	case "xai":
		q, ok = ParseXAIBilling(body, now)
	case "kimi":
		q, ok = ParseKimiUsages(body, now)
	}
	if !ok {
		return nil, errors.New("unparseable usage body")
	}
	q.Vendor = vendorKey(provider)
	if provider == "codex" {
		q.AccountID = strings.TrimSpace(gjson.GetBytes(raw, "account_id").String()) // keys state/reset-credits.json vendors.openai.<id>
	}
	return q, nil
}

// vendorKey maps a cpa provider onto the fleet credit collector's vendor name.
func vendorKey(provider string) string {
	switch provider {
	case "codex":
		return "openai"
	case "claude":
		return "anthropic"
	}
	return provider
}

// pollUsageAce maps feed rows onto Claude credentials. Match order: an explicit
// claude_key_map entry (auth id or email -> feed key), else the credential's
// label/name/email containing the feed key (e.g. "sub-vps-6").
func (p *Poller) pollUsageAce(cfg RuntimeConfig, entries []hostAuthEntry, now time.Time) error {
	body, status, errGet := p.httpGet(cfg.UsageAceURL, http.Header{"Accept": []string{"application/json"}})
	if errGet != nil {
		return errGet
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("http %d", status)
	}
	rows := ParseUsageAceClaude(body, now)
	if len(rows) == 0 {
		return errors.New("no claude rows")
	}
	byKey := make(map[string]*Quota, len(rows))
	for _, row := range rows {
		byKey[row.Key] = row.Quota
	}
	for _, entry := range entries {
		key := ""
		for _, candidate := range []string{entry.ID, entry.Email, entry.Name, entry.Label} {
			if mapped, ok := cfg.ClaudeKeyMap[candidate]; ok && candidate != "" {
				key = mapped
				break
			}
		}
		if key == "" {
			haystack := strings.ToLower(entry.Label + " " + entry.Name + " " + entry.Email + " " + entry.ID)
			for feedKey := range byKey {
				if strings.Contains(haystack, strings.ToLower(feedKey)) {
					key = feedKey
					break
				}
			}
		}
		if q, ok := byKey[key]; ok {
			p.engine.SetQuota(entry.ID, q)
		}
	}
	return nil
}

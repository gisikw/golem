package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gisikw/golem/protocol"
)

const tiamatCataloguePath = "/tiamat/v1/models"

// CapabilitySnapshot is the single catalogue used for both advertisement and
// dispatch authorization. Providers contains the Pi provider IDs that may be
// provisioned for the models in Capabilities.
type CapabilitySnapshot struct {
	Capabilities protocol.Capabilities
	PiProviders  map[string]bool
	// TiamatModels maps an advertised dynamic model ID to the exact,
	// credential-free catalogue row that a worker must be provisioned from.
	TiamatModels map[string]protocol.TiamatProvisioning
	Stale        bool
}

// CapabilityResolver resolves the current operator-owned harness offering.
// Implementations may augment configured models, but never add harnesses.
type CapabilityResolver interface {
	Resolve(context.Context) (CapabilitySnapshot, error)
}

type TiamatDiscoveryOptions struct {
	BaseURL          string
	TokenFile        string
	AllowedProviders []string
	AllowedModels    []string
	CacheTTL         time.Duration
	StaleTTL         time.Duration
	Timeout          time.Duration
	MaxModels        int
	MaxResponseBytes int64
	Client           *http.Client
}

// DynamicCapabilityResolver combines immutable operator configuration with a
// bounded, cached Tiamat catalogue. A single in-flight request refreshes the
// cache; callers either receive that result, an explicitly stale snapshot, or
// an error when no cache remains usable.
type DynamicCapabilityResolver struct {
	base             protocol.Capabilities
	providers        map[string]bool
	opts             TiamatDiscoveryOptions
	allowedProviders map[string]bool
	allowedModels    map[string]bool

	mu        sync.Mutex
	cached    *CapabilitySnapshot
	fetched   time.Time
	attempted time.Time
	refresh   chan struct{}
	lastErr   string
}

func NewDynamicCapabilityResolver(base protocol.Capabilities, staticProviders map[string]bool, opts TiamatDiscoveryOptions) (*DynamicCapabilityResolver, error) {
	if _, ok := base.Harnesses[string(protocol.HarnessPi)]; !ok {
		return nil, errors.New("tiamat discovery requires the pi harness")
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	if opts.BaseURL == "" || opts.TokenFile == "" {
		return nil, errors.New("tiamat discovery requires GOLEM_TIAMAT_URL and GOLEM_TIAMAT_TOKEN_FILE")
	}
	if u, err := url.Parse(opts.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("GOLEM_TIAMAT_URL must be an absolute HTTP(S) URL without userinfo, query, or fragment")
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 30 * time.Second
	}
	if opts.StaleTTL <= 0 {
		opts.StaleTTL = 10 * time.Minute
	}
	if opts.StaleTTL < opts.CacheTTL {
		return nil, errors.New("tiamat stale_ttl must be at least cache_ttl")
	}
	if opts.CacheTTL > time.Hour || opts.StaleTTL > 24*time.Hour {
		return nil, errors.New("tiamat cache_ttl must not exceed 1h and stale_ttl must not exceed 24h")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Timeout > 30*time.Second {
		return nil, errors.New("tiamat timeout must not exceed 30s")
	}
	if opts.MaxModels == 0 {
		opts.MaxModels = 1000
	}
	if opts.MaxModels < 1 || opts.MaxModels > 5000 {
		return nil, errors.New("tiamat max_models must be between 1 and 5000")
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 2 << 20
	}
	if opts.MaxResponseBytes > 16<<20 {
		return nil, errors.New("tiamat max_response_bytes must not exceed 16777216")
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	r := &DynamicCapabilityResolver{base: cloneCapabilities(base), providers: cloneBools(staticProviders), opts: opts, allowedProviders: sliceSet(opts.AllowedProviders), allowedModels: sliceSet(opts.AllowedModels)}
	return r, nil
}

func sliceSet(values []string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	m := make(map[string]bool, len(values))
	for _, value := range values {
		m[value] = true
	}
	return m
}

func (r *DynamicCapabilityResolver) Resolve(ctx context.Context) (CapabilitySnapshot, error) {
	for {
		now := time.Now()
		r.mu.Lock()
		if r.cached != nil && now.Sub(r.fetched) <= r.opts.CacheTTL {
			s := cloneSnapshot(*r.cached)
			r.mu.Unlock()
			return s, nil
		}
		if !r.attempted.IsZero() && now.Sub(r.attempted) <= r.opts.CacheTTL {
			s := r.fallbackLocked(now)
			r.mu.Unlock()
			return s, nil
		}
		wait := r.refresh
		if wait == nil {
			wait = make(chan struct{})
			r.refresh = wait
			// Refresh lifetime belongs to the resolver, not whichever HTTP caller
			// happened to miss the cache first.
			go r.refreshCatalogue(wait)
		}
		r.mu.Unlock()
		select {
		case <-wait:
			continue
		case <-ctx.Done():
			return CapabilitySnapshot{}, ctx.Err()
		}
	}
}

func (r *DynamicCapabilityResolver) refreshCatalogue(wait chan struct{}) {
	snapshot, err := r.fetch()
	r.mu.Lock()
	r.attempted = time.Now()
	if err == nil {
		r.cached, r.fetched, r.lastErr = &snapshot, r.attempted, ""
	} else {
		r.lastErr = err.Error()
	}
	close(wait)
	r.refresh = nil
	r.mu.Unlock()
}

func (r *DynamicCapabilityResolver) fallbackLocked(now time.Time) CapabilitySnapshot {
	if r.cached != nil && now.Sub(r.fetched) <= r.opts.StaleTTL {
		stale := cloneSnapshot(*r.cached)
		stale.Stale = true
		stale.Capabilities.Discovery = map[string]protocol.DiscoveryStatus{"tiamat": {Status: "stale", RefreshedAt: r.fetched, Error: r.lastErr}}
		return stale
	}
	caps := cloneCapabilities(r.base)
	caps.Discovery = map[string]protocol.DiscoveryStatus{"tiamat": {Status: "unavailable", Error: r.lastErr}}
	return CapabilitySnapshot{Capabilities: caps, PiProviders: cloneBools(r.providers)}
}

type tiamatRecord struct {
	Model                 string          `json:"model"`
	API                   string          `json:"api"`
	Provider              string          `json:"provider"`
	Fidelity              string          `json:"fidelity"`
	Availability          string          `json:"availability"`
	ContextWindow         json.RawMessage `json:"context_window,omitempty"`
	MaxOutputTokens       json.RawMessage `json:"max_output_tokens,omitempty"`
	Reasoning             json.RawMessage `json:"reasoning,omitempty"`
	Input                 json.RawMessage `json:"input,omitempty"`
	ThinkingLevelMap      json.RawMessage `json:"thinking_level_map,omitempty"`
	ForceAdaptiveThinking json.RawMessage `json:"force_adaptive_thinking,omitempty"`
}

var tiamatFamilies = map[string]string{
	"/anthropic/v1/messages":      "anthropic",
	"/openai/v1/chat/completions": "openai",
	"/responses/v1/responses":     "responses",
}

func (r *DynamicCapabilityResolver) fetch() (CapabilitySnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.Timeout)
	defer cancel()
	token, err := os.ReadFile(r.opts.TokenFile)
	if err != nil {
		return CapabilitySnapshot{}, errors.New("read tiamat token file")
	}
	secret := strings.TrimSpace(string(token))
	if secret == "" {
		return CapabilitySnapshot{}, errors.New("tiamat token file is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.opts.BaseURL+tiamatCataloguePath, nil)
	if err != nil {
		return CapabilitySnapshot{}, errors.New("build tiamat catalogue request")
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := r.opts.Client.Do(req)
	if err != nil {
		return CapabilitySnapshot{}, errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CapabilitySnapshot{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, r.opts.MaxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return CapabilitySnapshot{}, errors.New("read response")
	}
	if int64(len(body)) > r.opts.MaxResponseBytes {
		return CapabilitySnapshot{}, errors.New("response exceeds configured size limit")
	}
	var records []tiamatRecord
	if err = json.Unmarshal(body, &records); err != nil || records == nil {
		return CapabilitySnapshot{}, errors.New("invalid JSON response")
	}
	// Limit records as well as response bytes: unsupported and unavailable rows
	// must not become an unbounded parsing/iteration loophole.
	if len(records) > r.opts.MaxModels {
		return CapabilitySnapshot{}, errors.New("catalogue exceeds configured model limit")
	}

	models := make(map[string]bool)
	dynamic := make(map[string]protocol.TiamatProvisioning)
	providers := cloneBools(r.providers)
	for _, record := range records {
		family, supported := tiamatFamilies[record.API]
		if !supported {
			continue
		} // Match the worker: new wire families are ignored.
		if !safeIdentifier(record.Model) || !safeHeaderValue(record.Provider) || record.Fidelity == "" || !validAvailability(record.Availability) || !validMetadata(record) {
			return CapabilitySnapshot{}, errors.New("catalogue response has an invalid shape")
		}
		if record.Availability == "unavailable" {
			continue
		}
		if r.allowedProviders != nil && !r.allowedProviders[record.Provider] {
			continue
		}
		provider := "tiamat-" + family + "-" + encodeURIComponent(record.Provider)
		model := provider + "/" + record.Model
		if r.allowedModels != nil && !r.allowedModels[model] {
			continue
		}
		models[model], providers[provider] = true, true
		if _, exists := dynamic[model]; !exists {
			dynamic[model] = protocol.TiamatProvisioning{
				Model: record.Model, API: record.API, Provider: record.Provider,
				Fidelity: record.Fidelity, Availability: record.Availability,
				ContextWindow: decodeInt(record.ContextWindow), MaxOutputTokens: decodeInt(record.MaxOutputTokens),
				Reasoning: decodeBool(record.Reasoning), Input: decodeStrings(record.Input),
				ThinkingLevelMap: decodeThinkingMap(record.ThinkingLevelMap), ForceAdaptiveThinking: decodeBool(record.ForceAdaptiveThinking),
			}
		}
		if len(models) > r.opts.MaxModels {
			return CapabilitySnapshot{}, errors.New("catalogue exceeds configured model limit")
		}
	}

	caps := cloneCapabilities(r.base)
	pi := caps.Harnesses[string(protocol.HarnessPi)]
	for _, model := range pi.Models {
		models[model] = true
	}
	pi.Models = make([]string, 0, len(models))
	for model := range models {
		pi.Models = append(pi.Models, model)
	}
	sort.Strings(pi.Models)
	caps.Harnesses[string(protocol.HarnessPi)] = pi
	now := time.Now().UTC()
	caps.Discovery = map[string]protocol.DiscoveryStatus{"tiamat": {Status: "fresh", RefreshedAt: now}}
	return CapabilitySnapshot{Capabilities: caps, PiProviders: providers, TiamatModels: dynamic}, nil
}

func safeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, b := range []byte(s) {
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// Provider is emitted as x-tiamat-provider by the worker. Reject whitespace,
// controls and DEL here rather than relying on a particular fetch runtime's
// header validation.
func safeHeaderValue(s string) bool {
	if !safeIdentifier(s) {
		return false
	}
	for _, b := range []byte(s) {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

func validAvailability(s string) bool {
	return s == "available" || s == "degraded" || s == "unavailable"
}
func validMetadata(r tiamatRecord) bool {
	if !optionalPositiveInteger(r.ContextWindow) || !optionalPositiveInteger(r.MaxOutputTokens) || !optionalBool(r.Reasoning) || !optionalBool(r.ForceAdaptiveThinking) {
		return false
	}
	if r.Input != nil {
		var inputs []string
		if json.Unmarshal(r.Input, &inputs) != nil || inputs == nil {
			return false
		}
		for _, input := range inputs {
			if input != "text" && input != "image" {
				return false
			}
		}
	}
	if r.ThinkingLevelMap != nil {
		var value map[string]*string
		if json.Unmarshal(r.ThinkingLevelMap, &value) != nil || value == nil {
			return false
		}
		allowed := map[string]bool{"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
		for key := range value {
			if !allowed[key] {
				return false
			}
		}
	}
	return true
}

func optionalPositiveInteger(raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	number, ok := value.(float64)
	return ok && number > 0 && number == float64(int64(number))
}
func decodeInt(raw json.RawMessage) *int64 {
	if raw == nil {
		return nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}
func decodeBool(raw json.RawMessage) *bool {
	if raw == nil {
		return nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}
func decodeStrings(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var value []string
	_ = json.Unmarshal(raw, &value)
	return value
}
func decodeThinkingMap(raw json.RawMessage) map[string]*string {
	if raw == nil {
		return nil
	}
	var value map[string]*string
	_ = json.Unmarshal(raw, &value)
	return value
}

func optionalBool(raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	_, ok := value.(bool)
	return ok
}

// encodeURIComponent mirrors the TypeScript worker's provider-ID mapping.
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.!~*'()", rune(c)) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

func cloneCapabilities(in protocol.Capabilities) protocol.Capabilities {
	out := in
	out.Harnesses = make(map[string]protocol.HarnessCapability, len(in.Harnesses))
	for name, h := range in.Harnesses {
		h.Models = append([]string(nil), h.Models...)
		out.Harnesses[name] = h
	}
	out.Projects = append([]protocol.ProjectCapability(nil), in.Projects...)
	if in.Discovery != nil {
		out.Discovery = make(map[string]protocol.DiscoveryStatus, len(in.Discovery))
		for k, v := range in.Discovery {
			out.Discovery[k] = v
		}
	}
	return out
}
func cloneBools(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func cloneSnapshot(in CapabilitySnapshot) CapabilitySnapshot {
	in.Capabilities = cloneCapabilities(in.Capabilities)
	in.PiProviders = cloneBools(in.PiProviders)
	if in.TiamatModels != nil {
		out := make(map[string]protocol.TiamatProvisioning, len(in.TiamatModels))
		for k, v := range in.TiamatModels {
			out[k] = v
		}
		in.TiamatModels = out
	}
	return in
}

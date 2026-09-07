package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gisikw/golem/protocol"
)

func tiamatTestResolver(t *testing.T, handler http.HandlerFunc, mutate func(*TiamatDiscoveryOptions)) (*DynamicCapabilityResolver, *httptest.Server) {
	t.Helper()
	router := httptest.NewServer(handler)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := protocol.Capabilities{Name: "test", Harnesses: map[string]protocol.HarnessCapability{"pi": {Models: []string{"static/local"}}, "fake": {}}}
	opts := TiamatDiscoveryOptions{BaseURL: router.URL, TokenFile: token, CacheTTL: 15 * time.Millisecond, StaleTTL: time.Second, Timeout: time.Second, MaxModels: 20}
	if mutate != nil {
		mutate(&opts)
	}
	resolver, err := NewDynamicCapabilityResolver(base, map[string]bool{"static": true}, opts)
	if err != nil {
		router.Close()
		t.Fatal(err)
	}
	return resolver, router
}

func modelPresent(s CapabilitySnapshot, model string) bool {
	for _, got := range s.Capabilities.Harnesses["pi"].Models {
		if got == model {
			return true
		}
	}
	return false
}

func TestTiamatNewModelAppearsAndFilteringRestrictions(t *testing.T) {
	var body atomic.Value
	body.Store(`[
		{"model":"old","api":"/responses/v1/responses","provider":"astra/personal","fidelity":"native","availability":"available"},
		{"model":"down","api":"/anthropic/v1/messages","provider":"astra/personal","fidelity":"native","availability":"unavailable"},
		{"model":"audio","api":"/future/audio","provider":"astra/personal","fidelity":"native","availability":"available"},
		{"model":"other","api":"/openai/v1/chat/completions","provider":"other","fidelity":"native","availability":"degraded"}
	]`)
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tiamatCataloguePath || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("bad request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(body.Load().(string)))
	}
	allowed := "tiamat-responses-astra%2Fpersonal/new"
	resolver, router := tiamatTestResolver(t, h, func(o *TiamatDiscoveryOptions) {
		o.AllowedProviders = []string{"astra/personal"}
		o.AllowedModels = []string{allowed}
	})
	defer router.Close()

	first, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if modelPresent(first, "tiamat-responses-astra%2Fpersonal/old") || modelPresent(first, "tiamat-anthropic-astra%2Fpersonal/down") || modelPresent(first, "tiamat-openai-other/other") {
		t.Fatalf("unavailable, unsupported, or restricted model advertised: %#v", first.Capabilities.Harnesses["pi"].Models)
	}
	if !modelPresent(first, "static/local") {
		t.Fatal("static provider model regressed")
	}

	body.Store(`[{"model":"new","api":"/responses/v1/responses","provider":"astra/personal","fidelity":"native","availability":"degraded","input":["text","image"]}]`)
	time.Sleep(20 * time.Millisecond)
	second, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !modelPresent(second, allowed) || !second.PiProviders["tiamat-responses-astra%2Fpersonal"] {
		t.Fatalf("new Astra model/provider absent: %#v", second)
	}
	if second.Capabilities.Discovery["tiamat"].Status != "fresh" {
		t.Fatalf("status %#v", second.Capabilities.Discovery)
	}
}

func TestTiamatResolverStaleOutageAndConcurrentRefresh(t *testing.T) {
	var requests atomic.Int32
	var outage atomic.Bool
	h := func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(30 * time.Millisecond)
		if outage.Load() {
			http.Error(w, "do not expose this", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[{"model":"m","api":"/anthropic/v1/messages","provider":"astra","fidelity":"native","availability":"available"}]`))
	}
	resolver, router := tiamatTestResolver(t, h, func(o *TiamatDiscoveryOptions) {
		o.CacheTTL = 10 * time.Millisecond
		o.StaleTTL = 80 * time.Millisecond
	})
	defer router.Close()

	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := resolver.Resolve(context.Background()); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := requests.Load(); got != 1 {
		t.Fatalf("concurrent cold refresh made %d requests", got)
	}

	time.Sleep(15 * time.Millisecond)
	outage.Store(true)
	stale, err := resolver.Resolve(context.Background())
	if err != nil || !stale.Stale || stale.Capabilities.Discovery["tiamat"].Status != "stale" {
		t.Fatalf("stale fallback: %#v %v", stale, err)
	}
	if stale.Capabilities.Discovery["tiamat"].Error != "HTTP 502" {
		t.Fatalf("unbounded or unclear stale error: %#v", stale.Capabilities.Discovery)
	}
	time.Sleep(85 * time.Millisecond)
	if _, err = resolver.Resolve(context.Background()); err == nil {
		t.Fatal("expired stale catalogue accepted")
	}
}

func TestDynamicResolverSharedByAdvertisementAndDispatch(t *testing.T) {
	resolver, router := tiamatTestResolver(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"model":"gpt-astra","api":"/responses/v1/responses","provider":"astra","fidelity":"native","availability":"available"},
			{"model":"down","api":"/anthropic/v1/messages","provider":"astra","fidelity":"native","availability":"unavailable"},
			{"model":"audio","api":"/future/audio","provider":"astra","fidelity":"native","availability":"available"}
		]`))
	}, nil)
	defer router.Close()
	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base, _ := resolver.Resolve(context.Background())
	server := httptest.NewServer(API{Store: store, Capabilities: base.Capabilities, Resolver: resolver}.Handler())
	defer server.Close()
	res, err := http.Get(server.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps protocol.Capabilities
	if err = json.NewDecoder(res.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	model := "tiamat-responses-astra/gpt-astra"
	if !contains(caps.Harnesses["pi"].Models, model) {
		t.Fatalf("dynamic model not advertised: %#v", caps)
	}
	post := func(model string) int {
		b, _ := json.Marshal(protocol.CreateJob{IdempotencyKey: model, Harness: protocol.HarnessPi, Model: model, CWD: "/tmp", Prompt: "go"})
		r, e := http.Post(server.URL+"/v1/jobs", "application/json", bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		return r.StatusCode
	}
	if got := post(model); got != http.StatusCreated {
		t.Fatalf("advertised dispatch status %d", got)
	}
	for _, rejected := range []string{"tiamat-responses-astra/unknown", "tiamat-anthropic-astra/down", "tiamat-future-astra/audio"} {
		if got := post(rejected); got != http.StatusUnprocessableEntity {
			t.Fatalf("rejected model %q status %d", rejected, got)
		}
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

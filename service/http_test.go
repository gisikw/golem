package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gisikw/golem/protocol"
)

func TestCapabilitiesAndDispatchValidation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	caps := protocol.Capabilities{Name: "local-daemon", Version: "test", Harnesses: map[string]protocol.HarnessCapability{"pi": {Models: []string{"openai/gpt-5.6"}}, "fake": {Models: []string{}}}}
	server := httptest.NewServer(API{Store: store, Capabilities: caps}.Handler())
	defer server.Close()
	res, err := http.Get(server.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.Capabilities
	if err = json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if got.Name != caps.Name || got.Harnesses["pi"].Models[0] != "openai/gpt-5.6" {
		t.Fatalf("unexpected capabilities: %#v", got)
	}

	post := func(harness, model string) int {
		body, _ := json.Marshal(protocol.CreateJob{IdempotencyKey: harness + model, Harness: protocol.HarnessKind(harness), Model: model, CWD: "/tmp", Prompt: "go"})
		response, requestErr := http.Post(server.URL+"/v1/jobs", "application/json", bytes.NewReader(body))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := post("claude", ""); status < 400 || status >= 500 {
		t.Fatalf("unknown harness status %d", status)
	}
	if status := post("pi", "other/model"); status < 400 || status >= 500 {
		t.Fatalf("unknown model status %d", status)
	}
	if status := post("fake", ""); status != http.StatusCreated {
		t.Fatalf("model-less fake status %d", status)
	}
}

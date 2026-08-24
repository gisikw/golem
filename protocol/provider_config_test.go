package protocol

import (
	"encoding/json"
	"testing"
)

func providerModels(apiKey string) json.RawMessage {
	return json.RawMessage(`{"providers":{"demo":{"apiKey":` + apiKey + `,"models":[]}}}`)
}

func TestValidateProviderConfigAcceptsReferences(t *testing.T) {
	for _, ref := range []string{`"!cat -- /run/secrets/key"`, `"$GOLEM_API_KEY"`, `"${GOLEM_API_KEY}"`} {
		pc := &ProviderConfig{Provider: "demo", ModelsJSON: providerModels(ref)}
		if err := ValidateProviderConfig(pc); err != nil {
			t.Errorf("reference %s rejected: %v", ref, err)
		}
	}
}

func TestValidateProviderConfigRejectsPlaintextAndNestedSecrets(t *testing.T) {
	for _, models := range []json.RawMessage{
		providerModels(`"fixture-value"`),
		json.RawMessage(`{"providers":{"demo":{"metadata":{"apiKey":"fixture-value"},"models":[]}}}`),
		json.RawMessage(`{"providers":{"demo":{"credential":{"value":"fixture-value"},"models":[]}}}`),
		json.RawMessage(`{"providers":{"demo":{"apiKey":"$NOT VALID","models":[]}}}`),
	} {
		if err := ValidateProviderConfig(&ProviderConfig{Provider: "demo", ModelsJSON: models}); err == nil {
			t.Errorf("plaintext or invalid secret reference accepted: %s", models)
		}
	}
}

func TestValidateProviderConfigRequiresSingleMatchingProvider(t *testing.T) {
	for _, models := range []json.RawMessage{
		json.RawMessage(`{"providers":{"other":{"apiKey":"$KEY"}}}`),
		json.RawMessage(`{"providers":{"demo":{"apiKey":"$KEY"},"other":{}}}`),
	} {
		if err := ValidateProviderConfig(&ProviderConfig{Provider: "demo", ModelsJSON: models}); err == nil {
			t.Errorf("invalid provider shape accepted: %s", models)
		}
	}
}

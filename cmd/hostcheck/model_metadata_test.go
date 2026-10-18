package main

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// This nested module may import the real host registry without patching CPA.
// The v8.0.29 pin and deployed v8.0.30 have identical model_registry.go blobs:
// 5e900390ae630c880335e83b73dc01a42359f5e9 (projection at lines 1719-1836).
// https://github.com/router-for-me/CLIProxyAPI/blob/v8.0.30/internal/registry/model_registry.go
func TestHostModelMetadataRegistryProjection(t *testing.T) {
	// The registry is process-global: use a test-owned identity, inspect only its
	// model, and unregister it without resetting unrelated registrations.
	const provider = "hostcheck-metadata-projection"
	clientID := provider + "/" + t.Name()
	model := &registry.ModelInfo{
		ID:                         "hostcheck/" + t.Name(),
		Object:                     "model",
		OwnedBy:                    provider,
		DisplayName:                "Metadata projection fixture",
		Name:                       "models/hostcheck-metadata-projection",
		Version:                    "fixture-1",
		Description:                "Synthetic catalog metadata; no inference claim",
		InputTokenLimit:            96000,
		OutputTokenLimit:           8192,
		SupportedGenerationMethods: []string{"generateContent"},
		ContextLength:              100000,
		MaxCompletionTokens:        8192,
		SupportedParameters:        []string{"tools", "reasoning_effort"},
		SupportedInputModalities:   []string{"text", "image", "pdf"},
		SupportedOutputModalities:  []string{"text"},
		Thinking: &registry.ThinkingSupport{
			Min:            1024,
			Max:            4096,
			ZeroAllowed:    true,
			DynamicAllowed: true,
			Levels:         []string{"low", "high", "xhigh", "max"},
		},
		UserDefined: true,
	}
	r := registry.GetGlobalRegistry()
	t.Cleanup(func() { r.UnregisterClient(clientID) })
	r.RegisterClient(clientID, provider, []*registry.ModelInfo{model})

	registered := r.GetModelsForClient(clientID)
	if len(registered) != 1 || !reflect.DeepEqual(registered[0], model) {
		t.Fatalf("registered metadata = %#v, want one unchanged model %#v", registered, model)
	}
	if got := r.GetModelInfo(model.ID, provider); !reflect.DeepEqual(got, model) {
		t.Fatalf("provider metadata = %#v, want %#v", got, model)
	}

	for _, tc := range []struct {
		format       string
		identityKey  string
		identity     string
		want         map[string]any
		knownOmitted []string
	}{
		{
			format:      "openai",
			identityKey: "id",
			identity:    model.ID,
			want: map[string]any{
				"id":                    model.ID,
				"object":                "model",
				"owned_by":              provider,
				"display_name":          model.DisplayName,
				"version":               model.Version,
				"description":           model.Description,
				"context_length":        model.ContextLength,
				"max_completion_tokens": model.MaxCompletionTokens,
				"supported_parameters":  model.SupportedParameters,
			},
			knownOmitted: []string{"thinking", "inputTokenLimit", "outputTokenLimit", "supportedInputModalities", "supportedOutputModalities", "supportedGenerationMethods"},
		},
		{
			format:      "gemini",
			identityKey: "name",
			identity:    model.Name,
			want: map[string]any{
				"name":                       model.Name,
				"version":                    model.Version,
				"displayName":                model.DisplayName,
				"description":                model.Description,
				"inputTokenLimit":            model.InputTokenLimit,
				"outputTokenLimit":           model.OutputTokenLimit,
				"supportedGenerationMethods": model.SupportedGenerationMethods,
				"supportedInputModalities":   model.SupportedInputModalities,
				"supportedOutputModalities":  model.SupportedOutputModalities,
			},
			knownOmitted: []string{"thinking", "context_length", "max_completion_tokens", "supported_parameters"},
		},
	} {
		t.Run(tc.format, func(t *testing.T) {
			var got map[string]any
			for _, candidate := range r.GetAvailableModels(tc.format) {
				if candidate[tc.identityKey] != tc.identity {
					continue
				}
				if got != nil {
					t.Fatalf("duplicate projection for %q", tc.identity)
				}
				got = candidate
			}
			if got == nil {
				t.Fatalf("missing projection for %q", tc.identity)
			}
			// Metadata retained in the registry is not necessarily public on a
			// given API. In particular, neither list exposes reasoning levels.
			for _, key := range tc.knownOmitted {
				if _, exists := got[key]; exists {
					t.Errorf("host projection now includes %q; review the documented API boundary", key)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("projection = %#v, want %#v", got, tc.want)
			}
		})
	}
}

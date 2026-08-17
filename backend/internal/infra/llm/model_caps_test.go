package llm

import "testing"

func TestModelCapsFromCapabilitiesOverridesNameInference(t *testing.T) {
	caps := GetModelCapsFromCapabilities("custom-enterprise-model", `{
		"contextWindow": 64000,
		"maxOutputTokens": 12000
	}`)

	if caps.ContextWindow != 64_000 {
		t.Fatalf("expected context window from capabilities, got %d", caps.ContextWindow)
	}
	if caps.MaxOutputTokens != 12_000 {
		t.Fatalf("expected max output from capabilities, got %d", caps.MaxOutputTokens)
	}
}

func TestEffectiveContextBudgetFromCapabilitiesUsesConfiguredWindow(t *testing.T) {
	got := EffectiveContextBudgetFromCapabilities("custom-enterprise-model", `{
		"context_window_tokens": "64000",
		"max_output_tokens": "12000"
	}`)
	want := 64_000 - 12_000 - autocompactBufferTokens
	if got != want {
		t.Fatalf("expected budget %d, got %d", want, got)
	}
}

func TestModelAllowsInputModalityUsesConfiguredModalities(t *testing.T) {
	tests := []struct {
		name             string
		capabilitiesJSON string
		modality         string
		want             bool
	}{
		{name: "missing config preserves legacy behavior", capabilitiesJSON: `{}`, modality: "image", want: true},
		{name: "text only rejects images", capabilitiesJSON: `{"inputModalities":["text"]}`, modality: "image", want: false},
		{name: "image is supported", capabilitiesJSON: `{"inputModalities":["text","image"]}`, modality: "image", want: true},
		{name: "values are normalized", capabilitiesJSON: `{"inputModalities":[" IMAGE "]}`, modality: "image", want: true},
		{name: "invalid explicit config fails closed", capabilitiesJSON: `{"inputModalities":"text"}`, modality: "image", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ModelAllowsInputModality(test.capabilitiesJSON, test.modality); got != test.want {
				t.Fatalf("ModelAllowsInputModality() = %v, want %v", got, test.want)
			}
		})
	}
}

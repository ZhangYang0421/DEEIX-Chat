package llm

import (
	"encoding/json"
	"strings"
)

// ModelInputModalityImage identifies raw image input in a model capability
// declaration.
const ModelInputModalityImage = "image"

// ModelAllowsInputModality reports whether a model capability declaration
// allows the requested input modality. An omitted inputModalities field keeps
// historical compatibility; an explicitly malformed field is treated as not
// allowing the modality.
func ModelAllowsInputModality(capabilitiesJSON string, modality string) bool {
	requested := strings.ToLower(strings.TrimSpace(modality))
	if requested == "" {
		return false
	}
	raw := strings.TrimSpace(capabilitiesJSON)
	if raw == "" {
		return true
	}
	payload := map[string]interface{}{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return true
	}
	configured, exists := payload["inputModalities"]
	if !exists {
		return true
	}
	items, ok := configured.([]interface{})
	if !ok {
		return false
	}
	for _, item := range items {
		value, ok := item.(string)
		if ok && strings.EqualFold(strings.TrimSpace(value), requested) {
			return true
		}
	}
	return false
}

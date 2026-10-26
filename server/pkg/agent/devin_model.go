package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type devinModelChoice struct {
	Value   string             `json:"value"`
	Name    string             `json:"name"`
	Options []devinModelChoice `json:"options"`
}

type devinModelOption struct {
	ID           string             `json:"id"`
	Category     string             `json:"category"`
	CurrentValue string             `json:"currentValue"`
	Options      []devinModelChoice `json:"options"`
}

func devinSessionModel(raw json.RawMessage) devinModelOption {
	var response struct {
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	if json.Unmarshal(raw, &response) == nil {
		for _, rawOption := range response.ConfigOptions {
			var option devinModelOption
			if json.Unmarshal(rawOption, &option) == nil && (option.Category == "model" || option.ID == "model") {
				return option
			}
		}
	}
	// Older CLI builds expose the same selected-model fact in models.
	return devinModelOption{CurrentValue: extractACPCurrentModelID(raw)}
}

func devinModelAvailable(choices []devinModelChoice, value string) bool {
	for _, choice := range choices {
		if choice.Value == value || devinModelAvailable(choice.Options, value) {
			return true
		}
	}
	return false
}

// confirmDevinModel treats the session's state, not the launch flag or global
// catalog, as authoritative. The CLI can accept --model and silently use its
// configured default when that model is unavailable to the account. Session
// configOptions also let a resumed conversation apply a changed model choice.
func confirmDevinModel(ctx context.Context, request acpRequestFn, sessionID, requested string, raw json.RawMessage) (string, error) {
	option := devinSessionModel(raw)
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == option.CurrentValue {
		return option.CurrentValue, nil
	}
	if option.ID == "" || !devinModelAvailable(option.Options, requested) {
		return "", fmt.Errorf("requested Devin model %q is unavailable or cannot be confirmed (session model %q); choose an available model ID or use the CLI default", requested, option.CurrentValue)
	}
	result, err := request(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  option.ID,
		"value":     requested,
	})
	if err != nil {
		return "", fmt.Errorf("set Devin model %q: %w", requested, err)
	}
	actual := devinSessionModel(result).CurrentValue
	if actual != requested {
		return "", fmt.Errorf("Devin did not confirm requested model %q (session model %q); no prompt was sent", requested, actual)
	}
	return actual, nil
}

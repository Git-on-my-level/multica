package service

import (
	"encoding/json"
	"strings"
)

// codexRejectedModel recognizes the account-specific, deterministic rejection.
// Other 400s, unavailable models and unstructured error text retain their
// ordinary task failure behavior.
func codexRejectedModel(message string) string {
	var event struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(message), &event) != nil ||
		event.Type != "error" || event.Status != 400 ||
		event.Error.Type != "invalid_request_error" || event.Error.Code != "model_not_found" {
		return ""
	}
	model, ok := strings.CutPrefix(event.Error.Message, "The '")
	if !ok {
		return ""
	}
	model, ok = strings.CutSuffix(model, "' model is not supported when using Codex with a ChatGPT account.")
	if !ok {
		return ""
	}
	return model
}

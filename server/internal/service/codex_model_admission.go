package service

import (
	"encoding/json"
	"strings"
)

const codexChatGPTRejection = "' model is not supported when using Codex with a ChatGPT account."

// codexRejectedModel accepts the exact account-specific rejection from the
// provider envelope or its message as extracted by the Codex app-server.
func codexRejectedModel(message string) string {
	message = strings.TrimSpace(message)
	for _, prefix := range []string{"Error running turn:", "API error 400:"} {
		if rest, ok := strings.CutPrefix(message, prefix); ok {
			message = strings.TrimSpace(rest)
		}
	}
	if model := codexRejectedModelID(message); model != "" {
		return model
	}
	if strings.HasPrefix(message, "unexpected status 400 ") {
		_, payload, found := strings.Cut(message, "{")
		if !found {
			return ""
		}
		message = "{" + payload
	}

	var event struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// Codex's HTTP diagnostic can append the request URL and request ID
	// after the response body. Decode the first object at that boundary.
	if json.NewDecoder(strings.NewReader(message)).Decode(&event) != nil ||
		event.Type != "error" || event.Status != 400 ||
		event.Error.Type != "invalid_request_error" || event.Error.Code != "model_not_found" {
		return ""
	}
	return codexRejectedModelID(event.Error.Message)
}

func codexRejectedModelID(message string) string {
	model, ok := strings.CutPrefix(message, "The '")
	if !ok {
		return ""
	}
	model, ok = strings.CutSuffix(model, codexChatGPTRejection)
	if !ok || strings.ContainsAny(model, "'\r\n") {
		return ""
	}
	return model
}

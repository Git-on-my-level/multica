package protocol

// AdmissionCandidate identifies the agent and model seen by a read-only OpenCode
// queue preview. The server checks both again before delivering a claim.
type AdmissionCandidate struct {
	AgentID string `json:"agent_id"`
	Model   string `json:"model"`
}

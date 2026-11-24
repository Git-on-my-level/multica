package protocol

// AdmissionCandidate identifies the agent and model seen by a read-only OpenCode
// queue preview. The server checks both again before delivering a claim.
type AdmissionCandidate struct {
	AgentID string `json:"agent_id"`
	Model   string `json:"model"`
}

// BatchAdmissionCandidate is a single agent permit in the mixed-runtime
// batch. The reclaim ID limits the permit to one lost delivery or new claim.
type BatchAdmissionCandidate struct {
	AdmissionCandidate
	RuntimeID     string `json:"runtime_id"`
	ReclaimTaskID string `json:"reclaim_task_id,omitempty"`
	PromoteDue    bool   `json:"promote_due,omitempty"`
}

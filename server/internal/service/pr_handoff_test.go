package service

import "testing"

func TestClassifyTaskPRHandoff(t *testing.T) {
	allowed := map[string]struct{}{"acme/widget": {}}
	tests := []struct {
		name   string
		output string
		prURL  string
		states []string
	}{
		{
			name:   "one registered canonical candidate awaits mirror",
			output: "Opened https://github.com/acme/widget/pull/42",
			states: []string{"awaiting_mirror"},
		},
		{
			name:   "external candidate fails closed",
			output: "Opened https://github.com/other/widget/pull/42",
			states: []string{"invalid_external_pr"},
		},
		{
			name:   "ambiguous registered candidates need review",
			output: "https://github.com/acme/widget/pull/42 https://github.com/acme/widget/pull/43",
			states: []string{"candidate_detected", "candidate_detected"},
		},
		{
			name:   "malformed and absent candidates record nothing",
			output: "See https://github.com/acme/widget/pull/42/files",
			states: nil,
		},
		{
			name:   "legacy field and output deduplicate idempotently",
			output: "Opened https://github.com/ACME/WIDGET/pull/42",
			prURL:  "https://github.com/acme/widget/pull/42",
			states: []string{"awaiting_mirror"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTaskPRHandoff(tc.output, tc.prURL, allowed)
			if len(got) != len(tc.states) {
				t.Fatalf("candidate count = %d, want %d: %#v", len(got), len(tc.states), got)
			}
			for i, state := range tc.states {
				if got[i].State != state {
					t.Errorf("candidate %d state = %q, want %q", i, got[i].State, state)
				}
			}
		})
	}
}

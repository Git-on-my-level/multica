package main

import (
	"strings"
	"testing"
)

func TestParsePrivateRuntimePeers(t *testing.T) {
	const (
		workspace = "21743fbf-5050-499a-9d87-28d986f0050c"
		first     = "3774b8d8-a1ce-4df7-908d-e93671dfd2f1"
		second    = "0ca3e54f-38eb-459d-934c-bf8c2282a1c7"
	)
	valid := strings.Join([]string{workspace, first, second}, ",")
	for _, tc := range []struct {
		name string
		raw  string
		want string
		bad  bool
	}{
		{name: "unset"},
		{name: "canonical", raw: valid, want: valid},
		{name: "normalize", raw: strings.ToUpper(workspace) + ", " + first + ", " + second, want: valid},
		{name: "workspace only", raw: workspace, bad: true},
		{name: "unknown ID syntax", raw: workspace + "," + first + ",someone", bad: true},
		{name: "duplicate user", raw: workspace + "," + first + "," + first, bad: true},
		{name: "extra user", raw: valid + "," + workspace, bad: true},
		{name: "zero user", raw: workspace + "," + first + ",00000000-0000-0000-0000-000000000000", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePrivateRuntimePeers(tc.raw)
			if got != tc.want || (err != nil) != tc.bad {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}

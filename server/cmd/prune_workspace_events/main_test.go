package main

import (
	"io"
	"strings"
	"testing"
)

func TestParsePruneConfigRequiresExplicitSafeBounds(t *testing.T) {
	valid, err := parsePruneConfig([]string{"--retention", "720h", "--batch-size", "250", "--max-batches", "8", "--apply"}, io.Discard)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if valid.Retention.String() != "720h0m0s" || valid.BatchSize != 250 || valid.MaxBatches != 8 || !valid.Apply {
		t.Fatalf("config = %+v", valid)
	}

	for _, args := range [][]string{
		{},
		{"--retention", "0"},
		{"--retention", "1h", "--batch-size", "0"},
		{"--retention", "1h", "--batch-size", "10001"},
		{"--retention", "1h", "--max-batches", "0"},
		{"--retention", "1h", "--workspace-id", "not-a-uuid"},
	} {
		if _, err := parsePruneConfig(args, io.Discard); err == nil {
			t.Errorf("parsePruneConfig(%v) unexpectedly succeeded", args)
		}
	}
}

func TestParsePruneConfigDefaultsToPreview(t *testing.T) {
	cfg, err := parsePruneConfig([]string{"--retention", "24h"}, io.Discard)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.Apply {
		t.Fatal("pruner unexpectedly defaults to mutation")
	}
	if got := cfg.Retention.String(); !strings.HasPrefix(got, "24h") {
		t.Fatalf("retention = %q", got)
	}
}

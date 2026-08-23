package agent

import (
	"log/slog"
	"testing"
)

func TestDevinIsACPFamily(t *testing.T) {
	t.Parallel()

	desc, ok := BuiltinRuntimeByID("devin")
	if !ok {
		t.Fatal("devin missing from BuiltinRuntimes")
	}
	if desc.ProtocolFamily != "devin" || desc.DefaultCommand != "devin" {
		t.Fatalf("unexpected descriptor: %+v", desc)
	}
	if desc.LaunchHeader != "devin acp" {
		t.Fatalf("launch header = %q, want devin acp", desc.LaunchHeader)
	}

	b, err := New("devin", Config{Logger: slog.Default(), ExecutablePath: "devin"})
	if err != nil {
		t.Fatalf("New(devin): %v", err)
	}
	if _, ok := b.(*devinBackend); !ok {
		t.Fatalf("New(devin) = %T, want *devinBackend", b)
	}

	resolved, err := ResolveBackend("devin", Config{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("ResolveBackend(devin): %v", err)
	}
	if _, ok := resolved.(*devinBackend); !ok {
		t.Fatalf("ResolveBackend(devin) = %T, want *devinBackend", resolved)
	}
}

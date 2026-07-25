package main

import "testing"

func TestAppURLFromEnvUsesConfiguredDeploymentURL(t *testing.T) {
	t.Setenv("MULTICA_APP_URL", " https://multica-01.tail76ea03.ts.net/ ")
	t.Setenv("FRONTEND_ORIGIN", "https://fallback.example/")

	if got := appURLFromEnv(); got != "https://multica-01.tail76ea03.ts.net" {
		t.Fatalf("appURLFromEnv() = %q, want configured deployment URL", got)
	}
}

func TestAppURLFromEnvFallsBackToFrontendOrigin(t *testing.T) {
	t.Setenv("MULTICA_APP_URL", "")
	t.Setenv("FRONTEND_ORIGIN", " https://fallback.example/ ")

	if got := appURLFromEnv(); got != "https://fallback.example" {
		t.Fatalf("appURLFromEnv() = %q, want normalized frontend origin", got)
	}
}

func TestAppURLFromEnvIsEmptyWithoutDeploymentURL(t *testing.T) {
	t.Setenv("MULTICA_APP_URL", "")
	t.Setenv("FRONTEND_ORIGIN", "")

	if got := appURLFromEnv(); got != "" {
		t.Fatalf("appURLFromEnv() = %q, want empty URL", got)
	}
}

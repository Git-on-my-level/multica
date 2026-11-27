package main

import (
	"encoding/hex"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

var buildCommit, date string // Linker stamps, using the same names as cmd/multica.

type versionResponse struct {
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuiltAt   string `json:"built_at,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

func newServerVersion(startedAt time.Time) versionResponse {
	info, _ := debug.ReadBuildInfo()
	return serverVersionFromBuild(version, buildCommit, commit, date, startedAt, info)
}

func serverVersionFromBuild(version, fullCommit, displayCommit, builtAt string, startedAt time.Time, info *debug.BuildInfo) versionResponse {
	resp := versionResponse{Version: strings.TrimSpace(version)}
	if resp.Version == "unknown" {
		resp.Version = ""
	}
	if !startedAt.IsZero() {
		resp.StartedAt = startedAt.UTC().Format(time.RFC3339)
	}
	if stamp, err := time.Parse(time.RFC3339, builtAt); err == nil {
		resp.BuiltAt = stamp.UTC().Format(time.RFC3339)
	}

	var vcsRevision string
	if info != nil {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				vcsRevision = setting.Value
				break
			}
		}
	}
	// Source builds can use Go's embedded VCS revision when no linker stamp
	// supplies a full Git object ID. vcs.time records the commit, not the build.
	for _, candidate := range []string{fullCommit, displayCommit, vcsRevision} {
		candidate = strings.TrimSpace(candidate)
		if len(candidate) != 40 && len(candidate) != 64 {
			continue
		}
		if _, err := hex.DecodeString(candidate); err == nil {
			resp.Commit = strings.ToLower(candidate)
			break
		}
	}
	return resp
}

func (v versionResponse) handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, v)
}

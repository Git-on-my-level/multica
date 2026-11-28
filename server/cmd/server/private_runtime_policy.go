package main

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

const privateRuntimePeersEnv = "MULTICA_PRIVATE_RUNTIME_PEERS"
const privateRuntimePeersSetting = "multica.private_runtime_peers"

func parsePrivateRuntimePeers(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	fields := strings.Split(raw, ",")
	errConfig := errors.New(privateRuntimePeersEnv + " must contain a workspace UUID and two different user UUIDs, separated by commas")
	if len(fields) != 3 {
		return "", errConfig
	}
	for i, field := range fields {
		id, err := uuid.Parse(strings.TrimSpace(field))
		if err != nil || id == uuid.Nil {
			return "", errConfig
		}
		fields[i] = id.String()
	}
	if fields[1] == fields[2] {
		return "", errConfig
	}
	return strings.Join(fields, ","), nil
}

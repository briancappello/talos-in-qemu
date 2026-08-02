package cluster

import (
	"context"
	"fmt"
)

// NODE FACTS, NOT PROBES. Everything in this file asks a maintenance-mode node
// a QUESTION and returns the answer. Nothing here decides whether a node is
// ready, and nothing here may be used to.
//
// That distinction is why this file exists at all rather than living in
// client.go, whose header rule (2) forbids any probe from comparing, returning
// or logging a version — because `talosctl version` prints a constant compiled
// into the binary and will do so with no node in sight. These functions run
// AFTER readiness has been established by a real round trip, and their answers
// become values written to the node's disk.

// NodeVersion asks a maintenance-mode node for its own Talos version.
//
// It NEVER errors on an unidentifiable version, only on a failed call: "" is a
// real answer and matches platform.InspectImageVersion's contract, so both
// sources of a version fail the same way and step 3's guard is the single place
// that refuses one. Returning an error here instead would put the refusal in
// two places and let them drift.
func NodeVersion(ctx context.Context, endpoint string) (string, error) {
	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		return "", err
	}

	defer c.Close() //nolint:errcheck

	resp, err := c.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("asking the node its Talos version: %w", err)
	}

	// One node, so one message — but ranging costs nothing and a nil Messages
	// slice is a real reply shape rather than a panic.
	for _, m := range resp.GetMessages() {
		if tag := m.GetVersion().GetTag(); tag != "" {
			return tag, nil
		}
	}

	return "", nil
}

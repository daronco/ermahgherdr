package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// tailscaleStatus is the minimal shape of `tailscale status --json` the resolver
// needs. The real payload is far larger; unknown fields are dropped on purpose
// so a future tailscale version does not break the parse.
type tailscaleStatus struct {
	Self *struct {
		UserID int64 `json:"UserID"`
	} `json:"Self"`
	User map[string]struct {
		LoginName string `json:"LoginName"`
	} `json:"User"`
}

// statusRunner produces the raw bytes of `tailscale status --json`. Split out
// so tests feed canned JSON through the same code path the real resolver uses,
// without a tailscale binary on the test host.
type statusRunner func(ctx context.Context) ([]byte, error)

// defaultStatusRunner shells out to the `tailscale` binary on PATH. A missing
// binary is a hard error — the caller is expected to turn it into the
// fail-closed "login required" message.
func defaultStatusRunner(ctx context.Context) ([]byte, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return nil, fmt.Errorf("tailscale binary not found on PATH: %w", err)
	}
	return exec.CommandContext(ctx, bin, "status", "--json").Output()
}

// resolveTailscaleLogin returns the login name of the node owner reported by
// `tailscale status --json`. Any error, missing Self.UserID, or missing
// User[uid].LoginName returns a non-nil error so the caller can fail closed.
func resolveTailscaleLogin(ctx context.Context, run statusRunner) (string, error) {
	data, err := run(ctx)
	if err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	var s tailscaleStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return "", fmt.Errorf("tailscale status: parse: %w", err)
	}
	if s.Self == nil || s.Self.UserID == 0 {
		return "", fmt.Errorf("tailscale status: node has no owning user (tagged node, or logged out)")
	}
	entry, ok := s.User[strconv.FormatInt(s.Self.UserID, 10)]
	if !ok || entry.LoginName == "" {
		return "", fmt.Errorf("tailscale status: no LoginName for user %d", s.Self.UserID)
	}
	return entry.LoginName, nil
}

// tailscaleLoginResolver is the hook runServeWith uses to look up the login
// when none was passed explicitly. Overridden by tests to inject a canned
// outcome without touching the real tailscale binary.
var tailscaleLoginResolver = func(ctx context.Context) (string, error) {
	return resolveTailscaleLogin(ctx, defaultStatusRunner)
}

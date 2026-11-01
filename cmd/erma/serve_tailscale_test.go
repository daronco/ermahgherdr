package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolveTailscaleLogin(t *testing.T) {
	for _, c := range []struct {
		name      string
		json      string
		runErr    error
		wantLogin string
		wantErr   string
	}{
		{
			name:      "user-owned node",
			json:      `{"Self":{"UserID":1234567890},"User":{"1234567890":{"LoginName":"alice@example.com"}}}`,
			wantLogin: "alice@example.com",
		},
		{
			name:    "tagged node (zero UserID)",
			json:    `{"Self":{"UserID":0},"User":{}}`,
			wantErr: "no owning user",
		},
		{
			name:    "missing Self",
			json:    `{"User":{}}`,
			wantErr: "no owning user",
		},
		{
			name:    "User entry missing for Self.UserID",
			json:    `{"Self":{"UserID":42},"User":{}}`,
			wantErr: "no LoginName for user 42",
		},
		{
			name:    "User entry present but LoginName empty",
			json:    `{"Self":{"UserID":42},"User":{"42":{"LoginName":""}}}`,
			wantErr: "no LoginName for user 42",
		},
		{
			name:    "malformed JSON",
			json:    `{"Self":`,
			wantErr: "parse",
		},
		{
			name:    "runner error (binary missing / daemon down)",
			runErr:  errors.New("tailscale binary not found on PATH"),
			wantErr: "tailscale status",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(ctx context.Context) ([]byte, error) {
				if c.runErr != nil {
					return nil, c.runErr
				}
				return []byte(c.json), nil
			}
			got, err := resolveTailscaleLogin(context.Background(), run)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got login=%q", c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.wantLogin {
				t.Fatalf("got login %q, want %q", got, c.wantLogin)
			}
		})
	}
}

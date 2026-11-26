package agy

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestLaunchAndRestoreSandboxPolicy(t *testing.T) {
	p := New()
	p.resolvedBinary = "/usr/local/bin/agy"

	tests := []struct {
		name        string
		permissions ports.PermissionMode
		sandbox     ports.SandboxMode
		cfgSandbox  ports.SandboxMode
		wantFlags   []string
		forbidFlags []string
		wantErr     bool
	}{
		{
			name:        "unattended restricted sandbox",
			permissions: ports.PermissionModeBypassPermissions,
			sandbox:     ports.SandboxModeRestricted,
			wantFlags:   []string{"--sandbox", "--dangerously-skip-permissions"},
		},
		{
			name:        "auto mode in restricted sandbox",
			permissions: ports.PermissionModeAuto,
			sandbox:     ports.SandboxModeRestricted,
			wantFlags:   []string{"--sandbox", "--mode", "auto"},
			forbidFlags: []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "default sandbox omits flag",
			permissions: ports.PermissionModeAuto,
			sandbox:     ports.SandboxModeDefault,
			wantFlags:   []string{"--mode", "auto"},
			forbidFlags: []string{"--sandbox"},
		},
		{
			name:        "explicit unrestricted omits sandbox flag",
			permissions: ports.PermissionModeBypassPermissions,
			sandbox:     ports.SandboxModeUnrestricted,
			wantFlags:   []string{"--dangerously-skip-permissions"},
			forbidFlags: []string{"--sandbox"},
		},
		{
			name:        "falls back to Config.Sandbox when top-level empty",
			permissions: ports.PermissionModeBypassPermissions,
			cfgSandbox:  ports.SandboxModeRestricted,
			wantFlags:   []string{"--sandbox", "--dangerously-skip-permissions"},
		},
		{
			name:        "unsupported sandbox mode fails closed",
			permissions: ports.PermissionModeAuto,
			sandbox:     ports.SandboxMode("container-magic"),
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run("launch/"+tc.name, func(t *testing.T) {
			cmd, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{
				WorkspacePath: "/tmp/fresh-worktree",
				Permissions:   tc.permissions,
				Sandbox:       tc.sandbox,
				Config:        ports.AgentConfig{Model: "gemini-4-neon-low", Sandbox: tc.cfgSandbox},
				Prompt:        "inspect repo",
			})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unsupported sandbox mode") {
					t.Fatalf("err = %v, want unsupported sandbox mode error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetLaunchCommand() error = %v", err)
			}
			for _, flag := range tc.wantFlags {
				if !slices.Contains(cmd, flag) {
					t.Errorf("argv %v missing expected flag %q", cmd, flag)
				}
			}
			for _, flag := range tc.forbidFlags {
				if slices.Contains(cmd, flag) {
					t.Errorf("argv %v unexpectedly contains %q", cmd, flag)
				}
			}
		})

		t.Run("restore/"+tc.name, func(t *testing.T) {
			cmd, ok, err := p.GetRestoreCommand(context.Background(), ports.RestoreConfig{
				Session: ports.SessionRef{
					ID:            "sess-1",
					WorkspacePath: "/tmp/fresh-worktree",
					Metadata:      map[string]string{ports.MetadataKeyAgentSessionID: "conv-123"},
				},
				Permissions: tc.permissions,
				Sandbox:     tc.sandbox,
				Config:      ports.AgentConfig{Model: "gemini-4-neon-low", Sandbox: tc.cfgSandbox},
			})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unsupported sandbox mode") {
					t.Fatalf("err = %v, want unsupported sandbox mode error", err)
				}
				return
			}
			if err != nil || !ok {
				t.Fatalf("GetRestoreCommand() ok=%v err=%v", ok, err)
			}
			for _, flag := range tc.wantFlags {
				if !slices.Contains(cmd, flag) {
					t.Errorf("restore argv %v missing expected flag %q", cmd, flag)
				}
			}
			for _, flag := range tc.forbidFlags {
				if slices.Contains(cmd, flag) {
					t.Errorf("restore argv %v unexpectedly contains %q", cmd, flag)
				}
			}
		})
	}
}

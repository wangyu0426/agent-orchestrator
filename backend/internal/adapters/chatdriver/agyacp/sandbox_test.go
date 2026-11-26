package agyacp

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCLILaunchArgsSandboxPolicy(t *testing.T) {
	getenv := func(string) string { return "" }

	t.Run("worker restricted sandbox emits --sandbox", func(t *testing.T) {
		args := cliLaunchArgs(nil, getenv, false, ports.SandboxModeRestricted)
		if !slices.Contains(args, "--sandbox") {
			t.Fatalf("args = %v, want --sandbox", args)
		}
	})

	t.Run("worker default sandbox omits --sandbox", func(t *testing.T) {
		args := cliLaunchArgs(nil, getenv, false, ports.SandboxModeDefault)
		if slices.Contains(args, "--sandbox") {
			t.Fatalf("args = %v, did not expect --sandbox", args)
		}
	})

	t.Run("reviewer readOnly always enforces --sandbox even if unrestricted passed", func(t *testing.T) {
		args := cliLaunchArgs(nil, getenv, true, ports.SandboxModeUnrestricted)
		if !slices.Contains(args, "--sandbox") {
			t.Fatalf("args = %v, want --sandbox for readOnly reviewer", args)
		}
	})
}

func TestLaunchSandboxCompatibilityChecks(t *testing.T) {
	t.Run("legacy server rejects restricted sandbox", func(t *testing.T) {
		res := resolver{
			getenv: func(string) string { return "" },
			isFile: func(string) bool { return true },
			resolveSpec: func(context.Context, binaryutil.BinarySpec) (string, error) {
				return "/usr/local/bin/agy_acp_server", nil
			},
		}
		cfg := buildConfig(nil, res, nil)
		_, err := cfg.Launch(context.Background(), acpdriver.LaunchConfig{
			Sandbox: ports.SandboxModeRestricted,
		})
		if !errors.Is(err, ports.ErrChatDriverIncompatible) {
			t.Fatalf("err = %v, want ErrChatDriverIncompatible", err)
		}
	})

	t.Run("unsupported sandbox mode fails closed", func(t *testing.T) {
		res := resolver{
			getenv: func(string) string { return "" },
			isFile: func(string) bool { return true },
			agyCLI: func(context.Context) (string, error) { return "/usr/local/bin/agy", nil },
			cliVersion: func(context.Context, string) (string, error) {
				return "1.5.17", nil
			},
		}
		cfg := buildConfig(nil, res, nil)
		_, err := cfg.Launch(context.Background(), acpdriver.LaunchConfig{
			Sandbox: ports.SandboxMode("bogus"),
		})
		if !errors.Is(err, ports.ErrChatDriverIncompatible) {
			t.Fatalf("err = %v, want ErrChatDriverIncompatible", err)
		}
	})
}

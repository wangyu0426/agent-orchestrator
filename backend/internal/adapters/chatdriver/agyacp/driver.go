// Package agyacp binds Google Antigravity to AO's reusable ACP Chat transport.
//
// The agy CLI has a built-in ACP server, `agy acp`, which steers a running turn
// when another prompt arrives. AO launches it from agy 1.5.4, the first release
// whose `agy acp` does not hang on a second approval. Older CLIs have no ACP
// mode, or that bug, so AO falls back to
// Google's separate `agy_acp_server` from the ACP registry (`antigravity-acp`),
// found beside the agy CLI or on PATH. Like Autohand's separately distributed
// `autohand-acp`, AO never downloads or bundles either. Login, models, MCP
// configuration, and conversations stay the user's own under GEMINI_HOME
// (default ~/.gemini); both servers keep Chat conversations in the same
// antigravity-acp folder.
package agyacp

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agy"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Plugin is the existing Agy agent-plugin surface the binding reuses: the agy
// CLI's location, beside which the ACP server is usually installed, and the
// canonical auth probe. The server itself is resolved separately because it is
// not the agy CLI.
type Plugin interface {
	ResolveBinary(context.Context) (string, error)
	AuthStatus(context.Context) (ports.AgentAuthStatus, error)
}

// Antigravity's ACP mode ids, as advertised by agy_acp_server 1.3.0.
const (
	modeDefault  = "default"
	modeAutoEdit = "auto_edit"
	modeYolo     = "yolo"
)

// `agy acp`'s mode ids. It has no mode that runs every tool without asking;
// that is a launch flag, and AO's approval can change between turns. 1.5.4's
// `auto` still asks before a command leaves the terminal sandbox, so AO's
// auto, which runs without asking, does not map onto it.
const cliModeAcceptEdits = "accept-edits"

// New constructs the Antigravity ACP Chat driver.
func New(plugin Plugin, log *slog.Logger) ports.ChatDriver {
	var agyCLI func(context.Context) (string, error)
	if plugin != nil {
		agyCLI = plugin.ResolveBinary
	}
	return acpdriver.New(buildConfig(plugin, newResolver(agyCLI), log), log)
}

func buildConfig(plugin Plugin, resolve resolver, log *slog.Logger) acpdriver.Config {
	return acpdriver.Config{
		Harness: domain.HarnessAgy,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityStreaming: true,
			ports.ChatCapabilityTools:     true,
			// Antigravity raises session/request_permission for every gated tool.
			ports.ChatCapabilityApprovals: true,
			ports.ChatCapabilityInterrupt: true,
			ports.ChatCapabilityResume:    true,
		},
		Probe: func(ctx context.Context) error {
			if _, err := resolve.target(ctx); err != nil {
				return fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			if plugin != nil {
				status, err := plugin.AuthStatus(ctx)
				if err == nil && status == ports.AgentAuthStatusUnauthorized {
					return ports.ErrChatAuthRequired
				}
				if err != nil && log != nil {
					// The ACP handshake stays authoritative for an inconclusive probe.
					log.Debug("agy auth probe inconclusive; continuing", "error", err)
				}
			}
			return nil
		},
		Launch: func(ctx context.Context, cfg acpdriver.LaunchConfig) (acpdriver.Launch, error) {
			target, err := resolve.target(ctx)
			if err != nil {
				return acpdriver.Launch{}, fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			env := make(map[string]string, len(cfg.Env)+2)
			for key, value := range cfg.Env {
				env[key] = value
			}
			if env[disableWorkspaceTrustEnvVar] == "" {
				env[disableWorkspaceTrustEnvVar] = "1"
			}
			if target.cli {
				return acpdriver.Launch{Command: target.path, Args: cliLaunchArgs(env, resolve.getenv, cfg.ReadOnly), Env: env}, nil
			}
			if cfg.ReadOnly {
				return acpdriver.Launch{}, fmt.Errorf("%w: AGY read-only reviewer requires native `agy acp`", ports.ErrChatDriverIncompatible)
			}
			if env[harnessPathEnvVar] == "" {
				if harness := resolve.harnessPath(target.path); harness != "" {
					env[harnessPathEnvVar] = harness
				}
			}
			return acpdriver.Launch{Command: target.path, Args: launchArgs(runtime.GOOS), Env: env}, nil
		},
		ValidateInitialize:  validateInitialize,
		AfterInitialize:     afterInitialize(newAuthBootstrap(agy.GeminiHome)),
		SessionMode:         sessionMode,
		SessionModeForAgent: sessionModeForAgent,
		SessionOptions:      sessionOptions,
		SessionPreamble:     sessionPreamble,
		// Every request Antigravity raises is answered by the user, except under
		// `agy acp` in AO's auto modes (see permissionPolicy). AO never selects
		// allow_always, which would let a cached grant survive a later switch to
		// a stricter approval mode.
		PermissionPolicy: permissionPolicy,
		ReadOnly:         agyReadOnlySession,
		SteerPrompt:      &acpdriver.SteerPrompt{Supported: cliSteers},
	}
}

// cliLaunchArgs starts `agy acp` using the user's normal logged-in CLI profile
// (default --app-data-dir antigravity-cli) rather than overriding --app-data-dir
// to antigravity-acp, which lacks the host CLI's persisted authentication.
// `agy acp` ignores the GEMINI_HOME variable and stores under its --gemini-dir,
// so a GEMINI_HOME set for the session or inherited by the daemon is passed as
// that flag, the same precedence agy.GeminiHome uses.
func cliLaunchArgs(env map[string]string, getenv func(string) string, readOnly bool) []string {
	args := []string{"acp"}
	if readOnly {
		args = append(args, "--sandbox")
	}
	home := strings.TrimSpace(env["GEMINI_HOME"])
	if home == "" {
		home = strings.TrimSpace(getenv("GEMINI_HOME"))
	}
	if home != "" {
		args = append(args, "--gemini-dir", home)
	}
	return args
}

// afterInitialize runs the separate server's sign-in bootstrap. `agy acp`
// reuses the CLI's own login, and its authenticate method could start a
// browser sign-in, so AO leaves it alone.
func afterInitialize(bootstrap authBootstrap) func(context.Context, *acpsdk.ClientSideConnection, acpsdk.InitializeResponse, acpdriver.LaunchConfig) error {
	return func(ctx context.Context, conn *acpsdk.ClientSideConnection, init acpsdk.InitializeResponse, cfg acpdriver.LaunchConfig) error {
		if isCLIServer(init) {
			return nil
		}
		return bootstrap.run(ctx, conn, init, cfg)
	}
}

// sessionModeForAgent maps AO's approvals onto `agy acp`'s modes. AO's auto
// modes take accept-edits, and permissionPolicy approves the commands it still
// asks about. The separate server keeps sessionMode.
func sessionModeForAgent(init acpsdk.InitializeResponse) func(ports.PermissionMode) string {
	if !isCLIServer(init) {
		return nil
	}
	return func(permission ports.PermissionMode) string {
		switch ports.NormalizePermissionMode(permission) {
		case ports.PermissionModeDefault:
			return modeDefault
		case ports.PermissionModeAcceptEdits, ports.PermissionModeAuto, ports.PermissionModeBypassPermissions:
			return cliModeAcceptEdits
		default:
			return ""
		}
	}
}

// permissionPolicy approves a request once under AO's auto modes, which is
// what the separate server's yolo mode does. `agy acp` has no such mode. The
// separate server in yolo never asks, so this changes nothing there.
func permissionPolicy(permission ports.PermissionMode, request acpsdk.RequestPermissionRequest) (acpsdk.PermissionOptionId, bool) {
	switch ports.NormalizePermissionMode(permission) {
	case ports.PermissionModeAuto, ports.PermissionModeBypassPermissions:
	default:
		return "", false
	}
	for _, option := range request.Options {
		if option.Kind == acpsdk.PermissionOptionKindAllowOnce {
			return option.OptionId, true
		}
	}
	return "", false
}

// launchArgs mirrors the registry entry's per-platform arguments. The Linux
// build requires an explicit empty --uid flag.
func launchArgs(goos string) []string {
	if goos == "linux" {
		return []string{"--uid="}
	}
	return nil
}

// sessionMode maps AO's approval vocabulary onto Antigravity's mode ids.
// Antigravity has no classifier mode between auto_edit (edits only) and yolo
// (all tools). AO's auto means "run without asking", which is how the
// orchestrator and long worker turns operate, so it selects yolo, like Goose's
// auto. Default keeps every gated tool behind an approval.
func sessionMode(permission ports.PermissionMode) string {
	switch ports.NormalizePermissionMode(permission) {
	case ports.PermissionModeDefault:
		return modeDefault
	case ports.PermissionModeAcceptEdits:
		return modeAutoEdit
	case ports.PermissionModeAuto, ports.PermissionModeBypassPermissions:
		return modeYolo
	default:
		return ""
	}
}

func sessionOptions(settings ports.ChatTurnSettings) []acpdriver.SessionOption {
	var options []acpdriver.SessionOption
	if model := strings.TrimSpace(settings.Model); model != "" {
		options = append(options, acpdriver.SessionOption{ID: "model", Value: model})
	}
	if effort := strings.TrimSpace(settings.Effort); effort != "" {
		options = append(options, acpdriver.SessionOption{ID: "thought_level", Value: effort})
	}
	return options
}

// sessionPreamble delivers AO's standing instructions as a text block ahead of
// the first prompt. Antigravity accepts no system-prompt launch flag, and it
// treats an embedded resource as something to look up (it web-searched the
// resource URI in testing) rather than as instructions, so plain text is the
// channel it follows. The instructions do not depend on any mode the user can
// switch, so they survive approval changes.
func sessionPreamble(cfg acpdriver.LaunchConfig) []acpsdk.ContentBlock {
	standing := strings.TrimSpace(cfg.SystemPrompt)
	if standing == "" {
		return nil
	}
	return []acpsdk.ContentBlock{acpsdk.TextBlock(
		"Standing instructions from Agent Orchestrator. Follow them for this whole session:\n" +
			"<ao_standing_instructions>\n" + standing + "\n</ao_standing_instructions>",
	)}
}

// agyReadOnlySession pins AGY review sessions to "default" mode (where `agy acp`
// asks before every file edit and shell command) and enforces a fail-closed
// permission policy that allows only read-only PR/git inspection commands and
// the exact reviewer submit commands, rejecting all edits and other commands
// without ever parking for human approval.
func agyReadOnlySession(cfg acpdriver.LaunchConfig) acpdriver.ReadOnlySession {
	return acpdriver.ReadOnlySession{
		Mode:             modeDefault,
		PermissionPolicy: reviewerPermissionPolicy(strings.TrimSpace(cfg.Env["AO_REVIEW_WORKER_SESSION_ID"])),
	}
}

func reviewerPermissionPolicy(workerSessionID string) acpdriver.PermissionPolicy {
	return func(_ ports.PermissionMode, params acpsdk.RequestPermissionRequest) (acpsdk.PermissionOptionId, bool) {
		if params.ToolCall.Kind != nil && *params.ToolCall.Kind == acpsdk.ToolKindExecute {
			if command := extractCommand(params.ToolCall.RawInput); command != "" {
				if claudecode.ReviewerSubmitCommandAllowed(command, workerSessionID) || reviewerReadOnlyCommandAllowed(command) {
					if id, ok := permissionOption(params.Options, acpsdk.PermissionOptionKindAllowOnce); ok {
						return id, true
					}
				}
			}
		}
		for _, kind := range []acpsdk.PermissionOptionKind{acpsdk.PermissionOptionKindRejectOnce, acpsdk.PermissionOptionKindRejectAlways} {
			if id, ok := permissionOption(params.Options, kind); ok {
				return id, true
			}
		}
		return acpdriver.CancelPermission, true
	}
}

func extractCommand(rawInput any) string {
	input, ok := rawInput.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"CommandLine", "command"} {
		if cmd, _ := input[key].(string); strings.TrimSpace(cmd) != "" {
			return strings.TrimSpace(cmd)
		}
	}
	return ""
}

func reviewerReadOnlyCommandAllowed(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false
	}
	// Reject any shell chaining, piping, redirection, quoting, or expansion.
	for _, forbidden := range []string{";", "|", "&", ">", "<", "`", "$", "\n", "\r", "'", "\"", "\\"} {
		if strings.Contains(cmd, forbidden) {
			return false
		}
	}
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "git":
		return gitReadOnlyArgsAllowed(fields[1], fields[2:])
	case "gh":
		if len(fields) >= 3 && fields[1] == "pr" {
			return ghPRReadOnlyArgsAllowed(fields[2], fields[3:])
		}
	}
	return false
}

func gitReadOnlyArgsAllowed(subcmd string, args []string) bool {
	var allowedFlags map[string]bool
	allowCountFlag := false
	allowUnifiedFlag := false
	switch subcmd {
	case "status":
		allowedFlags = map[string]bool{
			"--short": true, "-s": true, "--branch": true, "-b": true,
			"--porcelain": true, "--untracked-files=no": true,
			"--untracked-files=normal": true, "--untracked-files=all": true,
			"-uno": true, "-u": true,
		}
	case "diff":
		allowedFlags = map[string]bool{
			"--stat": true, "--numstat": true, "--name-only": true,
			"--name-status": true, "--cached": true, "--staged": true,
			"--no-color": true, "--color=never": true,
			"--no-ext-diff": true, "--no-textconv": true,
		}
		allowUnifiedFlag = true
	case "log":
		allowedFlags = map[string]bool{
			"--oneline": true, "--stat": true, "--name-only": true,
			"--name-status": true, "--no-color": true, "--color=never": true,
			"--no-merges": true, "--first-parent": true,
		}
		allowCountFlag = true
	case "show":
		allowedFlags = map[string]bool{
			"--stat": true, "--name-only": true, "--name-status": true,
			"--no-patch": true, "-s": true, "--oneline": true,
			"--no-color": true, "--color=never": true,
			"--no-ext-diff": true, "--no-textconv": true,
		}
	case "rev-parse":
		allowedFlags = map[string]bool{
			"--short": true, "--abbrev-ref": true, "--verify": true,
			"--show-toplevel": true, "--is-inside-work-tree": true,
		}
	case "merge-base":
		allowedFlags = map[string]bool{"--is-ancestor": true}
	case "ls-files":
		allowedFlags = map[string]bool{
			"--cached": true, "--others": true, "--exclude-standard": true,
			"--modified": true,
		}
	default:
		return false
	}

	afterDoubleDash := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !afterDoubleDash && arg == "--" {
			afterDoubleDash = true
			continue
		}
		if !afterDoubleDash && strings.HasPrefix(arg, "-") {
			if allowedFlags[arg] {
				continue
			}
			if allowCountFlag {
				if arg == "-n" {
					if i+1 >= len(args) || !isASCIIPositiveInt(args[i+1]) {
						return false
					}
					i++
					continue
				}
				if strings.HasPrefix(arg, "-n") && isASCIIPositiveInt(arg[2:]) {
					continue
				}
				if strings.HasPrefix(arg, "--max-count=") && isASCIIPositiveInt(strings.TrimPrefix(arg, "--max-count=")) {
					continue
				}
				if len(arg) > 1 && isASCIIPositiveInt(arg[1:]) {
					continue
				}
			}
			if allowUnifiedFlag {
				if strings.HasPrefix(arg, "-U") && isASCIINonNegativeInt(arg[2:]) {
					continue
				}
				if strings.HasPrefix(arg, "--unified=") && isASCIINonNegativeInt(strings.TrimPrefix(arg, "--unified=")) {
					continue
				}
			}
			return false
		}
		if !isSafeReadOnlyArg(arg) {
			return false
		}
	}
	return true
}

func ghPRReadOnlyArgsAllowed(subcmd string, args []string) bool {
	var allowedFlags map[string]bool
	allowJSON := false
	switch subcmd {
	case "view":
		allowedFlags = map[string]bool{"--comments": true}
		allowJSON = true
	case "diff":
		allowedFlags = map[string]bool{"--name-only": true, "--patch": true, "--color=never": true}
	case "checks":
		allowedFlags = map[string]bool{"--required": true}
	default:
		return false
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if allowedFlags[arg] {
				continue
			}
			if arg == "--repo" || arg == "-R" {
				if i+1 >= len(args) || !isSafeReadOnlyArg(args[i+1]) {
					return false
				}
				i++
				continue
			}
			if allowJSON && arg == "--json" {
				if i+1 >= len(args) || !isSafeReadOnlyArg(args[i+1]) {
					return false
				}
				i++
				continue
			}
			return false
		}
		if !isSafeReadOnlyArg(arg) {
			return false
		}
	}
	return true
}

func isASCIIPositiveInt(s string) bool {
	if s == "" {
		return false
	}
	nonZero := false
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		if s[i] != '0' {
			nonZero = true
		}
	}
	return nonZero
}

func isASCIINonNegativeInt(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isSafeReadOnlyArg(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '.', '/', '_', '-', ':', '~', '^', '@', ',':
			continue
		default:
			return false
		}
	}
	return true
}

func permissionOption(options []acpsdk.PermissionOption, kind acpsdk.PermissionOptionKind) (acpsdk.PermissionOptionId, bool) {
	for _, option := range options {
		if option.Kind == kind {
			return option.OptionId, true
		}
	}
	return "", false
}

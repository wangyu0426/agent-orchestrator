package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestProjectAddBootstrapURL(t *testing.T) {
	cfg := setConfigEnv(t)
	resp := `{
		"action": "cloned_and_registered",
		"project": {"id": "dev-rag-service", "name": "dev-rag-service", "kind": "single_repo", "path": "/srv/projects/dev-rag-service", "repo": "https://github.com/acme/dev-rag-service.git", "defaultBranch": "develop"},
		"effective": {
			"defaultBranch": "develop", "defaultBranchSource": "git_remote",
			"workerAgent": "claude-code", "workerAgentSource": "quota_primary",
			"autoReview": true, "autoReviewSource": "global",
			"trackerIntake": {"enabled": true, "daemonGateEnabled": true, "provider": "github", "repo": "acme/dev-rag-service", "assignee": "octocat", "source": "global"},
			"quotaRouting": {"workerFailover": true, "reviewerFailover": true, "orchestratorFailover": false, "quotumConfigured": true}
		},
		"readiness": {"ready": true, "gitOk": true, "scmOk": true, "trackerOk": true, "agentOk": true}
	}`
	srv, capture := projectServer(t, http.StatusCreated, resp)
	writeRunFileFor(t, cfg, srv)

	out, errOut, err := executeCLI(t, Deps{
		ProcessAlive: func(int) bool { return true },
	}, "project", "add", "https://github.com/acme/dev-rag-service.git")
	if err != nil {
		t.Fatalf("ao project add <url> failed: %v\nstderr=%s", err, errOut)
	}
	if capture.method != http.MethodPost || capture.path != "/api/v1/projects/bootstrap" {
		t.Fatalf("request = %s %s, want POST /api/v1/projects/bootstrap", capture.method, capture.path)
	}
	var body bootstrapProjectRequest
	if err := json.Unmarshal(capture.body, &body); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if body.RemoteURL != "https://github.com/acme/dev-rag-service.git" {
		t.Fatalf("remoteUrl = %q", body.RemoteURL)
	}
	for _, want := range []string{"bootstrapped project dev-rag-service", "branch:        develop (git_remote)", "auto-review:   true (global)", "ready=true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestProjectAddBootstrapUsageErrors(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, _ := projectServer(t, http.StatusOK, `{}`)
	writeRunFileFor(t, cfg, srv)

	if _, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "add", "https://github.com/acme/demo.git", "--path", "/tmp/demo"); err == nil {
		t.Fatal("expected usage error when both <repo-url> and --path are provided")
	}
	if _, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "project", "add", "https://github.com/acme/demo.git", "--auto-review", "--no-auto-review"); err == nil {
		t.Fatal("expected usage error when both --auto-review and --no-auto-review are provided")
	}
}

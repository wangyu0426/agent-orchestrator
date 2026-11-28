// Package githubreview submits AO review results through a narrowly scoped
// GitHub App credential. Reviewer harnesses pass structured results to the
// daemon and never receive the App token or private key.
package githubreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

var (
	// ErrInvalidRequest reports a malformed or incomplete native review request.
	ErrInvalidRequest = errors.New("github review: invalid request")
	// ErrRepositoryForbidden reports a request for a repository outside the allowlist.
	ErrRepositoryForbidden = errors.New("github review: repository is not authorized")
	// ErrSameActor reports a reviewer App actor that cannot independently review this PR.
	ErrSameActor = errors.New("github review: reviewer and worker resolve to the same GitHub actor")
	// ErrStaleHead reports that the PR head no longer matches the reviewed commit.
	ErrStaleHead = errors.New("github review: pull request head changed since the review started")
	// ErrProviderRejected reports a native review request rejected by GitHub.
	ErrProviderRejected = errors.New("github review: provider rejected native review")
	// ErrUnverifiedReview reports that GitHub's response did not match the requested review.
	ErrUnverifiedReview = errors.New("github review: provider response did not match the requested review")
	// ErrConflictingRetry reports that a run already has a different submission recorded.
	ErrConflictingRetry = errors.New("github review: run already has a different provider submission")
	// ErrReviewLookupUncertain reports that an ambiguous delivery could not be reconciled.
	ErrReviewLookupUncertain = errors.New("github review: could not prove whether a previous submission exists")
)

const (
	// EventApprove submits a native GitHub approval.
	EventApprove = "APPROVE"
	// EventRequestChanges submits a native GitHub changes-requested review.
	EventRequestChanges = "REQUEST_CHANGES"
	// EventComment submits a native GitHub comment review.
	EventComment   = "COMMENT"
	maxReviewPages = 20
)

var reviewPermission = map[string]string{"metadata": "read", "pull_requests": "write"}

// AppIdentity resolves the effective bot account for a GitHub App.
type AppIdentity interface {
	AppActorLogin(context.Context) (string, error)
}

// AppTokenProvider is the token-scoped subset needed by this submitter.
type AppTokenProvider interface {
	AppIdentity
	MintScopedToken(context.Context, githubapp.ScopedTokenRequest) (githubapp.ScopedToken, error)
	InvalidateScopedToken(githubapp.ScopedTokenRequest)
}

// Config binds the daemon reviewer App to the repository allowlist. WorkerApp
// is optional when workers use legacy user credentials; when configured, its
// effective actor is also checked against the reviewer App.
type Config struct {
	ReviewerApp  AppTokenProvider
	WorkerApp    AppIdentity
	APIBaseURL   string
	GitHubHost   string
	Repositories []string
	HTTPClient   *http.Client
}

// Submitter performs native GitHub review submissions as the reviewer App.
type Submitter struct {
	reviewerApp AppTokenProvider
	workerApp   AppIdentity
	apiBaseURL  string
	host        string
	allowed     map[string]string
	httpClient  *http.Client
}

// Request identifies exactly one AO review attempt and the PR head it reviewed.
type Request struct {
	RunID           string
	PRURL           string
	PRAuthor        string
	ExpectedHeadSHA string
	Event           string
	Body            string
	Comments        []Comment
}

// Comment is one inline GitHub review comment anchored to a changed file line.
type Comment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// Result contains fields verified from the created or recovered GitHub review.
type Result struct {
	ID        string
	Author    string
	State     string
	CommitSHA string
	URL       string
}

// New validates the configured repository allowlist without making a network
// request. Actor identities are resolved only when a review is submitted.
func New(cfg Config) (*Submitter, error) {
	if cfg.ReviewerApp == nil {
		return nil, fmt.Errorf("%w: reviewer App is required", ErrInvalidRequest)
	}
	allowed := make(map[string]string, len(cfg.Repositories))
	for _, raw := range cfg.Repositories {
		owner, repo, ok := strings.Cut(strings.TrimSpace(raw), "/")
		if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || strings.ContainsAny(raw, "?#") {
			return nil, fmt.Errorf("%w: repository %q must be owner/repo", ErrInvalidRequest, raw)
		}
		full := owner + "/" + strings.TrimSuffix(repo, ".git")
		allowed[strings.ToLower(full)] = full
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%w: at least one owner/repo must be allowed", ErrInvalidRequest)
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if base == "" {
		base = "https://api.github.com"
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: API base URL must be an absolute HTTP(S) URL", ErrInvalidRequest)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%w: plain HTTP API base URLs are allowed only for loopback test servers", ErrInvalidRequest)
	}
	host := strings.ToLower(strings.TrimSpace(cfg.GitHubHost))
	if host == "" {
		host = "github.com"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Submitter{
		reviewerApp: cfg.ReviewerApp,
		workerApp:   cfg.WorkerApp,
		apiBaseURL:  base,
		host:        host,
		allowed:     allowed,
		httpClient:  client,
	}, nil
}

// Submit publishes one review as APPROVE, REQUEST_CHANGES, or COMMENT. It
// checks for a prior run marker before posting, which recovers a response lost
// after GitHub accepted the mutation without creating a duplicate review.
func (s *Submitter) Submit(ctx context.Context, in Request) (Result, error) {
	owner, repo, number, fullRepo, err := s.parsePRURL(in.PRURL)
	if err != nil {
		return Result{}, err
	}
	if s.allowed[strings.ToLower(fullRepo)] == "" {
		return Result{}, fmt.Errorf("%w: %s is outside the configured reviewer repository scope", ErrRepositoryForbidden, fullRepo)
	}
	if strings.TrimSpace(in.RunID) == "" || len(in.RunID) > 256 || !validSHA(in.ExpectedHeadSHA) {
		return Result{}, fmt.Errorf("%w: run id and full reviewed head SHA are required", ErrInvalidRequest)
	}
	event := strings.ToUpper(strings.TrimSpace(in.Event))
	if event != EventApprove && event != EventRequestChanges && event != EventComment {
		return Result{}, fmt.Errorf("%w: event must be APPROVE, REQUEST_CHANGES, or COMMENT", ErrInvalidRequest)
	}
	if event == EventRequestChanges && strings.TrimSpace(in.Body) == "" && len(in.Comments) == 0 {
		return Result{}, fmt.Errorf("%w: REQUEST_CHANGES requires a review body or inline comment", ErrInvalidRequest)
	}
	if event == EventComment && strings.TrimSpace(in.Body) == "" && len(in.Comments) == 0 {
		return Result{}, fmt.Errorf("%w: COMMENT requires a review body or inline comment", ErrInvalidRequest)
	}
	if len(in.Body) > 65536 || len(in.Comments) > 100 {
		return Result{}, fmt.Errorf("%w: review body or inline comment count exceeds GitHub limits", ErrInvalidRequest)
	}
	for i := range in.Comments {
		comment := &in.Comments[i]
		cleanPath := path.Clean(strings.TrimSpace(comment.Path))
		side := strings.ToUpper(strings.TrimSpace(comment.Side))
		if path.IsAbs(strings.TrimSpace(comment.Path)) || strings.Contains(comment.Path, "\\") || comment.Line < 1 || cleanPath == "." || strings.HasPrefix(cleanPath, "../") || (side != "LEFT" && side != "RIGHT") || strings.TrimSpace(comment.Body) == "" || len(comment.Body) > 65536 {
			return Result{}, fmt.Errorf("%w: each inline comment requires a repository path, positive line, LEFT/RIGHT side, and non-empty body", ErrInvalidRequest)
		}
		comment.Side = side
	}

	reviewerActor, err := s.reviewerApp.AppActorLogin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("resolve reviewer App actor: %w", err)
	}
	reviewerActor = normalizeActor(reviewerActor)
	if reviewerActor == "" {
		return Result{}, fmt.Errorf("%w: GitHub did not return the reviewer App actor", ErrUnverifiedReview)
	}
	if s.workerApp != nil {
		workerActor, actorErr := s.workerApp.AppActorLogin(ctx)
		if actorErr != nil {
			return Result{}, fmt.Errorf("resolve worker App actor: %w", actorErr)
		}
		if sameActor(reviewerActor, workerActor) {
			return Result{}, fmt.Errorf("%w: both configured Apps resolve to %q; use different GitHub Apps, not different installations", ErrSameActor, reviewerActor)
		}
	}
	if strings.TrimSpace(in.PRAuthor) != "" && sameActor(reviewerActor, in.PRAuthor) {
		return Result{}, fmt.Errorf("%w: PR author %q is also the reviewer App actor; no native review was posted", ErrSameActor, reviewerActor)
	}

	repositoryName := repo
	tokenReq := githubapp.ScopedTokenRequest{Repositories: []string{repositoryName}, Permissions: clonePermissions(reviewPermission)}
	token, err := s.reviewerApp.MintScopedToken(ctx, tokenReq)
	if err != nil {
		return Result{}, fmt.Errorf("mint reviewer token for %s: %w", fullRepo, err)
	}
	if len(token.FullNames) != 1 || !strings.EqualFold(token.FullNames[0], fullRepo) {
		s.reviewerApp.InvalidateScopedToken(tokenReq)
		return Result{}, fmt.Errorf("%w: reviewer token scope %v does not match %s", ErrRepositoryForbidden, token.FullNames, fullRepo)
	}

	marker := requestMarker(in.RunID, event, in.Body, in.Comments)
	apiPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), number)
	if existing, found, findErr := s.findRunReview(ctx, token.Token, tokenReq, apiPath, marker, in, event, reviewerActor); findErr != nil {
		return Result{}, findErr
	} else if found {
		return existing, nil
	}

	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := s.doJSON(ctx, token.Token, tokenReq, http.MethodGet, apiPath, nil, &pull, http.StatusOK); err != nil {
		return Result{}, err
	}
	if !strings.EqualFold(pull.Head.SHA, in.ExpectedHeadSHA) {
		return Result{}, fmt.Errorf("%w: expected %s, provider reports %s", ErrStaleHead, in.ExpectedHeadSHA, pull.Head.SHA)
	}
	if in.PRAuthor != "" && !sameActor(pull.User.Login, in.PRAuthor) {
		return Result{}, fmt.Errorf("%w: stored PR author %q no longer matches provider author %q", ErrUnverifiedReview, in.PRAuthor, pull.User.Login)
	}
	if sameActor(reviewerActor, pull.User.Login) {
		return Result{}, fmt.Errorf("%w: live PR author %q is also the reviewer App actor; no native review was posted", ErrSameActor, reviewerActor)
	}

	comments := make([]Comment, 0, len(in.Comments))
	comments = append(comments, in.Comments...)
	payload := map[string]any{
		"commit_id": in.ExpectedHeadSHA,
		"event":     event,
		"body":      appendMarker(in.Body, marker),
	}
	if len(comments) > 0 {
		payload["comments"] = comments
	}
	var created reviewResponse
	if err := s.doJSON(ctx, token.Token, tokenReq, http.MethodPost, apiPath+"/reviews", payload, &created, http.StatusOK, http.StatusCreated); err != nil {
		if errors.Is(err, ErrProviderRejected) || errors.Is(err, ErrStaleHead) {
			return Result{}, err
		}
		// POST delivery may be ambiguous. Reconcile by the stable attempt marker
		// before returning an error so a caller retry cannot contradict or repeat it.
		if existing, found, findErr := s.findRunReview(ctx, token.Token, tokenReq, apiPath, marker, in, event, reviewerActor); findErr != nil {
			return Result{}, fmt.Errorf("%w: could not reconcile provider delivery: %w; original submission error: %w", ErrReviewLookupUncertain, findErr, err)
		} else if found {
			return existing, nil
		}
		return Result{}, err
	}
	result, err := verifyReview(created, in.ExpectedHeadSHA, event, reviewerActor)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

type reviewResponse struct {
	ID       int64  `json:"id"`
	Login    string `json:"-"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	URL      string `json:"html_url"`
	Body     string `json:"body"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Submitter) findRunReview(ctx context.Context, token string, tokenReq githubapp.ScopedTokenRequest, pullPath, marker string, in Request, event, expectedActor string) (Result, bool, error) {
	for page := 1; page <= maxReviewPages; page++ {
		endpoint := pullPath + "/reviews?per_page=100&page=" + strconv.Itoa(page)
		var reviews []reviewResponse
		if err := s.doJSON(ctx, token, tokenReq, http.MethodGet, endpoint, nil, &reviews, http.StatusOK); err != nil {
			return Result{}, false, fmt.Errorf("inspect prior GitHub review submissions: %w", err)
		}
		prefix := runMarkerPrefix(in.RunID)
		for _, review := range reviews {
			if !strings.Contains(review.Body, prefix) {
				continue
			}
			if !strings.Contains(review.Body, marker) {
				return Result{}, false, ErrConflictingRetry
			}
			result, err := verifyReview(review, in.ExpectedHeadSHA, event, expectedActor)
			if err != nil {
				return Result{}, false, fmt.Errorf("%w: prior run marker was found but its state, actor, or commit did not match", ErrConflictingRetry)
			}
			return result, true, nil
		}
		if len(reviews) < 100 {
			return Result{}, false, nil
		}
	}
	return Result{}, false, ErrReviewLookupUncertain
}

func (s *Submitter) doJSON(ctx context.Context, token string, tokenReq githubapp.ScopedTokenRequest, method, endpoint string, payload, out any, success ...int) error {
	var reader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode GitHub review request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	u := s.apiBaseURL + endpoint
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("build GitHub review request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ao-agent-orchestrator/native-review")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub native review request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read GitHub native review response: %w", err)
	}
	for _, code := range success {
		if resp.StatusCode == code {
			if out == nil || len(b) == 0 {
				return nil
			}
			if err := json.Unmarshal(b, out); err != nil {
				return fmt.Errorf("decode GitHub native review response: %w", err)
			}
			return nil
		}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.reviewerApp.InvalidateScopedToken(tokenReq)
		return fmt.Errorf("GitHub rejected the scoped reviewer token; token cache invalidated")
	}
	message := githubMessage(b)
	if method == http.MethodPost {
		if resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(message), "head") {
			return fmt.Errorf("%w: %s", ErrStaleHead, message)
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
			return fmt.Errorf("%w: GitHub returned HTTP %d: %s", ErrProviderRejected, resp.StatusCode, message)
		}
	}
	return fmt.Errorf("GitHub API returned HTTP %d: %s", resp.StatusCode, message)
}

func (s *Submitter) parsePRURL(raw string) (owner, repo string, number int, fullRepo string, err error) {
	u, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), s.host) || u.User != nil || u.RawQuery != "" {
		return "", "", 0, "", fmt.Errorf("%w: PR URL must be an HTTPS GitHub pull request on %s", ErrInvalidRequest, s.host)
	}
	parts := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
	if len(parts) != 4 || !strings.EqualFold(parts[2], "pull") {
		return "", "", 0, "", fmt.Errorf("%w: malformed GitHub pull request URL", ErrInvalidRequest)
	}
	n, convErr := strconv.Atoi(parts[3])
	if convErr != nil || n <= 0 {
		return "", "", 0, "", fmt.Errorf("%w: pull request number must be positive", ErrInvalidRequest)
	}
	owner, repo, number = parts[0], strings.TrimSuffix(parts[1], ".git"), n
	if owner == "" || repo == "" || strings.ContainsAny(owner+repo, "?#") {
		return "", "", 0, "", fmt.Errorf("%w: malformed GitHub repository path", ErrInvalidRequest)
	}
	return owner, repo, number, owner + "/" + repo, nil
}

func requestMarker(runID, event, body string, comments []Comment) string {
	encoded, _ := json.Marshal(struct {
		Event    string
		Body     string
		Comments []Comment
	}{Event: event, Body: body, Comments: comments})
	fingerprint := sha256.Sum256(encoded)
	return fmt.Sprintf("%s payload:%s -->", runMarkerPrefix(runID), hex.EncodeToString(fingerprint[:]))
}

func runMarkerPrefix(runID string) string {
	runHash := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("<!-- ao-review-run:%s", hex.EncodeToString(runHash[:]))
}

func appendMarker(body, marker string) string {
	if strings.TrimSpace(body) == "" {
		return marker
	}
	return strings.TrimRight(body, " \t\r\n") + "\n\n" + marker
}

func verifyReview(review reviewResponse, expectedSHA, expectedEvent, expectedActor string) (Result, error) {
	wantState := map[string]string{EventApprove: "APPROVED", EventRequestChanges: "CHANGES_REQUESTED", EventComment: "COMMENTED"}[expectedEvent]
	actor := strings.TrimSpace(review.User.Login)
	state := strings.ToUpper(strings.TrimSpace(review.State))
	commit := strings.TrimSpace(review.CommitID)
	if review.ID <= 0 || !sameActor(actor, expectedActor) || state != wantState || !strings.EqualFold(commit, expectedSHA) {
		return Result{}, fmt.Errorf("%w: expected actor=%q state=%q commit=%q, received actor=%q state=%q commit=%q", ErrUnverifiedReview, expectedActor, wantState, expectedSHA, actor, state, commit)
	}
	return Result{ID: strconv.FormatInt(review.ID, 10), Author: actor, State: state, CommitSHA: commit, URL: strings.TrimSpace(review.URL)}, nil
}

func validSHA(raw string) bool {
	if len(raw) != 40 && len(raw) != 64 {
		return false
	}
	for _, r := range raw {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func normalizeActor(raw string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "@")))
}
func sameActor(left, right string) bool {
	return normalizeActor(left) != "" && normalizeActor(left) == normalizeActor(right)
}

func clonePermissions(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func githubMessage(body []byte) string {
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && strings.TrimSpace(parsed.Message) != "" {
		message := strings.TrimSpace(parsed.Message)
		if len(message) > 300 {
			message = message[:300]
		}
		return message
	}
	return "provider rejected request"
}

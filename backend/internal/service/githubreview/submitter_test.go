package githubreview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/githubapp"
)

const (
	testHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prURL    = "https://github.com/org/repo/pull/7"
)

type testApp struct {
	actor       string
	fullNames   []string
	identityErr error
	mintErr     error
	mintCalls   int
	lastReq     githubapp.ScopedTokenRequest
	invalidated int
}

func (a *testApp) AppActorLogin(context.Context) (string, error) { return a.actor, a.identityErr }
func (a *testApp) MintScopedToken(_ context.Context, req githubapp.ScopedTokenRequest) (githubapp.ScopedToken, error) {
	a.mintCalls++
	a.lastReq = req
	if a.mintErr != nil {
		return githubapp.ScopedToken{}, a.mintErr
	}
	return githubapp.ScopedToken{Token: "scoped-review-token", FullNames: append([]string(nil), a.fullNames...)}, nil
}
func (a *testApp) InvalidateScopedToken(githubapp.ScopedTokenRequest) { a.invalidated++ }

type testReview struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	URL      string `json:"html_url"`
	Body     string `json:"body"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

type reviewServer struct {
	headSHA               string
	actor                 string
	reviewerActor         string
	reviews               []testReview
	posts                 int
	lastEvent             string
	lastCommit            string
	lastBody              string
	lastComments          []Comment
	rejectPost            bool
	losePostResponse      bool
	rejectLookupAfterPost bool
	rejectLookup          bool
	rejectHead            bool
}

func (s *reviewServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer scoped-review-token" {
		http.Error(w, `{"message":"wrong token"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/pulls/7":
		if s.rejectHead {
			http.Error(w, `{"message":"temporary head lookup failure"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"head": map[string]string{"sha": s.headSHA},
			"user": map[string]string{"login": "worker[bot]"},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/pulls/7/reviews":
		if s.rejectLookup || (s.rejectLookupAfterPost && s.posts > 0) {
			http.Error(w, `{"message":"Pull request reviews permission is missing"}`, http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(s.reviews)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/org/repo/pulls/7/reviews":
		s.posts++
		var in struct {
			CommitID string    `json:"commit_id"`
			Event    string    `json:"event"`
			Body     string    `json:"body"`
			Comments []Comment `json:"comments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"message":"bad payload"}`, http.StatusBadRequest)
			return
		}
		s.lastEvent, s.lastCommit, s.lastBody, s.lastComments = in.Event, in.CommitID, in.Body, in.Comments
		if s.rejectPost {
			http.Error(w, `{"message":"Pull request review permission is missing"}`, http.StatusForbidden)
			return
		}
		state := map[string]string{EventApprove: "APPROVED", EventRequestChanges: "CHANGES_REQUESTED", EventComment: "COMMENTED"}[in.Event]
		review := testReview{ID: int64(200 + s.posts), State: state, CommitID: in.CommitID, URL: "https://github.com/org/repo/pull/7#pullrequestreview-201", Body: in.Body}
		review.User.Login = s.actor
		s.reviews = append(s.reviews, review)
		if s.losePostResponse {
			http.Error(w, `{"message":"upstream response lost"}`, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(review)
	default:
		http.NotFound(w, r)
	}
}

func makeSubmitter(t *testing.T, server *reviewServer, workerActor string) (*Submitter, *testApp) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	t.Cleanup(srv.Close)
	reviewerActor := server.reviewerActor
	if reviewerActor == "" {
		reviewerActor = server.actor
	}
	reviewer := &testApp{actor: reviewerActor, fullNames: []string{"org/repo"}}
	var worker AppIdentity
	if workerActor != "" {
		worker = &testApp{actor: workerActor}
	}
	submitter, err := New(Config{
		ReviewerApp:  reviewer,
		WorkerApp:    worker,
		APIBaseURL:   srv.URL,
		GitHubHost:   "github.com",
		Repositories: []string{"org/repo"},
		HTTPClient:   srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return submitter, reviewer
}

func submitRequest(event string) Request {
	return Request{
		RunID:           "run-123",
		PRURL:           prURL,
		PRAuthor:        "worker[bot]",
		ExpectedHeadSHA: testHead,
		Event:           event,
		Body:            "Review summary",
		Comments:        []Comment{{Path: "src/main.go", Line: 12, Side: "RIGHT", Body: "Handle the empty value."}},
	}
}

func TestSubmitUsesNativeEventActorHeadAndVerifiesState(t *testing.T) {
	tests := []struct {
		event string
		state string
	}{
		{EventApprove, "APPROVED"},
		{EventRequestChanges, "CHANGES_REQUESTED"},
		{EventComment, "COMMENTED"},
	}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			server := &reviewServer{headSHA: testHead, actor: "reviewer-app[bot]"}
			submitter, app := makeSubmitter(t, server, "worker-app[bot]")
			result, err := submitter.Submit(context.Background(), submitRequest(tt.event))
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if server.posts != 1 || server.lastEvent != tt.event || server.lastCommit != testHead {
				t.Fatalf("outbound review posts=%d event=%q commit=%q", server.posts, server.lastEvent, server.lastCommit)
			}
			if result.Author != "reviewer-app[bot]" || result.State != tt.state || result.CommitSHA != testHead || result.ID == "" {
				t.Fatalf("verified result = %+v, want actor reviewer-app[bot], state %s, head %s", result, tt.state, testHead)
			}
			if !strings.Contains(server.lastBody, "ao-review-run:") || len(server.lastComments) != 1 || server.lastComments[0].Path != "src/main.go" {
				t.Fatalf("native review body/comments = %q / %+v", server.lastBody, server.lastComments)
			}
			if server.lastComments[0].Side != "RIGHT" {
				t.Fatalf("inline comment side = %q, want RIGHT", server.lastComments[0].Side)
			}
			if app.mintCalls != 1 || app.lastReq.Repositories[0] != "repo" || app.lastReq.Permissions["pull_requests"] != "write" || len(app.lastReq.Permissions) != 2 {
				t.Fatalf("token scope = calls:%d request:%+v, want only org/repo pull_requests write and metadata read", app.mintCalls, app.lastReq)
			}
		})
	}
}

func TestSubmitRejectsSameEffectiveActorAndUnauthorizedRepository(t *testing.T) {
	t.Run("same configured actor", func(t *testing.T) {
		server := &reviewServer{headSHA: testHead, actor: "same-app[bot]"}
		submitter, app := makeSubmitter(t, server, "same-app[bot]")
		_, err := submitter.Submit(context.Background(), submitRequest(EventApprove))
		if !errors.Is(err, ErrSameActor) || app.mintCalls != 0 || server.posts != 0 {
			t.Fatalf("Submit err=%v token_calls=%d posts=%d; want same-actor rejection before credentials", err, app.mintCalls, server.posts)
		}
	})
	t.Run("same PR author", func(t *testing.T) {
		server := &reviewServer{headSHA: testHead, actor: "worker[bot]"}
		submitter, app := makeSubmitter(t, server, "reviewer-app[bot]")
		_, err := submitter.Submit(context.Background(), submitRequest(EventRequestChanges))
		if !errors.Is(err, ErrSameActor) || app.mintCalls != 0 || server.posts != 0 {
			t.Fatalf("Submit err=%v token_calls=%d posts=%d; want same-author rejection before credentials", err, app.mintCalls, server.posts)
		}
	})
	t.Run("repository outside allowlist", func(t *testing.T) {
		server := &reviewServer{headSHA: testHead, actor: "reviewer-app[bot]"}
		submitter, app := makeSubmitter(t, server, "worker-app[bot]")
		request := submitRequest(EventApprove)
		request.PRURL = "https://github.com/other/repo/pull/7"
		_, err := submitter.Submit(context.Background(), request)
		if !errors.Is(err, ErrRepositoryForbidden) || app.mintCalls != 0 || server.posts != 0 {
			t.Fatalf("Submit err=%v token_calls=%d posts=%d; want repo-scope rejection", err, app.mintCalls, server.posts)
		}
	})
}

func TestSubmitRejectsStaleHeadAndProviderPermissionFailure(t *testing.T) {
	t.Run("stale head", func(t *testing.T) {
		server := &reviewServer{headSHA: strings.Repeat("b", 40), actor: "reviewer-app[bot]"}
		submitter, _ := makeSubmitter(t, server, "worker-app[bot]")
		_, err := submitter.Submit(context.Background(), submitRequest(EventApprove))
		if !errors.Is(err, ErrStaleHead) || server.posts != 0 {
			t.Fatalf("Submit err=%v posts=%d; want stale-head error without POST", err, server.posts)
		}
	})
	t.Run("provider rejects review", func(t *testing.T) {
		server := &reviewServer{headSHA: testHead, actor: "reviewer-app[bot]", rejectPost: true}
		submitter, _ := makeSubmitter(t, server, "worker-app[bot]")
		_, err := submitter.Submit(context.Background(), submitRequest(EventRequestChanges))
		if !errors.Is(err, ErrProviderRejected) || server.posts != 1 {
			t.Fatalf("Submit err=%v posts=%d; want explicit provider rejection", err, server.posts)
		}
	})
}

func TestSubmitMarksPrePostFailures(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*reviewServer, *testApp)
	}{
		{
			name: "reviewer identity lookup",
			configure: func(_ *reviewServer, app *testApp) {
				app.identityErr = errors.New("identity service unavailable")
			},
		},
		{
			name: "token mint",
			configure: func(_ *reviewServer, app *testApp) {
				app.mintErr = errors.New("token service unavailable")
			},
		},
		{
			name: "prior marker lookup",
			configure: func(server *reviewServer, _ *testApp) {
				server.rejectLookup = true
			},
		},
		{
			name: "pull request head lookup",
			configure: func(server *reviewServer, _ *testApp) {
				server.rejectHead = true
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := &reviewServer{headSHA: testHead, actor: "reviewer-app[bot]"}
			submitter, app := makeSubmitter(t, server, "worker-app[bot]")
			tc.configure(server, app)
			_, err := submitter.Submit(context.Background(), submitRequest(EventRequestChanges))
			if !errors.Is(err, ErrPrePostFailure) || server.posts != 0 {
				t.Fatalf("Submit err=%v posts=%d; want a confirmed pre-POST failure", err, server.posts)
			}
		})
	}
}

func TestSubmitRecoversAmbiguousDeliveryAndRejectsContradictoryRetry(t *testing.T) {
	server := &reviewServer{headSHA: testHead, actor: "reviewer-app[bot]", losePostResponse: true}
	submitter, _ := makeSubmitter(t, server, "worker-app[bot]")
	first, err := submitter.Submit(context.Background(), submitRequest(EventRequestChanges))
	if err != nil || first.State != "CHANGES_REQUESTED" {
		t.Fatalf("ambiguous delivery recovery = %+v err=%v", first, err)
	}
	server.losePostResponse = false
	retried, err := submitter.Submit(context.Background(), submitRequest(EventRequestChanges))
	if err != nil || retried.ID != first.ID || server.posts != 1 {
		t.Fatalf("idempotent retry = %+v err=%v posts=%d", retried, err, server.posts)
	}
	if _, err := submitter.Submit(context.Background(), submitRequest(EventApprove)); !errors.Is(err, ErrConflictingRetry) || server.posts != 1 {
		t.Fatalf("contradictory retry err=%v posts=%d; want rejection without duplicate", err, server.posts)
	}
}

func TestSubmitAmbiguousDeliveryWithFailedLookupRemainsRetryable(t *testing.T) {
	server := &reviewServer{headSHA: testHead, actor: "reviewer-app[bot]", losePostResponse: true, rejectLookupAfterPost: true}
	submitter, _ := makeSubmitter(t, server, "worker-app[bot]")
	if _, err := submitter.Submit(context.Background(), submitRequest(EventRequestChanges)); !errors.Is(err, ErrReviewLookupUncertain) || errors.Is(err, ErrProviderRejected) || errors.Is(err, ErrPrePostFailure) {
		t.Fatalf("ambiguous result err=%v, want lookup-uncertain without permanent provider rejection", err)
	}
	server.rejectLookupAfterPost = false
	server.losePostResponse = false
	got, err := submitter.Submit(context.Background(), submitRequest(EventRequestChanges))
	if err != nil || got.State != "CHANGES_REQUESTED" || server.posts != 1 {
		t.Fatalf("reconciled retry = %+v err=%v posts=%d, want original review and no duplicate", got, err, server.posts)
	}
}

func TestSubmitRejectsUnverifiedProviderResponse(t *testing.T) {
	server := &reviewServer{headSHA: testHead, actor: "someone-else[bot]", reviewerActor: "reviewer-app[bot]"}
	submitter, _ := makeSubmitter(t, server, "worker-app[bot]")
	_, err := submitter.Submit(context.Background(), submitRequest(EventApprove))
	if !errors.Is(err, ErrUnverifiedReview) || server.posts != 1 {
		t.Fatalf("Submit err=%v posts=%d; want unverified actor rejection", err, server.posts)
	}
	if _, err := submitter.Submit(context.Background(), submitRequest(EventApprove)); !errors.Is(err, ErrConflictingRetry) || server.posts != 1 {
		t.Fatalf("retry after unverified response err=%v posts=%d", err, server.posts)
	}
}

func TestNewRejectsMalformedConfiguration(t *testing.T) {
	_, err := New(Config{ReviewerApp: &testApp{}, Repositories: []string{"not-a-repository"}})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New malformed repositories err=%v, want ErrInvalidRequest", err)
	}
}

func TestNewRejectsInsecureRemoteAPIBase(t *testing.T) {
	_, err := New(Config{ReviewerApp: &testApp{}, Repositories: []string{"org/repo"}, APIBaseURL: "http://api.example.test"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New remote HTTP API base err=%v, want ErrInvalidRequest", err)
	}
}

package workerquota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const quotumPollInterval = 30 * time.Second

// Binding identifies a board-visible subscription and any model-group windows
// which apply in addition to the account-wide windows. Operators choose these
// explicitly because AO and Quotum expose different account and model IDs.
type Binding struct {
	SourceID    string              `json:"sourceId"`
	ModelGroups map[string][]string `json:"modelGroups"`
}

type quotumCard struct {
	ID           string          `json:"id"`
	Provider     string          `json:"provider"`
	SuccessAt    int64           `json:"successAt"`
	StaleAfterMS int64           `json:"staleAfterMs"`
	Stale        *bool           `json:"stale"`
	Error        json.RawMessage `json:"error"`
	Windows      []struct {
		ID      string   `json:"id"`
		Label   string   `json:"label"`
		Used    *float64 `json:"used"`
		ResetAt int64    `json:"resetAt"`
	} `json:"windows"`
}

// Quotum reads the hub's board overview. It never invokes a measurement or
// login route. A shared cache coalesces simultaneous reads and bounds polling.
type Quotum struct {
	endpoint string
	cookie   *http.Cookie
	bindings map[domain.AgentHarness]Binding
	http     *http.Client
	mu       sync.Mutex
	next     time.Time
	cards    []quotumCard
	lastErr  error
}

var quotumProviders = map[domain.AgentHarness]string{
	domain.HarnessCodex: "codex", domain.HarnessClaudeCode: "claude", domain.HarnessAgy: "antigravity",
}

// NewQuotum connects the quota source to a configured dashboard and login.
func NewQuotum(base, board, session string, bindings map[domain.AgentHarness]Binding) (*Quotum, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Quotum base URL")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || ip.IsLoopback()
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return nil, errors.New("Quotum requires HTTPS or a loopback HTTP URL")
	}
	// Request cookies use only the name and value. Transport is checked above.
	cookie := &http.Cookie{
		Name: "quotum_session", Value: session, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
	if session == "" || cookie.Valid() != nil {
		return nil, errors.New("invalid Quotum session cookie value")
	}
	for harness, binding := range bindings {
		if quotumProviders[harness] == "" || binding.SourceID == "" {
			return nil, errors.New("Quotum binding requires a supported harness and sourceId")
		}
	}
	u = u.JoinPath("api", "overview")
	if board != "" {
		u.RawQuery = url.Values{"board": {board}}.Encode()
	}
	return &Quotum{endpoint: u.String(), cookie: cookie, bindings: bindings, http: &http.Client{
		Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

// Read returns the applicable windows from the cached board overview.
func (q *Quotum) Read(ctx context.Context, targets []Target) (map[Target][]Window, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	if !now.Before(q.next) {
		q.cards, q.lastErr = q.readOverview(ctx)
		if ctx.Err() == nil {
			q.next = now.Add(quotumPollInterval)
		}
	}
	if q.lastErr != nil {
		return nil, q.lastErr
	}
	result := make(map[Target][]Window, len(targets))
	for _, t := range targets {
		binding, ok := q.bindings[t.Harness]
		if !ok {
			continue
		}
		for _, card := range q.cards {
			if card.ID == binding.SourceID && card.Provider == quotumProviders[t.Harness] {
				result[t] = card.windows(binding.ModelGroups[t.Model])
				break
			}
		}
	}
	return result, nil
}

func (q *Quotum) readOverview(ctx context.Context) ([]quotumCard, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.endpoint, http.NoBody)
	if err != nil {
		return nil, errors.New("invalid Quotum overview request")
	}
	req.AddCookie(q.cookie)
	res, err := q.http.Do(req)
	if err != nil {
		return nil, errors.New("Quotum overview unreachable")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Quotum overview returned HTTP %d", res.StatusCode)
	}
	var snapshot struct {
		Sources []quotumCard `json:"sources"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&snapshot); err != nil || snapshot.Sources == nil {
		return nil, errors.New("invalid Quotum overview response")
	}
	return snapshot.Sources, nil
}

func (c quotumCard) windows(scoped []string) []Window {
	if c.Stale == nil || *c.Stale || (len(c.Error) > 0 && string(c.Error) != "null") ||
		c.SuccessAt <= 0 || c.StaleAfterMS <= 0 || c.StaleAfterMS > int64((24*time.Hour)/time.Millisecond) {
		return nil
	}
	var result []Window
	for _, w := range c.Windows {
		if w.Used == nil || *w.Used < 0 || *w.Used > 100 || !appliesToModel(w.ID, w.Label, scoped) {
			continue
		}
		window := Window{ObservedAt: time.UnixMilli(c.SuccessAt), ValidUntil: time.UnixMilli(c.SuccessAt + c.StaleAfterMS), Exhausted: *w.Used == 100}
		if w.ResetAt > 0 {
			window.ResetAt = time.UnixMilli(w.ResetAt)
		}
		result = append(result, window)
	}
	return result
}

func appliesToModel(id, label string, scoped []string) bool {
	if !strings.Contains(id, ":") && label == "" && (id == "session" || id == "weekly" || id == "window" || strings.HasPrefix(id, "window-")) {
		return true
	}
	for _, windowID := range scoped {
		if id == windowID {
			return true
		}
	}
	return false
}

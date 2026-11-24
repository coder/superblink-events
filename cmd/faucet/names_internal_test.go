package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// namesAPI is GitHub's REST API as faucet asks it, with no token, for the
// login of an account and the names of a public repository. A path that it
// was not given answers 404, as GitHub does for a private repository. delay
// holds every answer back, and an answer that faucet stops waiting for is
// never sent.
type namesAPI struct {
	*httptest.Server
	calls atomic.Int64

	mu    sync.Mutex
	users map[int64]string
	repos map[int64]faucet.Repository
	delay time.Duration
}

func newNamesAPI(t *testing.T) *namesAPI {
	t.Helper()
	g := &namesAPI{users: map[int64]string{}, repos: map[int64]faucet.Repository{}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls.Add(1)
		g.mu.Lock()
		delay := g.delay
		g.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var id int64
		if _, err := fmt.Sscanf(r.URL.Path, "/user/%d", &id); err == nil {
			if login, ok := g.users[id]; ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "login": login, "type": "Organization"})
				return
			}
		}
		if _, err := fmt.Sscanf(r.URL.Path, "/repositories/%d", &id); err == nil {
			if repo, ok := g.repos[id]; ok {
				_, name, _ := strings.Cut(repo.FullName, "/")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": name, "full_name": repo.FullName, "private": false,
					"owner": map[string]any{"id": repo.OwnerID, "login": repo.OwnerLogin}})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","status":"404"}`))
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *namesAPI) user(id int64, login string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[id] = login
}

func (g *namesAPI) repo(repo faucet.Repository) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[repo.ID] = repo
}

func (g *namesAPI) hold(delay time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.delay = delay
}

// timedPost posts a signed delivery and returns faucet's status and how long
// faucet took to answer.
func (s *loginServer) timedPost(t *testing.T, event, delivery string, body []byte) (int, time.Duration) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+"/webhook/github", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	start := time.Now()
	resp, err := s.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode, time.Since(start)
}

// pushOf is a push to the repository id of the account ownerID, which the
// delivery names login.
func pushOf(id int64, name string, ownerID int64, login string) []byte {
	body, _ := json.Marshal(map[string]any{"ref": "refs/heads/main", "repository": map[string]any{
		"id": id, "name": name, "full_name": login + "/" + name, "owner": map[string]any{"id": ownerID, "login": login},
	}})
	return body
}

// renameOf is a repository event that renamed the repository id from the
// name from.
func renameOf(id int64, fullName string, ownerID int64, from string) []byte {
	owner, name, _ := strings.Cut(fullName, "/")
	body, _ := json.Marshal(map[string]any{
		"action":     "renamed",
		"repository": map[string]any{"id": id, "name": name, "full_name": fullName, "owner": map[string]any{"id": ownerID, "login": owner}},
		"changes":    map[string]any{"repository": map[string]any{"name": map[string]any{"from": from}}},
	})
	return body
}

func (s *loginServer) boundTo(name string) int64 {
	repo, err := s.store.RepositoryByName(context.Background(), name)
	if err != nil {
		return 0
	}
	return repo.ID
}

// After GitHub renames an organization, its deliveries come in bursts that
// all name the new login. Faucet asks GitHub once, and the burst leaves the
// account holding the login that GitHub gave it.
func TestABurstOfDeliveriesAfterAnOwnerRenameLeavesTheOwnerResolving(t *testing.T) {
	t.Parallel()
	for _, delay := range []time.Duration{200 * time.Millisecond, 3 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			t.Parallel()
			github := newNamesAPI(t)
			srv := newLoginServer(t, func(c *loginConfig) { c.githubAPI, c.maxOpenConns = github.URL, maxOpenConns })
			const n = 40
			for i := range n {
				require.Less(t, mustStatus(srv.timedPost(t, "push", fmt.Sprintf("seed-%d", i), pushOf(int64(100+i), fmt.Sprintf("r%d", i), 7, "old"))), 300)
			}
			github.user(7, "new")
			github.hold(delay)

			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() {
					status, _ := srv.timedPost(t, "push", fmt.Sprintf("burst-%d", i), pushOf(int64(100+i), fmt.Sprintf("r%d", i), 7, "new"))
					require.Less(t, status, 300)
				})
			}
			wg.Wait()
			require.EqualValues(t, 1, github.calls.Load(), "faucet asked GitHub once")
			got, err := srv.store.OwnerByID(context.Background(), 7)
			require.NoError(t, err, "the account that GitHub confirmed is not quarantined")
			require.Equal(t, faucet.Owner{ID: 7, Login: "new"}, got)
			require.EqualValues(t, 100, srv.boundTo("new/r0"))
		})
	}
}

func mustStatus(status int, _ time.Duration) int { return status }

// GitHub's answer can lag a rename. An answer that still gives the name
// that the rename moved the repository from cannot be told from a kept
// rename that GitHub undid since, so faucet binds neither name and judges
// the body: sent again, under any delivery GUID, it asks GitHub nothing and
// binds nothing, even while GitHub refuses to answer.
func TestAnAnswerFromGitHubThatGivesTheOldNameBindsNeither(t *testing.T) {
	t.Parallel()
	github := newNamesAPI(t)
	srv := newLoginServer(t, func(c *loginConfig) { c.githubAPI = github.URL })
	before := faucet.Repository{ID: 5, OwnerID: 7, OwnerLogin: "coder", FullName: "coder/r"}
	github.user(7, "coder")
	github.repo(before)
	require.Less(t, mustStatus(srv.timedPost(t, "push", "d-1", pushOf(5, "r", 7, "coder"))), 300)

	rename := renameOf(5, "coder/s", 7, "r")
	require.Less(t, mustStatus(srv.timedPost(t, "repository", "rename-1", rename)), 300)
	require.Zero(t, srv.boundTo("coder/r"), "the old name reads nothing")
	require.Zero(t, srv.boundTo("coder/s"))

	asked := github.calls.Load()
	for i := range 10 {
		require.Less(t, mustStatus(srv.timedPost(t, "repository", fmt.Sprintf("replay-%d", i), rename)), 300)
	}
	require.Equal(t, asked, github.calls.Load(), "a body sent again asks GitHub nothing")
	require.Zero(t, srv.boundTo("coder/s"), "a body sent again binds nothing")
	require.Zero(t, srv.boundTo("coder/r"))
}

// Faucet answers every delivery within GitHub's 10 seconds although GitHub
// hangs and many deliveries ask it at once, and a delivery that asks nothing
// is not held up by them.
func TestDeliveriesThatAskAHangingGitHubAnswerInTime(t *testing.T) {
	t.Parallel()
	github := newNamesAPI(t)
	srv := newLoginServer(t, func(c *loginConfig) { c.githubAPI, c.maxOpenConns = github.URL, maxOpenConns })
	const n = 30
	for i := range n {
		id := int64(1000 + i)
		require.Less(t, mustStatus(srv.timedPost(t, "push", fmt.Sprintf("seed-%d", i), pushOf(id, "r", id, fmt.Sprintf("o%d", i)))), 300)
	}
	github.hold(time.Minute)

	times := make([]time.Duration, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			id := int64(1000 + i)
			event, body := "push", pushOf(id, "r", id, fmt.Sprintf("n%d", i))
			if i%2 == 0 {
				event, body = "repository", renameOf(id, fmt.Sprintf("o%d/s", i), id, "r")
			}
			var status int
			status, times[i] = srv.timedPost(t, event, fmt.Sprintf("load-%d", i), body)
			require.Less(t, status, 500)
		})
	}
	time.Sleep(200 * time.Millisecond)
	_, bystander := srv.timedPost(t, "push", "bystander", pushOf(1000, "r", 1000, "o0"))
	wg.Wait()
	t.Logf("slowest of %d deliveries that asked: %v; a delivery that asked nothing: %v", n, slices.Max(times), bystander)
	require.Less(t, bystander, 1500*time.Millisecond, "a delivery that asks GitHub nothing")
	for i, took := range times {
		require.Less(t, took, 5500*time.Millisecond, "delivery %d", i)
	}
}

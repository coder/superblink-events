package faucet_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

// The IDs of the answers that testdata/githubapi holds, which api.github.com
// gave to requests with no token on 2026-11-23.
const (
	coderOrgID   int64 = 95932066   // user_95932066.json: the organization coder
	thomasID     int64 = 2198487    // user_2198487.json: the user ThomasK33
	publicRepoID int64 = 1456631239 // repositories_public.json: coder/superblink-events
	acmeID       int64 = 4242
)

// namesStub is the part of GitHub's REST API that faucet asks, with no
// token, for the login of an account and the names of a repository. A path
// that it was not given answers as GitHub answers for a private repository.
type namesStub struct {
	url    string
	client *http.Client
	// arrived gets a value when a request that hangs or is held arrives.
	arrived chan struct{}

	mu      sync.Mutex
	answers map[string]githubAnswer
	asked   map[string]int
	tokens  int
}

type githubAnswer struct {
	status int
	header http.Header
	body   []byte
	// hang makes the stub answer nothing until faucet gives up.
	hang bool
	// release, when set, holds the answer back until it is closed.
	release chan struct{}
}

func newNamesStub(t *testing.T) *namesStub {
	t.Helper()
	g := &namesStub{arrived: make(chan struct{}, 16), answers: map[string]githubAnswer{}, asked: map[string]int{}}
	notFound := recorded(t, "repositories_private_404.json")
	notFound.status = http.StatusNotFound
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.asked[r.URL.Path]++
		if r.Header.Get("Authorization") != "" {
			g.tokens++
		}
		answer, ok := g.answers[r.URL.Path]
		g.mu.Unlock()
		if !ok || r.Method != http.MethodGet {
			answer = notFound
		}
		if answer.hang || answer.release != nil {
			g.arrived <- struct{}{}
			select {
			case <-r.Context().Done():
				return
			case <-released:
				return
			case <-answer.release:
			}
		}
		for k, v := range answer.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(answer.status)
		_, _ = w.Write(answer.body)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(released) })
	t.Cleanup(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		require.Zero(t, g.tokens, "faucet sends GitHub no token")
	})
	g.url, g.client = srv.URL, srv.Client()
	return g
}

func (g *namesStub) answer(path string, a githubAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answers[path] = a
}

func (g *namesStub) askedFor(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked[path]
}

func askingGitHub(g *namesStub) func(*faucet.Deps) {
	return func(d *faucet.Deps) { d.GitHubAPIURL, d.GitHubClient = g.url, g.client }
}

func userPath(id int64) string { return fmt.Sprintf("/user/%d", id) }

func repoPath(id int64) string { return fmt.Sprintf("/repositories/%d", id) }

// recorded is an answer that api.github.com gave.
func recorded(t *testing.T, name string) githubAnswer {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "githubapi", name))
	require.NoError(t, err)
	return githubAnswer{status: http.StatusOK, body: body}
}

// githubUser is the recorded answer for the user ThomasK33, given another
// ID and login.
func githubUser(t *testing.T, id int64, login string) githubAnswer {
	t.Helper()
	return edited(t, recorded(t, "user_2198487.json"), func(doc map[string]any) {
		doc["id"], doc["login"] = id, login
	})
}

// githubRepository is the recorded answer for the public repository
// coder/superblink-events, given other names.
func githubRepository(t *testing.T, id, ownerID int64, fullName string) githubAnswer {
	t.Helper()
	owner, name, _ := strings.Cut(fullName, "/")
	return edited(t, recorded(t, "repositories_public.json"), func(doc map[string]any) {
		doc["id"], doc["name"], doc["full_name"] = id, name, fullName
		o := doc["owner"].(map[string]any)
		o["id"], o["login"] = ownerID, owner
	})
}

func edited(t *testing.T, a githubAnswer, edit func(map[string]any)) githubAnswer {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(a.body, &doc))
	edit(doc)
	body, err := json.Marshal(doc)
	require.NoError(t, err)
	a.body = body
	return a
}

// rateLimited is GitHub's answer once a client with no token used its 60
// requests of the hour, as
// https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api
// shows it.
func rateLimited() githubAnswer {
	return githubAnswer{
		status: http.StatusForbidden,
		header: http.Header{"X-Ratelimit-Limit": {"60"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Used": {"60"}},
		body:   []byte(`{"message":"API rate limit exceeded for 203.0.113.7. (But here's the good news: Authenticated requests get a higher rate limit. Check out the documentation for more details.)","documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting"}`),
	}
}

// requireAcknowledgedWhileGitHubHangs serves a delivery while GitHub
// answers nothing, and moves the clock to the end of faucet's wait.
func requireAcknowledgedWhileGitHubHangs(t *testing.T, handler http.Handler, clock *quartz.Mock, github *namesStub, event, delivery string, body []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	served := make(chan *httptest.ResponseRecorder, 1)
	go func() { served <- serveWebhook(handler, newRequest(http.MethodPost, body, event, delivery)) }()
	select {
	case <-github.arrived:
	case <-ctx.Done():
		t.Fatal("faucet did not ask GitHub")
	}
	_, w := clock.AdvanceNext()
	w.MustWait(ctx)
	select {
	case rec := <-served:
		require.Less(t, rec.Code, 300, rec.Body.String())
	case <-ctx.Done():
		t.Fatal("faucet kept waiting for GitHub past its deadline")
	}
}

func requireOwner(t *testing.T, store *fakeStore, login string, id int64) {
	t.Helper()
	got, err := store.OwnerByLogin(context.Background(), login)
	require.NoError(t, err, login)
	require.Equal(t, faucet.Owner{ID: id, Login: login}, got)
}

func requireNoOwner(t *testing.T, store *fakeStore, login string) {
	t.Helper()
	_, err := store.OwnerByLogin(context.Background(), login)
	require.ErrorIs(t, err, faucet.ErrUnknownOwner, login)
}

// lifecycleBodyOf is lifecycleBody for the repository id.
func lifecycleBodyOf(id int64, action, fullName string, ownerID int64, changes string) []byte {
	owner, name, _ := strings.Cut(fullName, "/")
	return fmt.Appendf(nil, `{"action":%q,"changes":%s,"repository":{"id":%d,"name":%q,"full_name":%q,"owner":{"id":%d,"login":%q}}}`,
		action, changes, id, name, fullName, ownerID, owner)
}

// GitHub signs a delivery's body and nothing else, so whoever kept a body can
// send it again under a new delivery GUID after the account that it names was
// renamed. The replay must not give the account its former login back, so
// that a grant of a repository under that login stays without access. When
// GitHub has no such account, limits faucet's rate or answers nothing, faucet
// cannot tell the replay from a rename, so the account holds neither login.
func TestAReplayedDeliveryGivesAnAccountNoFormerLoginBack(t *testing.T) {
	t.Parallel()
	kept := pushNaming(70, coderOrgID, "coder-legacy/tap")
	for name, answer := range map[string]func(t *testing.T) githubAnswer{
		"GitHubNamesTheCurrentLogin": func(t *testing.T) githubAnswer { return recorded(t, "user_95932066.json") },
		"NotFound": func(*testing.T) githubAnswer {
			return githubAnswer{status: http.StatusNotFound, body: []byte(`{"message":"Not Found"}`)}
		},
		"RateLimited": func(*testing.T) githubAnswer { return rateLimited() },
		"Hangs":       func(*testing.T) githubAnswer { return githubAnswer{hang: true} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			github := newNamesStub(t)
			store, clock, handler := newRepositoryHarness(t, askingGitHub(github))
			requireAcknowledged(t, handler, "push", "d-1", kept)
			requireOwner(t, store, "coder-legacy", coderOrgID)

			github.answer(userPath(coderOrgID), recorded(t, "user_95932066.json"))
			requireAcknowledged(t, handler, "push", "d-2", pushNaming(70, coderOrgID, "coder/tap"))
			requireOwner(t, store, "coder", coderOrgID)

			clock.Advance(2 * time.Minute)
			github.answer(userPath(coderOrgID), answer(t))
			if name == "Hangs" {
				requireAcknowledgedWhileGitHubHangs(t, handler, clock, github, "push", "d-3", kept)
			} else {
				requireAcknowledged(t, handler, "push", "d-3", kept)
			}
			requireUnbound(t, store, "coder-legacy/tap")
			requireNoOwner(t, store, "coder-legacy")
			require.Equal(t, 2, github.askedFor(userPath(coderOrgID)), "faucet asks GitHub before it moves a login")
			if name != "GitHubNamesTheCurrentLogin" {
				requireNoOwner(t, store, "coder")
				requireUnbound(t, store, "coder/tap")
				return
			}
			requireOwner(t, store, "coder", coderOrgID)
			requireBoundTo(t, store, "coder/tap", faucet.Repository{ID: 70, OwnerID: coderOrgID, OwnerLogin: "coder", FullName: "coder/tap"})
		})
	}
}

// An account that GitHub renamed takes its new login once GitHub, asked for
// the account's ID, gives it. Until GitHub answers, the account holds no
// login, the old one included, and faucet asks about one account at most once
// a minute, so a flood of deliveries cannot spend the 60 requests an hour
// that GitHub allows a client with no token. When GitHub gives another login
// than the delivery, GitHub's wins.
func TestAnAccountTakesTheLoginThatGitHubGivesIt(t *testing.T) {
	t.Parallel()
	github := newNamesStub(t)
	store, clock, handler := newRepositoryHarness(t, askingGitHub(github))
	requireAcknowledged(t, handler, "push", "d-1", pushNaming(70, coderOrgID, "coder-legacy/tap"))
	require.Zero(t, github.askedFor(userPath(coderOrgID)), "an account that faucet has not seen, under a login that no account holds, is added without asking")

	github.answer(userPath(coderOrgID), rateLimited())
	requireAcknowledged(t, handler, "push", "d-2", pushNaming(70, coderOrgID, "coder/tap"))
	requireNoOwner(t, store, "coder-legacy")
	requireNoOwner(t, store, "coder")
	requireUnbound(t, store, "coder-legacy/tap")

	github.answer(userPath(coderOrgID), recorded(t, "user_95932066.json"))
	requireAcknowledged(t, handler, "push", "d-3", pushNaming(70, coderOrgID, "coder/tap"))
	requireNoOwner(t, store, "coder")
	require.Equal(t, 1, github.askedFor(userPath(coderOrgID)), "faucet asks about an account at most once a minute")

	clock.Advance(time.Minute)
	requireAcknowledged(t, handler, "push", "d-4", pushNaming(70, coderOrgID, "coder-next/tap"))
	requireOwner(t, store, "coder", coderOrgID)
	requireNoOwner(t, store, "coder-next")
	requireBoundTo(t, store, "coder/tap", faucet.Repository{ID: 70, OwnerID: coderOrgID, OwnerLogin: "coder", FullName: "coder/tap"})
}

// One login names one account. A delivery that names an account under a
// login that faucet holds for another account moves the login only when
// GitHub gives it to the delivered account, so a kept delivery of the account
// that gave the login up does not take it back. Until GitHub answers, no
// account holds the login.
func TestALoginMovesToAnotherAccountOnlyWhenGitHubGivesItThere(t *testing.T) {
	t.Parallel()
	const formerID int64 = 1001
	github := newNamesStub(t)
	store, clock, handler := newRepositoryHarness(t, askingGitHub(github))
	kept := pushNaming(80, formerID, "ThomasK33/dotfiles")
	requireAcknowledged(t, handler, "push", "d-1", kept)

	requireAcknowledged(t, handler, "push", "d-2", pushNaming(81, thomasID, "ThomasK33/faucet"))
	requireNoOwner(t, store, "ThomasK33")
	requireUnbound(t, store, "ThomasK33/dotfiles")

	github.answer(userPath(thomasID), recorded(t, "user_2198487.json"))
	clock.Advance(time.Minute)
	requireAcknowledged(t, handler, "push", "d-3", pushNaming(81, thomasID, "ThomasK33/faucet"))
	requireOwner(t, store, "ThomasK33", thomasID)
	requireUnbound(t, store, "ThomasK33/dotfiles")

	github.answer(userPath(formerID), githubUser(t, formerID, "tk-former"))
	clock.Advance(time.Minute)
	requireAcknowledged(t, handler, "push", "d-4", kept)
	requireOwner(t, store, "ThomasK33", thomasID)
	requireBoundTo(t, store, "ThomasK33/faucet", faucet.Repository{ID: 81, OwnerID: thomasID, OwnerLogin: "ThomasK33", FullName: "ThomasK33/faucet"})
	requireOwner(t, store, "tk-former", formerID)
	requireUnbound(t, store, "ThomasK33/dotfiles")
}

// A rename that never reached faucet leaves it with the name from before,
// and the transfer after it quarantines the repository. Whoever kept the body
// of the missed rename can send it again under a new GUID, fresh, and its
// from is the name that faucet holds. It must not lift the quarantine, or a
// grant of the name that it gives would read a repository that moved away.
// Only GitHub's answer for the repository's ID lifts it, and GitHub gives
// none for a private repository to a client with no token.
func TestAMissedRenameSentAgainLiftsNoQuarantine(t *testing.T) {
	t.Parallel()
	t.Run("Private", func(t *testing.T) {
		t.Parallel()
		github := newNamesStub(t)
		store, clock, handler := newRepositoryHarness(t, askingGitHub(github))
		requireAcknowledged(t, handler, "push", "d-1", pushNaming(5, 9, "coder/r"))
		requireAcknowledged(t, handler, "repository", "d-2", lifecycleBody("transferred", "acme/s", 10, transferredFromOrg("coder")))
		clock.Advance(time.Minute)
		requireAcknowledged(t, handler, "repository", "d-3", lifecycleBody("renamed", "coder/s", 9, renamedFromName("r")))
		for _, name := range []string{"coder/r", "coder/s", "acme/s"} {
			requireUnbound(t, store, name)
		}
		require.Equal(t, 2, github.askedFor(repoPath(5)), "faucet asks GitHub about each rename and transfer")
	})
	t.Run("Public", func(t *testing.T) {
		t.Parallel()
		github := newNamesStub(t)
		store, clock, handler := newRepositoryHarness(t, askingGitHub(github))
		github.answer(repoPath(publicRepoID), rateLimited())
		requireAcknowledged(t, handler, "push", "d-1", pushNaming(publicRepoID, coderOrgID, "coder/faucet"))
		requireAcknowledged(t, handler, "repository", "d-2", lifecycleBodyOf(publicRepoID, "transferred", "acme/superblink-events", acmeID, transferredFromOrg("coder")))
		clock.Advance(time.Minute)
		requireAcknowledged(t, handler, "repository", "d-3", lifecycleBodyOf(publicRepoID, "renamed", "coder/superblink-events", coderOrgID, renamedFromName("faucet")))
		for _, name := range []string{"coder/faucet", "coder/superblink-events", "acme/superblink-events"} {
			requireUnbound(t, store, name)
		}

		github.answer(repoPath(publicRepoID), recorded(t, "repositories_public.json"))
		clock.Advance(time.Minute)
		requireAcknowledged(t, handler, "repository", "d-4", lifecycleBodyOf(publicRepoID, "transferred", "coder/superblink-events", coderOrgID, transferredFromOrg("acme")))
		requireBoundTo(t, store, "coder/superblink-events", faucet.Repository{ID: publicRepoID, OwnerID: coderOrgID, OwnerLogin: "coder", FullName: "coder/superblink-events"})
		requireUnbound(t, store, "coder/faucet")
		requireUnbound(t, store, "acme/superblink-events")
	})
}

// A transfer whose body faucet never judged, such as one signed before faucet
// kept the bodies it judged, can come again after the repository came back to
// the name that the transfer moved it from. For a public repository faucet
// takes GitHub's names, not the body's, so the transfer gives the repository
// no name back.
func TestAKeptTransferGivesAPublicRepositoryNoNameBack(t *testing.T) {
	t.Parallel()
	github := newNamesStub(t)
	store, clock, handler := newRepositoryHarness(t, askingGitHub(github))
	atCoder := faucet.Repository{ID: publicRepoID, OwnerID: coderOrgID, OwnerLogin: "coder", FullName: "coder/superblink-events"}
	atAcme := faucet.Repository{ID: publicRepoID, OwnerID: acmeID, OwnerLogin: "acme", FullName: "acme/superblink-events"}
	requireAcknowledged(t, handler, "push", "d-1", pushNaming(publicRepoID, coderOrgID, "coder/superblink-events"))

	away := lifecycleBodyOf(publicRepoID, "transferred", "acme/superblink-events", acmeID, transferredFromOrg("coder"))
	github.answer(repoPath(publicRepoID), githubRepository(t, publicRepoID, acmeID, "acme/superblink-events"))
	requireAcknowledged(t, handler, "repository", "d-2", away)
	requireBoundTo(t, store, "acme/superblink-events", atAcme)

	github.answer(repoPath(publicRepoID), recorded(t, "repositories_public.json"))
	clock.Advance(time.Minute)
	requireAcknowledged(t, handler, "repository", "d-3", lifecycleBodyOf(publicRepoID, "transferred", "coder/superblink-events", coderOrgID, transferredFromOrg("acme")))
	requireBoundTo(t, store, "coder/superblink-events", atCoder)

	neverJudged := fmt.Appendf(nil, `{"action":"transferred","changes":%s,"repository":{"id":%d,"name":"superblink-events","full_name":"acme/superblink-events","owner":{"id":%d,"login":"acme"}},"sender":{"login":"someone"}}`,
		transferredFromOrg("coder"), publicRepoID, acmeID)
	for i, body := range [][]byte{away, neverJudged} {
		clock.Advance(time.Minute)
		requireAcknowledged(t, handler, "repository", fmt.Sprintf("replay-%d", i), body)
		requireBoundTo(t, store, "coder/superblink-events", atCoder)
		requireUnbound(t, store, "acme/superblink-events")
	}
}

// While an account holds no login because GitHub did not confirm one, every
// delivery that names it asks GitHub again, at most once a minute, whatever
// login it carries, and GitHub's answer gives the account its login back.
func TestAQuarantinedAccountHealsOnTheNextDeliveryAfterGitHubAnswers(t *testing.T) {
	t.Parallel()
	for name, login := range map[string]string{"UnderTheOldLogin": "coder-legacy", "UnderTheNewLogin": "coder", "UnderAnotherLogin": "coder-next"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			github := newNamesStub(t)
			store, clock, handler := newRepositoryHarness(t, askingGitHub(github))
			requireAcknowledged(t, handler, "push", "d-1", pushNaming(70, coderOrgID, "coder-legacy/tap"))
			github.answer(userPath(coderOrgID), rateLimited())
			requireAcknowledged(t, handler, "push", "d-2", pushNaming(70, coderOrgID, "coder/tap"))
			requireNoOwner(t, store, "coder")
			requireNoOwner(t, store, "coder-legacy")

			github.answer(userPath(coderOrgID), recorded(t, "user_95932066.json"))
			clock.Advance(time.Minute)
			requireAcknowledged(t, handler, "push", "d-3", pushNaming(70, coderOrgID, login+"/tap"))
			requireOwner(t, store, "coder", coderOrgID)
			requireBoundTo(t, store, "coder/tap", faucet.Repository{ID: 70, OwnerID: coderOrgID, OwnerLogin: "coder", FullName: "coder/tap"})
			requireUnbound(t, store, "coder-legacy/tap")
		})
	}
}

// serveAsync serves a delivery in its own goroutine.
func serveAsync(handler http.Handler, event, delivery string, body []byte) <-chan *httptest.ResponseRecorder {
	served := make(chan *httptest.ResponseRecorder, 1)
	go func() { served <- serveWebhook(handler, newRequest(http.MethodPost, body, event, delivery)) }()
	return served
}

// Two deliveries that rename or transfer one repository can ask GitHub at
// once, and the one that asked first can get the older names. faucet writes
// GitHub's answers for one repository in the order in which it asked, so an
// older answer never replaces a newer one.
func TestAnOlderAnswerFromGitHubNeverReplacesANewerOne(t *testing.T) {
	t.Parallel()
	github := newNamesStub(t)
	store, _, handler := newRepositoryHarness(t, askingGitHub(github))
	requireAcknowledged(t, handler, "push", "d-1", pushNaming(publicRepoID, coderOrgID, "coder/superblink-events"))

	release := make(chan struct{})
	before := recorded(t, "repositories_public.json")
	before.release = release
	github.answer(repoPath(publicRepoID), before)
	kept := serveAsync(handler, "repository", "d-2", lifecycleBodyOf(publicRepoID, "renamed", "coder/superblink-events", coderOrgID, renamedFromName("faucet")))
	select {
	case <-github.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("faucet did not ask GitHub")
	}

	github.answer(repoPath(publicRepoID), githubRepository(t, publicRepoID, acmeID, "acme/superblink-events"))
	transfer := serveAsync(handler, "repository", "d-3", lifecycleBodyOf(publicRepoID, "transferred", "acme/superblink-events", acmeID, transferredFromOrg("coder")))
	var transferred *httptest.ResponseRecorder
	select {
	case transferred = <-transfer:
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	for _, served := range []<-chan *httptest.ResponseRecorder{kept, transfer} {
		if served == transfer && transferred != nil {
			continue
		}
		select {
		case rec := <-served:
			require.Less(t, rec.Code, 300, rec.Body.String())
		case <-time.After(10 * time.Second):
			t.Fatal("a delivery did not end")
		}
	}
	requireBoundTo(t, store, "acme/superblink-events", faucet.Repository{ID: publicRepoID, OwnerID: acmeID, OwnerLogin: "acme", FullName: "acme/superblink-events"})
	requireUnbound(t, store, "coder/superblink-events")
}

package faucet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet/internal/githubapi"
	"github.com/coder/quartz"
)

const (
	// githubAskBudget is how long the GitHub lookups of one delivery take
	// together at most. GitHub gives a webhook 10 seconds to answer.
	githubAskBudget = 4 * time.Second
	// accountAskInterval is how often faucet asks GitHub about one account at
	// most. GitHub gives a client with no token 60 requests an hour, and
	// every delivery that names an old login would ask.
	accountAskInterval = time.Minute
)

var (
	// errAskedRecently says that faucet asked GitHub about the account less
	// than accountAskInterval ago.
	errAskedRecently = xerrors.New("faucet asked GitHub about the account less than a minute ago")
	// errGitHubNotFound is GitHub's 404: no such account, or a repository
	// that is private or deleted, which GitHub does not tell apart for a
	// client with no token.
	errGitHubNotFound = xerrors.New("GitHub answered 404")
)

// githubNames asks GitHub's REST API, with no token, for the names that
// GitHub gives an account or a public repository now. GitHub signs only the
// body of a delivery and gives no time, so a body that someone kept cannot be
// told from a fresh one, and only GitHub can say whether its names are
// current.
type githubNames struct {
	base   *url.URL
	client *http.Client
	clock  quartz.Clock

	mu    sync.Mutex
	asked map[int64]askedAccount
}

// askedAccount is when faucet last asked GitHub about an account, and the
// login that GitHub gave, empty when it gave none.
type askedAccount struct {
	at    time.Time
	login string
}

func newGitHubNames(apiURL string, client *http.Client, clock quartz.Clock) (*githubNames, error) {
	base, err := githubapi.ParseBaseURL(apiURL)
	if err != nil {
		return nil, err
	}
	return &githubNames{base: base, client: client, clock: clock, asked: map[int64]askedAccount{}}, nil
}

// asks returns the lookups of one delivery, which end githubAskBudget after
// the first one starts. The caller calls done.
func (g *githubNames) asks(ctx context.Context) *deliveryAsks {
	return &deliveryAsks{names: g, parent: ctx}
}

type deliveryAsks struct {
	names  *githubNames
	parent context.Context
	ctx    context.Context
	stop   func()
}

func (a *deliveryAsks) context() context.Context {
	if a.ctx == nil {
		ctx, cancel := context.WithCancel(a.parent)
		timer := a.names.clock.AfterFunc(githubAskBudget, cancel)
		a.ctx, a.stop = ctx, func() { timer.Stop(); cancel() }
	}
	return a.ctx
}

func (a *deliveryAsks) done() {
	if a.stop != nil {
		a.stop()
	}
}

// account returns the account id with the login that GitHub gives it now.
// When faucet asked less than accountAskInterval ago, it returns
// errAskedRecently and the login that GitHub gave then, if any.
func (a *deliveryAsks) account(id int64) (Owner, error) {
	if last, recent := a.names.lastAsked(id); recent {
		return Owner{ID: id, Login: last.login}, errAskedRecently
	}
	owner, err := a.askAccount(id)
	a.names.answered(id, owner.Login)
	return owner, err
}

func (a *deliveryAsks) askAccount(id int64) (Owner, error) {
	var body struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := a.names.get(a.context(), fmt.Sprintf("/user/%d", id), &body); err != nil {
		return Owner{}, err
	}
	if body.ID != id || !githubapi.ValidOwner(body.Login) {
		return Owner{}, xerrors.New("GitHub answered with another account or a login that is not valid")
	}
	return Owner{ID: body.ID, Login: body.Login}, nil
}

// repository returns the repository id with the names that GitHub gives it
// now, and errGitHubNotFound when it is private or deleted.
func (a *deliveryAsks) repository(id int64) (Repository, error) {
	var body struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Owner    struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := a.names.get(a.context(), fmt.Sprintf("/repositories/%d", id), &body); err != nil {
		return Repository{}, err
	}
	repo := Repository{ID: body.ID, OwnerID: body.Owner.ID, OwnerLogin: body.Owner.Login, FullName: body.FullName}
	owner, _, err := githubapi.ParseRepo(repo.FullName)
	if repo.ID != id || err != nil || !strings.EqualFold(owner, repo.OwnerLogin) || repo.Validate() != nil {
		return Repository{}, xerrors.New("GitHub answered with another repository or names that are not valid")
	}
	return repo, nil
}

// lastAsked returns when faucet last asked about the account id, and whether
// that was less than accountAskInterval ago. When it was not, it records that
// faucet asks now.
func (g *githubNames) lastAsked(id int64) (askedAccount, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	maps.DeleteFunc(g.asked, func(_ int64, a askedAccount) bool { return now.Sub(a.at) >= accountAskInterval })
	if last, ok := g.asked[id]; ok {
		return last, true
	}
	g.asked[id] = askedAccount{at: now}
	return askedAccount{}, false
}

// answered records the login that GitHub gave for the account id, empty when
// it gave none.
func (g *githubNames) answered(id int64, login string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a, ok := g.asked[id]; ok {
		a.login = login
		g.asked[id] = a
	}
}

func (g *githubNames) get(ctx context.Context, path string, out any) error {
	resp, err := githubapi.Call(ctx, g.client, g.base, "", http.MethodGet, path, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, githubapi.MaxResponseBytes))
	switch {
	case err != nil:
		return xerrors.Errorf("read GitHub's answer: %w", err)
	case githubapi.RateLimited(resp.StatusCode, resp.Header, answer):
		return githubapi.ErrRateLimited
	case resp.StatusCode == http.StatusNotFound:
		return errGitHubNotFound
	case resp.StatusCode != http.StatusOK:
		return xerrors.Errorf("GitHub answered %d", resp.StatusCode)
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return xerrors.Errorf("decode GitHub's answer: %w", err)
	}
	return nil
}

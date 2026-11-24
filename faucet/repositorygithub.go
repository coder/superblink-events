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

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
)

const (
	// githubAskBudget is how long the GitHub lookups of one delivery take
	// together at most. GitHub gives a webhook 10 seconds to answer.
	githubAskBudget = 4 * time.Second
	// accountAskInterval is how often faucet asks GitHub about one account at
	// most. GitHub gives a client with no token 60 requests an hour, and
	// every delivery that names an old login would ask.
	accountAskInterval = time.Minute
	// maxConcurrentLookups is how many lookups one process runs at once. Each
	// holds a database connection while GitHub answers, and cmd/faucet's pool
	// holds 10, so a hanging GitHub cannot take the connections that other
	// deliveries need.
	maxConcurrentLookups = 4
)

var (
	// errAskedRecently says that faucet asked GitHub about the account less
	// than accountAskInterval ago.
	errAskedRecently = xerrors.New("faucet asked GitHub about the account less than a minute ago")
	// errNoLookupSlot says that the process ran maxConcurrentLookups lookups
	// for the whole budget of the delivery.
	errNoLookupSlot = xerrors.New("faucet asked GitHub as often at once as it may for the whole time it may wait")
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

	slots chan struct{}

	mu    sync.Mutex
	asked map[int64]time.Time
}

func newGitHubNames(apiURL string, client *http.Client, clock quartz.Clock) (*githubNames, error) {
	base, err := githubapi.ParseBaseURL(apiURL)
	if err != nil {
		return nil, err
	}
	return &githubNames{base: base, client: client, clock: clock, slots: make(chan struct{}, maxConcurrentLookups), asked: map[int64]time.Time{}}, nil
}

// asks returns the lookups of one delivery, which end githubAskBudget after
// the first one starts, waits for a slot and for the lock of the name
// included. The caller calls done.
func (g *githubNames) asks(ctx context.Context) *deliveryAsks {
	return &deliveryAsks{names: g, parent: ctx}
}

type deliveryAsks struct {
	names  *githubNames
	parent context.Context
	ctx    context.Context
	until  time.Time
	stop   func()
}

func (a *deliveryAsks) context() context.Context {
	if a.ctx == nil {
		ctx, cancel := context.WithCancel(a.parent)
		timer := a.names.clock.AfterFunc(githubAskBudget, cancel)
		a.ctx, a.until, a.stop = ctx, a.names.clock.Now().Add(githubAskBudget), func() { timer.Stop(); cancel() }
	}
	return a.ctx
}

// lookupOf waits, within the budget, for one of the process's lookup slots,
// and gives the store what is left of the budget to wait for the lock of the
// name. The caller calls release once the store returns. A lookup that got no
// slot asks GitHub nothing.
func lookupOf[T any](a *deliveryAsks, ask func() (T, error)) (lookup Lookup[T], release func()) {
	ctx := a.context()
	select {
	case a.names.slots <- struct{}{}:
		release = func() { <-a.names.slots }
	case <-ctx.Done():
		release = func() {}
		ask = func() (T, error) {
			var zero T
			return zero, errNoLookupSlot
		}
	}
	return Lookup[T]{Wait: a.until.Sub(a.names.clock.Now()), Ask: ask}, release
}

func (a *deliveryAsks) done() {
	if a.stop != nil {
		a.stop()
	}
}

// account returns the account id with the login that GitHub gives it now.
func (a *deliveryAsks) account(id int64) (Owner, error) {
	if !a.names.mayAsk(id) {
		return Owner{}, errAskedRecently
	}
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

// mayAsk records that faucet asks about the account id now, unless it did so
// less than accountAskInterval ago.
func (g *githubNames) mayAsk(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	maps.DeleteFunc(g.asked, func(_ int64, at time.Time) bool { return now.Sub(at) >= accountAskInterval })
	if _, ok := g.asked[id]; ok {
		return false
	}
	g.asked[id] = now
	return true
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

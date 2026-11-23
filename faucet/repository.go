package faucet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

// ErrUnknownRepository means the store has seen no delivery that names the
// repository.
var ErrUnknownRepository = xerrors.New("no delivery has named the repository")

// ErrUnknownOwner means the store has seen no delivery that names a
// repository of the owner.
var ErrUnknownOwner = xerrors.New("no delivery has named a repository of the owner")

// ErrNameBusy means that a store did not get the lock of an account or a
// repository within Lookup.Wait, because another delivery was asking GitHub
// about it.
var ErrNameBusy = xerrors.New("another delivery is asking GitHub about the account or the repository")

// Lookup is a question to GitHub about the names of an account or a
// repository now. A store calls Ask while it holds the lock of the account or
// the repository, and waits at most Wait for the lock, so that the step stays
// within the budget of the delivery's lookups.
type Lookup[T any] struct {
	Wait time.Duration
	Ask  func() (T, error)
}

// ErrQuarantinedOwner means that a delivery named the account under a login
// that GitHub did not confirm, so the account holds no login until GitHub
// gives one.
var ErrQuarantinedOwner = xerrors.New("GitHub has not confirmed the login of the owner")

// Owner is the user or organization that owns a repository, with the login
// that GitHub gave it latest as far as the store knows. GitHub keeps ID when
// the account is renamed, and Login can later name another account.
type Owner struct {
	ID    int64
	Login string
}

// Validate reports whether a store may keep o.
func (o Owner) Validate() error {
	switch {
	case o.ID <= 0:
		return xerrors.New("an owner needs a positive ID")
	case o.Login == "":
		return xerrors.New("an owner needs a login")
	case strings.ContainsAny(o.Login, "/\x00"):
		return xerrors.New("an owner's login holds no slash and no NUL byte")
	}
	return nil
}

// Repository is a GitHub repository as a signed delivery names it. GitHub
// keeps ID when the repository is renamed or moves to another owner, and
// FullName, written owner/name, can later name another repository.
type Repository struct {
	ID         int64
	OwnerID    int64
	OwnerLogin string
	FullName   string
}

// Validate reports whether a store may keep r.
func (r Repository) Validate() error {
	owner, name, ok := strings.Cut(r.FullName, "/")
	switch {
	case r.ID <= 0 || r.OwnerID <= 0:
		return xerrors.New("a repository needs a positive ID and owner ID")
	case r.OwnerLogin == "":
		return xerrors.New("a repository needs its owner's login")
	case !ok || owner == "" || name == "" || strings.Contains(name, "/"):
		return xerrors.New("a repository's full name is written owner/name")
	case strings.ContainsRune(r.FullName+r.OwnerLogin, 0):
		return xerrors.New("a repository's names hold no NUL byte")
	}
	return nil
}

// RepositoryRename is a repository event that renamed a repository or
// transferred it to another owner. Repo holds its names after the event, and
// From its full name before, as GitHub wrote it.
type RepositoryRename struct {
	Repo Repository
	From string
	// Body is the SHA-256 of the delivery's signed body. GitHub signs the
	// body alone, so a replay of it under another delivery GUID has the same
	// Body.
	Body [sha256.Size]byte
	// Late is true for a delivery that faucet's redelivery asked GitHub to
	// send again. It can come after a newer rename of the repository.
	Late bool
}

// RenameOutcome is what Store.RenameRepository did with a rename.
type RenameOutcome int

const (
	// Renamed means the repository took the rename's names: the store held
	// the name that it renamed the repository from, or no row of it.
	Renamed RenameOutcome = iota + 1
	// RenameQuarantined means the store held another name: a rename before
	// this one never reached it, or this one is an old body sent again, and
	// the store cannot tell which. The repository resolves no name until
	// GitHub names it (RenameConfirmed).
	RenameQuarantined
	// RenameLate means the rename was late and the store held another name,
	// which a newer rename can have given, so it changed nothing.
	RenameLate
	// RenameSeenBefore means the store had judged a rename with the same body
	// before, so it changed nothing.
	RenameSeenBefore
	// RenameConfirmed means that GitHub named the repository, and the store
	// took GitHub's names.
	RenameConfirmed
)

// recordRepository records the repository and the organization that a
// signed delivery names, so a service can be granted them by name before
// faucet stores an event of them. GitHub signs the body of a delivery and
// nothing else, and gives it no time, so a body that someone kept and sends
// again under a new delivery GUID cannot be told from a fresh one. A delivery
// therefore adds what the store lacks, but it changes a name that the store
// holds only to the one that GitHub, asked for the ID, gives now (recordOwner,
// renameRepository). A failed write of a rename or transfer fails the
// delivery, so that GitHub records the failure and redelivery asks for it
// again. Any other failed write is logged, and the delivery goes on: the next
// one that names the accounts writes them.
func (s *Service) recordRepository(ctx context.Context, logger slog.Logger, eventName, deliveryID string, payload []byte) error {
	named, hasRepo := repositoryOf(eventName, payload)
	var owners []Owner
	if org, ok := organizationOf(payload); ok {
		owners = append(owners, org)
	}
	if hasRepo && !slices.ContainsFunc(owners, func(o Owner) bool { return o.ID == named.repo.OwnerID }) {
		owners = append(owners, Owner{ID: named.repo.OwnerID, Login: named.repo.OwnerLogin})
	}
	asks := s.names.asks(ctx)
	defer asks.done()
	for _, owner := range owners {
		s.recordOwner(ctx, logger, owner, asks)
	}
	switch {
	case !hasRepo:
		return nil
	case named.previous == "":
		if err := s.store.RecordRepository(ctx, named.repo, s.clock.Now()); err != nil {
			logger.Warn(ctx, "could not record the repository", slog.Error(err))
		}
		return nil
	}
	return s.renameRepository(ctx, logger, deliveryID, RepositoryRename{Repo: named.repo, From: named.previous, Body: sha256.Sum256(payload)}, asks)
}

// renameRepository applies a repository event that renamed or transferred a
// repository, and the store asks GitHub once for each body that it has not
// judged. GitHub names a public repository, and the store takes GitHub's
// names, not the body's. GitHub names no private repository to a client with
// no token, and may not answer at all. Then the store judges the body by the
// name that it holds (Store.RenameRepository).
func (s *Service) renameRepository(ctx context.Context, logger slog.Logger, deliveryID string, rename RepositoryRename, asks *deliveryAsks) error {
	logger = logger.With(slog.F("repo_id", rename.Repo.ID))
	rename.Late = s.replayed(ctx, logger, deliveryID)
	lookup, release := lookupOf(asks, func() (Repository, error) { return asks.repository(rename.Repo.ID) })
	defer release()
	outcome, err := s.store.RenameRepository(ctx, rename, s.clock.Now(), Lookup[Repository]{Wait: lookup.Wait, Ask: func() (Repository, error) {
		current, err := lookup.Ask()
		switch {
		case err == nil:
		case xerrors.Is(err, errGitHubNotFound):
			logger.Info(ctx, "GitHub names no such public repository, so faucet judges the rename by the name that it holds")
		default:
			logger.Warn(ctx, "GitHub gave no names for the repository, so faucet judges the rename by the name that it holds", slog.Error(err))
		}
		return current, err
	}})
	if err != nil {
		return xerrors.Errorf("rename the repository: %w", err)
	}
	if outcome == RenameQuarantined {
		logger.Warn(ctx, "a rename or transfer of the repository never reached faucet, or an old one came again, so the repository resolves no name until GitHub names it",
			slog.F("from", rename.From), slog.F("full_name", rename.Repo.FullName))
	}
	return nil
}

// RedeliveryWants is the value that a host gives redelivery.Deps.Wants. It
// wants what RawWants wants and, when the service records repositories, every
// repository event that renamed or transferred a repository, so that
// redelivery asks again for one whose write failed.
func (s *Service) RedeliveryWants(ctx context.Context) (func(event, action string, repoID int64) bool, error) {
	raw, err := s.RawWants(ctx)
	return func(event, action string, repoID int64) bool {
		return s.recordRepos && renamesRepository(event, action) || raw(event, action, repoID)
	}, err
}

// renamesRepository reports whether a delivery of the event and action
// renamed or transferred a repository.
func renamesRepository(eventName, action string) bool {
	return eventName == "repository" && (action == "renamed" || action == "transferred")
}

// recordOwner adds an account that the store lacks under a login that no
// account holds or held, and writes nothing for an account under the login
// that the store holds for it, as nearly every delivery names it. Any other
// delivery would change what a name resolves to: GitHub renamed the account,
// or gave its login to another account, or the body is an old one sent
// again, or the account is quarantined. Then the store asks GitHub for the
// account and writes the login that GitHub gives it (Store.ConfirmOwner).
// When GitHub does not answer, neither login resolves until a later delivery
// of the account gets an answer. Faucet asks about one account at most once a
// minute, and a delivery within that minute gets no answer. A delivery that
// waited for another one's lookup of the account finds the login that GitHub
// gave, and asks nothing.
func (s *Service) recordOwner(ctx context.Context, logger slog.Logger, owner Owner, asks *deliveryAsks) {
	held, err := s.store.OwnerByID(ctx, owner.ID)
	switch {
	case err == nil && held.Login == owner.Login:
		return
	case xerrors.Is(err, ErrUnknownOwner):
		added, err := s.store.AddOwner(ctx, owner)
		if err != nil {
			logger.Warn(ctx, "could not record the owner", slog.Error(err))
		}
		if added || err != nil {
			return
		}
	case err != nil && !xerrors.Is(err, ErrQuarantinedOwner):
		logger.Warn(ctx, "could not read the owner", slog.Error(err))
		return
	}
	lookup, release := lookupOf(asks, func() (Owner, error) { return asks.account(owner.ID) })
	defer release()
	err = s.store.ConfirmOwner(ctx, owner, Lookup[Owner]{Wait: lookup.Wait, Ask: func() (Owner, error) {
		current, err := lookup.Ask()
		if err != nil {
			logger.Warn(ctx, "GitHub did not give the account a login, so it and the account that holds the delivered login hold none until GitHub gives one",
				slog.F("owner_id", owner.ID), slog.Error(err))
		}
		return current, err
	}})
	switch {
	case xerrors.Is(err, ErrNameBusy):
		logger.Info(ctx, "another delivery is asking GitHub about the account, so this one changes nothing", slog.F("owner_id", owner.ID))
	case err != nil:
		logger.Warn(ctx, "could not record the owner", slog.Error(err))
	}
}

// replayed reports whether redelivery asked GitHub to send the delivery of
// deliveryID again. When it cannot tell, it says yes: a late rename moves a
// name only from the name that the store holds, and quarantines nothing.
func (s *Service) replayed(ctx context.Context, logger slog.Logger, deliveryID string) bool {
	replay, err := s.redelivered(ctx, deliveryID)
	if err != nil {
		logger.Warn(ctx, "could not tell whether the delivery is a redelivery, so it is taken as one", slog.Error(err))
		return true
	}
	return replay
}

// organizationOf returns the organization that a delivery's payload names,
// as an organization's own events and the events of its repositories do,
// and false when it names none or names it in a form that Validate refuses.
func organizationOf(payload []byte) (Owner, bool) {
	var body struct {
		Organization *struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Organization == nil {
		return Owner{}, false
	}
	org := Owner{ID: body.Organization.ID, Login: body.Organization.Login}
	return org, org.Validate() == nil
}

// namedRepository is the repository that a delivery names, and the full
// name that it had before when the delivery renamed or transferred it.
type namedRepository struct {
	repo     Repository
	previous string
}

// repositoryOf returns the repository that a delivery's payload names, and
// false when it names none or names it in a form that Validate refuses. The
// previous name comes from the changes of a repository event that renamed
// it (changes.repository.name.from) or transferred it (changes.owner.from),
// and is empty for any other delivery.
func repositoryOf(eventName string, payload []byte) (namedRepository, bool) {
	type login struct {
		Login string `json:"login"`
	}
	var body struct {
		Action     string `json:"action"`
		Repository *struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
			Owner    struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Changes struct {
			Repository struct {
				Name struct {
					From string `json:"from"`
				} `json:"name"`
			} `json:"repository"`
			Owner struct {
				From struct {
					User         *login `json:"user"`
					Organization *login `json:"organization"`
				} `json:"from"`
			} `json:"owner"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Repository == nil {
		return namedRepository{}, false
	}
	r := Repository{ID: body.Repository.ID, OwnerID: body.Repository.Owner.ID, OwnerLogin: body.Repository.Owner.Login, FullName: body.Repository.FullName}
	if r.Validate() != nil {
		return namedRepository{}, false
	}
	named := namedRepository{repo: r}
	if !renamesRepository(eventName, body.Action) {
		return named, true
	}
	owner, name, _ := strings.Cut(r.FullName, "/")
	switch from := body.Changes.Owner.From; {
	case body.Action == "renamed" && body.Changes.Repository.Name.From != "":
		named.previous = owner + "/" + body.Changes.Repository.Name.From
	case body.Action == "transferred" && from.Organization != nil && from.Organization.Login != "":
		named.previous = from.Organization.Login + "/" + name
	case body.Action == "transferred" && from.User != nil && from.User.Login != "":
		named.previous = from.User.Login + "/" + name
	}
	return named, true
}

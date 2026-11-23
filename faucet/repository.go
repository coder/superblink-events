package faucet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"

	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

// ErrUnknownRepository means the store has seen no delivery that names the
// repository.
var ErrUnknownRepository = xerrors.New("no delivery has named the repository")

// ErrUnknownOwner means the store has seen no delivery that names a
// repository of the owner.
var ErrUnknownOwner = xerrors.New("no delivery has named a repository of the owner")

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
	// GitHub names it (Store.ConfirmRepository).
	RenameQuarantined
	// RenameLate means the rename was late and the store held another name,
	// which a newer rename can have given, so it changed nothing.
	RenameLate
	// RenameSeenBefore means the store had judged a rename with the same body
	// before, so it changed nothing.
	RenameSeenBefore
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
// repository, and asks GitHub once for each body that the store has not
// judged. GitHub names a public repository, and the store takes GitHub's
// names, not the body's. GitHub names no private repository to a client with
// no token, and may not answer at all. Then the store judges the body by the
// name that it holds (Store.RenameRepository).
func (s *Service) renameRepository(ctx context.Context, logger slog.Logger, deliveryID string, rename RepositoryRename, asks *deliveryAsks) error {
	seen, err := s.store.RenameSeen(ctx, rename.Body)
	switch {
	case err != nil:
		return xerrors.Errorf("read whether the rename was judged: %w", err)
	case seen:
		return nil
	}
	logger = logger.With(slog.F("repo_id", rename.Repo.ID))
	current, err := asks.repository(rename.Repo.ID)
	if err == nil {
		s.recordGitHubOwner(ctx, logger, Owner{ID: current.OwnerID, Login: current.OwnerLogin})
		if err := s.store.ConfirmRepository(ctx, current, rename.Body, s.clock.Now()); err != nil {
			return xerrors.Errorf("record the names that GitHub gives the repository: %w", err)
		}
		return nil
	}
	if xerrors.Is(err, errGitHubNotFound) {
		logger.Info(ctx, "GitHub names no such public repository, so faucet judges the rename by the name that it holds")
	} else {
		logger.Warn(ctx, "GitHub gave no names for the repository, so faucet judges the rename by the name that it holds", slog.Error(err))
	}
	rename.Late = s.replayed(ctx, logger, deliveryID)
	outcome, err := s.store.RenameRepository(ctx, rename, s.clock.Now())
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
// account holds, and writes nothing for an account under the login that the
// store holds for it, as nearly every delivery names it. Any other login
// would change what a name resolves to: GitHub renamed the account, or gave
// its login to another account, or the body is an old one sent again. Then
// faucet asks GitHub for the account, at most once a minute, and writes the
// login that GitHub gives it. When GitHub does not answer, nothing changes,
// and a later delivery asks again.
func (s *Service) recordOwner(ctx context.Context, logger slog.Logger, owner Owner, asks *deliveryAsks) {
	held, err := s.store.OwnerByID(ctx, owner.ID)
	switch {
	case err == nil && held.Login == owner.Login:
		return
	case xerrors.Is(err, ErrUnknownOwner):
		if _, err = s.store.OwnerByLogin(ctx, owner.Login); xerrors.Is(err, ErrUnknownOwner) {
			if err := s.store.AddOwner(ctx, owner); err != nil {
				logger.Warn(ctx, "could not record the owner", slog.Error(err))
			}
			return
		}
	}
	if err != nil {
		logger.Warn(ctx, "could not read the owner", slog.Error(err))
		return
	}
	current, err := asks.account(owner.ID)
	if err != nil {
		logger.Info(ctx, "GitHub did not give the account a login, so faucet keeps the logins that it holds",
			slog.F("owner_id", owner.ID), slog.Error(err))
		return
	}
	s.recordGitHubOwner(ctx, logger, current)
}

// recordGitHubOwner writes the login that GitHub gives the account, unless
// the store holds it.
func (s *Service) recordGitHubOwner(ctx context.Context, logger slog.Logger, owner Owner) {
	if held, err := s.store.OwnerByID(ctx, owner.ID); err == nil && held.Login == owner.Login {
		return
	}
	if err := s.store.RecordOwner(ctx, owner); err != nil {
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

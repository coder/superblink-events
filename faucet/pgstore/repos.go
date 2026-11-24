package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/pgstore/database"
)

// RecordRepository implements faucet.Store. A row of the same ID makes it
// write nothing.
func (s *Store) RecordRepository(ctx context.Context, repo faucet.Repository, seenAt time.Time) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := repo.Validate(); err != nil {
		return xerrors.Errorf("validate repository: %w", err)
	}
	if err := s.q.RecordRepository(ctx, database.RecordRepositoryParams{
		RepoID: repo.ID, OwnerID: repo.OwnerID, OwnerLogin: repo.OwnerLogin, FullName: repo.FullName, SeenAt: seenAt,
	}); err != nil {
		return xerrors.Errorf("record repository: %w", err)
	}
	return nil
}

// RenameRepository implements faucet.Store. The row of the body, which comes
// first, makes a concurrent call with the same body wait, and then find it.
// The lock of the repository orders concurrent calls for it, and holds one
// connection while lookup.Ask runs.
func (s *Store) RenameRepository(ctx context.Context, rename faucet.RepositoryRename, seenAt time.Time, lookup faucet.Lookup[faucet.Repository]) (_ faucet.RenameOutcome, err error) {
	defer func() { err = endedBy(ctx, err) }()
	repo := rename.Repo
	if err := repo.Validate(); err != nil {
		return 0, xerrors.Errorf("validate repository: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	recorded, err := q.RecordRepositoryRename(ctx, database.RecordRepositoryRenameParams{BodySha256: rename.Body[:], RepoID: repo.ID, SeenAt: seenAt})
	switch {
	case err != nil:
		return 0, xerrors.Errorf("record the body of the rename: %w", err)
	case recorded == 0:
		return faucet.RenameSeenBefore, nil
	}
	if err := lockName(ctx, tx, q, repositoryLock(repo.ID), max(lookup.Wait, time.Millisecond)); err != nil {
		return 0, xerrors.Errorf("lock repository: %w", err)
	}
	outcome, err := renameLocked(ctx, q, rename, seenAt, lookup.Ask)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, xerrors.Errorf("commit: %w", err)
	}
	return outcome, nil
}

func renameLocked(ctx context.Context, q *database.Queries, rename faucet.RepositoryRename, seenAt time.Time, ask func() (faucet.Repository, error)) (faucet.RenameOutcome, error) {
	current, err := ask()
	answered := err == nil && current.ID == rename.Repo.ID && current.Validate() == nil
	if answered && strings.EqualFold(current.FullName, rename.From) && !strings.EqualFold(current.FullName, rename.Repo.FullName) {
		return quarantineAnswered(ctx, q, current, seenAt)
	}
	if answered {
		if err := recordOwner(ctx, q, faucet.Owner{ID: current.OwnerID, Login: current.OwnerLogin}); err != nil {
			return 0, err
		}
		if err := q.RenameRepository(ctx, database.RenameRepositoryParams{
			RepoID: current.ID, OwnerID: current.OwnerID, OwnerLogin: current.OwnerLogin, FullName: current.FullName, SeenAt: seenAt,
		}); err != nil {
			return 0, xerrors.Errorf("rename repository: %w", err)
		}
		return faucet.RenameConfirmed, nil
	}
	repo := rename.Repo
	held, err := q.LockRepositoryHolding(ctx, database.LockRepositoryHoldingParams{RepoID: repo.ID, FromFullName: rename.From})
	outcome := faucet.Renamed
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, xerrors.Errorf("read repository: %w", err)
	case rename.Late && (held.Quarantined || !held.HoldsFrom):
		outcome = faucet.RenameLate
	case held.Quarantined || !held.HoldsFrom:
		outcome = faucet.RenameQuarantined
	}
	switch outcome {
	case faucet.Renamed:
		if err := q.RenameRepository(ctx, database.RenameRepositoryParams{
			RepoID: repo.ID, OwnerID: repo.OwnerID, OwnerLogin: repo.OwnerLogin, FullName: repo.FullName, SeenAt: seenAt,
		}); err != nil {
			return 0, xerrors.Errorf("rename repository: %w", err)
		}
	case faucet.RenameQuarantined:
		if err := q.QuarantineRepository(ctx, database.QuarantineRepositoryParams{
			RepoID: repo.ID, SeenAt: sql.NullTime{Time: seenAt, Valid: true},
		}); err != nil {
			return 0, xerrors.Errorf("quarantine repository: %w", err)
		}
	}
	return outcome, nil
}

// quarantineAnswered quarantines the repository of a rename that GitHub
// answered with the rename's from name, inserting a row of current when the
// store holds none. The body stays judged, so that sent again it asks GitHub
// nothing and can never fall back to the held-name rules.
func quarantineAnswered(ctx context.Context, q *database.Queries, current faucet.Repository, seenAt time.Time) (faucet.RenameOutcome, error) {
	if err := q.RecordRepository(ctx, database.RecordRepositoryParams{
		RepoID: current.ID, OwnerID: current.OwnerID, OwnerLogin: current.OwnerLogin, FullName: current.FullName, SeenAt: seenAt,
	}); err != nil {
		return 0, xerrors.Errorf("record repository: %w", err)
	}
	if err := q.QuarantineRepository(ctx, database.QuarantineRepositoryParams{
		RepoID: current.ID, SeenAt: sql.NullTime{Time: seenAt, Valid: true},
	}); err != nil {
		return 0, xerrors.Errorf("quarantine repository: %w", err)
	}
	return faucet.RenameQuarantined, nil
}

// lockName takes the lock of key for the transaction, waiting at most wait
// when wait is positive, and returns faucet.ErrNameBusy when another
// transaction holds it for longer.
func lockName(ctx context.Context, tx *sql.Tx, q *database.Queries, key string, wait time.Duration) error {
	if wait <= 0 {
		return q.LockName(ctx, key)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", max(wait.Milliseconds(), 1))); err != nil {
		return xerrors.Errorf("set the lock timeout: %w", err)
	}
	var pqErr *pq.Error
	switch err := q.LockName(ctx, key); {
	case xerrors.As(err, &pqErr) && pqErr.Code == lockNotAvailable:
		return faucet.ErrNameBusy
	case err != nil:
		return err
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout TO DEFAULT"); err != nil {
		return xerrors.Errorf("reset the lock timeout: %w", err)
	}
	return nil
}

// lockNotAvailable is Postgres's lock_not_available, which a lock wait past
// lock_timeout ends with.
const lockNotAvailable = "55P03"

func repositoryLock(id int64) string { return fmt.Sprintf("faucet_repos:%d", id) }

func ownerLock(id int64) string { return fmt.Sprintf("faucet_owners:%d", id) }

// RecordOwner implements faucet.Store. Another account that held the login
// gives it up first, in the same transaction, so the unique login of
// faucet_owners never refuses the write.
func (s *Store) RecordOwner(ctx context.Context, owner faucet.Owner) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := owner.Validate(); err != nil {
		return xerrors.Errorf("validate owner: %w", err)
	}
	return s.inOwnerLock(ctx, owner.ID, 0, func(q *database.Queries) error { return recordOwner(ctx, q, owner) })
}

// ConfirmOwner implements faucet.Store. The lock of the account holds one
// connection while lookup.Ask runs.
func (s *Store) ConfirmOwner(ctx context.Context, owner faucet.Owner, lookup faucet.Lookup[faucet.Owner]) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := owner.Validate(); err != nil {
		return xerrors.Errorf("validate owner: %w", err)
	}
	return s.inOwnerLock(ctx, owner.ID, max(lookup.Wait, time.Millisecond), func(q *database.Queries) error {
		if held, err := q.GetOwnerByID(ctx, owner.ID); err == nil && !held.Quarantined && held.Login == owner.Login {
			return nil
		}
		if current, err := lookup.Ask(); err == nil && current.ID == owner.ID && current.Validate() == nil {
			return recordOwner(ctx, q, current)
		}
		if err := q.QuarantineOwner(ctx, database.QuarantineOwnerParams{OwnerID: owner.ID, Login: owner.Login}); err != nil {
			return xerrors.Errorf("quarantine owner: %w", err)
		}
		return nil
	})
}

// inOwnerLock runs fn in a transaction that holds the lock of the account,
// waiting for the lock as lockName does.
func (s *Store) inOwnerLock(ctx context.Context, id int64, wait time.Duration, fn func(*database.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if err := lockName(ctx, tx, q, ownerLock(id), wait); err != nil {
		return xerrors.Errorf("lock owner: %w", err)
	}
	if err := fn(q); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return xerrors.Errorf("commit: %w", err)
	}
	return nil
}

// recordOwner takes the lock of the account, which the caller may hold
// already, since the lock is the transaction's.
func recordOwner(ctx context.Context, q *database.Queries, owner faucet.Owner) error {
	if err := q.LockName(ctx, ownerLock(owner.ID)); err != nil {
		return xerrors.Errorf("lock owner: %w", err)
	}
	if err := q.ReleaseOwnerLogin(ctx, database.ReleaseOwnerLoginParams{Login: owner.Login, OwnerID: owner.ID}); err != nil {
		return xerrors.Errorf("release the login of other owners: %w", err)
	}
	if err := q.RecordOwner(ctx, database.RecordOwnerParams{OwnerID: owner.ID, Login: owner.Login}); err != nil {
		return xerrors.Errorf("record owner: %w", err)
	}
	return nil
}

// AddOwner implements faucet.Store.
func (s *Store) AddOwner(ctx context.Context, owner faucet.Owner) (_ bool, err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := owner.Validate(); err != nil {
		return false, xerrors.Errorf("validate owner: %w", err)
	}
	added, err := s.q.AddOwner(ctx, database.AddOwnerParams{OwnerID: owner.ID, Login: owner.Login})
	if err != nil {
		return false, xerrors.Errorf("add owner: %w", err)
	}
	return added == 1, nil
}

// RepositoryByName implements faucet.Store.
func (s *Store) RepositoryByName(ctx context.Context, fullName string) (_ faucet.Repository, err error) {
	defer func() { err = endedBy(ctx, err) }()
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return faucet.Repository{}, faucet.ErrUnknownRepository
	}
	row, err := s.q.GetRepositoryByName(ctx, database.GetRepositoryByNameParams{OwnerLogin: owner, Name: name})
	switch {
	case xerrors.Is(err, sql.ErrNoRows), err == nil && row.Quarantined:
		return faucet.Repository{}, faucet.ErrUnknownRepository
	case err != nil:
		return faucet.Repository{}, xerrors.Errorf("read repository: %w", err)
	}
	return faucet.Repository{ID: row.RepoID, OwnerID: row.OwnerID, OwnerLogin: row.OwnerLogin, FullName: row.FullName}, nil
}

// OwnerByLogin implements faucet.Store.
func (s *Store) OwnerByLogin(ctx context.Context, login string) (_ faucet.Owner, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetOwnerByLogin(ctx, login)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return faucet.Owner{}, faucet.ErrUnknownOwner
	case err != nil:
		return faucet.Owner{}, xerrors.Errorf("read owner: %w", err)
	}
	return faucet.Owner{ID: row.OwnerID, Login: row.Login}, nil
}

// OwnerByID implements faucet.Store.
func (s *Store) OwnerByID(ctx context.Context, id int64) (_ faucet.Owner, err error) {
	defer func() { err = endedBy(ctx, err) }()
	row, err := s.q.GetOwnerByID(ctx, id)
	switch {
	case xerrors.Is(err, sql.ErrNoRows):
		return faucet.Owner{}, faucet.ErrUnknownOwner
	case err != nil:
		return faucet.Owner{}, xerrors.Errorf("read owner: %w", err)
	case row.Quarantined:
		return faucet.Owner{ID: row.OwnerID, Login: row.Login}, faucet.ErrQuarantinedOwner
	}
	return faucet.Owner{ID: row.OwnerID, Login: row.Login}, nil
}

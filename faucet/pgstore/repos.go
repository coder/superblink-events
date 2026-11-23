package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/pgstore/database"
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
// connection while ask runs.
func (s *Store) RenameRepository(ctx context.Context, rename faucet.RepositoryRename, seenAt time.Time, ask func() (faucet.Repository, error)) (_ faucet.RenameOutcome, err error) {
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
	if err := q.LockName(ctx, repositoryLock(repo.ID)); err != nil {
		return 0, xerrors.Errorf("lock repository: %w", err)
	}
	outcome, err := renameLocked(ctx, q, rename, seenAt, ask)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, xerrors.Errorf("commit: %w", err)
	}
	return outcome, nil
}

func renameLocked(ctx context.Context, q *database.Queries, rename faucet.RepositoryRename, seenAt time.Time, ask func() (faucet.Repository, error)) (faucet.RenameOutcome, error) {
	if current, err := ask(); err == nil && current.ID == rename.Repo.ID && current.Validate() == nil {
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
	return s.inOwnerLock(ctx, owner.ID, func(q *database.Queries) error { return recordOwner(ctx, q, owner) })
}

// ConfirmOwner implements faucet.Store. The lock of the account holds one
// connection while ask runs.
func (s *Store) ConfirmOwner(ctx context.Context, owner faucet.Owner, ask func() (faucet.Owner, error)) (err error) {
	defer func() { err = endedBy(ctx, err) }()
	if err := owner.Validate(); err != nil {
		return xerrors.Errorf("validate owner: %w", err)
	}
	return s.inOwnerLock(ctx, owner.ID, func(q *database.Queries) error {
		if current, err := ask(); err == nil && current.ID == owner.ID && current.Validate() == nil {
			return recordOwner(ctx, q, current)
		}
		if err := q.QuarantineOwner(ctx, database.QuarantineOwnerParams{OwnerID: owner.ID, Login: owner.Login}); err != nil {
			return xerrors.Errorf("quarantine owner: %w", err)
		}
		return nil
	})
}

// inOwnerLock runs fn in a transaction that holds the lock of the account.
func (s *Store) inOwnerLock(ctx context.Context, id int64, fn func(*database.Queries) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return xerrors.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if err := q.LockName(ctx, ownerLock(id)); err != nil {
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

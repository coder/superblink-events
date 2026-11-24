// Package pgtest gives a test the Postgres server for tests and its own
// database there. It sits at the module root so that cmd/faucet's tests take
// it as faucet's do.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// URL returns the URL of the Postgres server for tests, which
// FAUCET_TEST_DATABASE_URL names. Without the variable the test fails, on a
// laptop as in CI: a skipped test reads as a pass, so a change that breaks
// the store would look green. go test -short skips the test instead.
func URL(t testing.TB) string {
	t.Helper()
	serverURL := os.Getenv("FAUCET_TEST_DATABASE_URL")
	if serverURL == "" {
		if testing.Short() {
			t.Skip("-short skips the tests that need Postgres")
		}
		t.Fatal("set FAUCET_TEST_DATABASE_URL to a Postgres 13 or newer, for example postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable, or run go test -short to skip the tests that need it")
	}
	return serverURL
}

// NewDB creates a database on the server that URL names, runs migrate on it
// and drops it when the test ends.
func NewDB(t *testing.T, migrate func(context.Context, *sql.DB) error) *sql.DB {
	t.Helper()
	adminURL := URL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(t, err)
	name := "faucet_test_" + hex.EncodeToString(suffix)
	admin, err := sql.Open("postgres", adminURL)
	require.NoError(t, err)
	// The handle creates the database and drops it, and keeps no connection
	// in between. One idle connection for each test that runs can pass the
	// server's 100 connections.
	admin.SetMaxIdleConns(0)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	u, err := url.Parse(adminURL)
	require.NoError(t, err)
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		_, err := admin.ExecContext(context.Background(), "DROP DATABASE "+pq.QuoteIdentifier(name)+" WITH (FORCE)")
		require.NoError(t, err)
	})
	require.NoError(t, migrate(ctx, db))
	return db
}

// Package testutil provides helpers shared by integration tests.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenos/internal/store"
)

// DB returns a migrated store living in its own throwaway schema inside the
// database named by XENOS_TEST_DATABASE_URL, so tests in different packages can
// run in parallel and nothing in `public` is touched. It skips when the
// variable is unset and refuses any URL that does not contain "test".
func DB(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("XENOS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("XENOS_TEST_DATABASE_URL not set")
	}
	if !strings.Contains(url, "test") {
		t.Fatal("refusing to run against a database whose URL lacks 'test'")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	st, err := store.Open(ctx, url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

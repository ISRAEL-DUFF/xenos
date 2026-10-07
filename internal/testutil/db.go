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

	"github.com/israel-duff/xenos/internal/jobs"
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

// DrainJobs runs queued jobs through handlers until none are ready, the way a
// worker would, and returns how many ran. A failing job is recorded as failed
// or requeued exactly as in production, and the loop stops after maxRounds so a
// permanently retrying job cannot hang a test.
func DrainJobs(t *testing.T, st *store.Store, handlers map[string]jobs.Handler) int {
	t.Helper()
	ctx := context.Background()
	q := jobs.New(st.Pool)
	ran := 0
	for i := 0; i < 100; i++ {
		j, err := q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			return ran
		}
		ran++
		h, ok := handlers[j.Kind]
		if !ok {
			t.Fatalf("no handler for job kind %q", j.Kind)
		}
		if err := h(ctx, j); err != nil {
			t.Logf("job %s failed: %v", j.Kind, err)
			if err := q.Fail(ctx, j, err); err != nil {
				t.Fatal(err)
			}
			// Make a requeued job ready again so the test can drive retries.
			_, _ = st.Pool.Exec(ctx, `UPDATE jobs SET run_after = now() WHERE status = 'queued'`)
			continue
		}
		if err := q.Done(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("DrainJobs: jobs did not settle")
	return ran
}

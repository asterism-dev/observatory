// Command accounts-compare compares every permission decision of both models on a (copied) database; it only reads.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/accounts"
	"github.com/asterism-dev/observatory/internal/store"
)

const accountsMigration = 33

func main() {
	dbURL := flag.String("db", os.Getenv("OBSERVATORY_DATABASE_URL"), "database URL")
	flag.Parse()
	code, err := run(context.Background(), *dbURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "accounts-compare:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(ctx context.Context, dbURL string) (int, error) {
	if dbURL == "" {
		return 0, fmt.Errorf("no database: pass -db or set OBSERVATORY_DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return 0, err
	}
	defer pool.Close()
	var version int64
	if err := pool.QueryRow(ctx, "SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied").Scan(&version); err != nil {
		return 0, err
	}
	if version < accountsMigration {
		return 0, fmt.Errorf("database is at migration %d; the accounts migration (%d) must be applied first", version, accountsMigration)
	}
	s := store.New(pool)
	p, err := s.PolicySnapshot(ctx)
	if err != nil {
		return 0, err
	}
	snap, err := s.AccountSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	rep, err := accounts.Compare(p, snap)
	if err != nil {
		return 0, err
	}
	fmt.Printf("same: %d\nskipped: %d\nregressions: %d\n", rep.Same, rep.Skipped, len(rep.Regressions))
	actions := make([]string, 0, len(rep.Widenings))
	for a := range rep.Widenings {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		fmt.Printf("widening %s: %d\n", a, rep.Widenings[a])
	}
	for _, c := range rep.Collapsed {
		fmt.Printf("COLLAPSED (review manually) %s\n", c)
	}
	for _, d := range rep.Regressions {
		fmt.Printf("REGRESSION user=%s org=%s object=%s action=%s reason=%s\n", d.User, d.Org, d.Object, d.Action, d.Reason)
	}
	if len(rep.Regressions) > 0 {
		return 1, nil
	}
	return 0, nil
}

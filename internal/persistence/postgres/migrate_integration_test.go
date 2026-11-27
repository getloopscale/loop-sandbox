//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
	"github.com/getloopscale/loop-sandbox/migrations"
)

// The migration tests share one PostgreSQL server and get a fresh database each: a
// server takes seconds to start, a database milliseconds, and a database of its own
// keeps every test independent of the others.
var (
	sharedServerOnce sync.Once
	sharedServerDSN  string
	sharedServerErr  error
	databaseSeq      atomic.Int64
)

// sharedServer starts the server on first use. testcontainers' reaper removes the
// container when the test process exits.
func sharedServer(t *testing.T) string {
	t.Helper()
	sharedServerOnce.Do(func() {
		ctx := context.Background()
		container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.WithUsername("test"),
			tcpostgres.WithPassword("test"),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			sharedServerErr = fmt.Errorf("start postgres container: %w", err)
			return
		}
		sharedServerDSN, sharedServerErr = container.ConnectionString(ctx, "sslmode=disable")
	})
	if sharedServerErr != nil {
		t.Fatalf("shared postgres: %v", sharedServerErr)
	}
	return sharedServerDSN
}

// emptyPostgres returns a pool on a new database with no schema applied.
func emptyPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	serverDSN := sharedServer(t)
	admin, err := pgx.Connect(ctx, serverDSN)
	if err != nil {
		t.Fatalf("connect to the shared server: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	name := fmt.Sprintf("migrate_test_%d", databaseSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	dsn, err := url.Parse(serverDSN)
	if err != nil {
		t.Fatalf("parse server dsn: %v", err)
	}
	dsn.Path = "/" + name
	pool, err := pgxpool.New(ctx, dsn.String())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func embedded(t *testing.T) []migrations.Migration {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	return all
}

func recordedVersions(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

func TestMigrateAppliesEveryMigrationOnAnEmptyDatabase(t *testing.T) {
	pool := emptyPostgres(t)
	all := embedded(t)

	applied, err := postgres.Migrate(context.Background(), pool, all)

	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if applied != len(all) || recordedVersions(t, pool) != len(all) {
		t.Errorf("applied %d and recorded %d, want %d of each", applied, recordedVersions(t, pool), len(all))
	}
}

func TestMigrateAppliesNothingOnTheSecondRun(t *testing.T) {
	pool := emptyPostgres(t)
	all := embedded(t)
	if _, err := postgres.Migrate(context.Background(), pool, all); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	applied, err := postgres.Migrate(context.Background(), pool, all)

	if err != nil || applied != 0 {
		t.Errorf("second Migrate applied %d (err %v), want 0", applied, err)
	}
}

func TestMigrateAdoptsADatabaseCreatedBeforeVersionTracking(t *testing.T) {
	pool := emptyPostgres(t)
	all := embedded(t)
	for _, m := range all { // the old runner: every file applied, nothing recorded
		if _, err := pool.Exec(context.Background(), m.SQL); err != nil {
			t.Fatalf("pre-apply %s: %v", m.Name, err)
		}
	}

	applied, err := postgres.Migrate(context.Background(), pool, all)

	if err != nil {
		t.Fatalf("Migrate on a pre-existing schema: %v", err)
	}
	if applied != len(all) || recordedVersions(t, pool) != len(all) {
		t.Errorf("applied %d and recorded %d, want %d of each", applied, recordedVersions(t, pool), len(all))
	}
}

func TestMigrateRefusesAnAppliedMigrationThatWasEdited(t *testing.T) {
	pool := emptyPostgres(t)
	all := embedded(t)
	if _, err := postgres.Migrate(context.Background(), pool, all); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	edited := append([]migrations.Migration(nil), all...)
	edited[0].Checksum = "0000000000000000000000000000000000000000000000000000000000000000"

	_, err := postgres.Migrate(context.Background(), pool, edited)

	if !errors.Is(err, postgres.ErrMigrationChanged) {
		t.Errorf("Migrate = %v, want ErrMigrationChanged", err)
	}
}

func TestMigrateLeavesNoRecordWhenAMigrationFails(t *testing.T) {
	pool := emptyPostgres(t)
	broken := []migrations.Migration{{
		Version: "0001", Name: "0001_broken.sql", Checksum: "x",
		SQL: "CREATE TABLE half_applied (id INT); SELECT * FROM table_that_does_not_exist;",
	}}

	_, err := postgres.Migrate(context.Background(), pool, broken)

	if err == nil {
		t.Fatal("Migrate succeeded on a migration with invalid SQL")
	}
	if recordedVersions(t, pool) != 0 {
		t.Error("a failed migration left a record in schema_migrations")
	}
	var exists bool
	if qerr := pool.QueryRow(context.Background(), "SELECT to_regclass('half_applied') IS NOT NULL").Scan(&exists); qerr != nil {
		t.Fatalf("check half_applied: %v", qerr)
	}
	if exists {
		t.Error("the first statement of a failed migration was not rolled back")
	}
}

func TestMigrateFromConcurrentReplicasAppliesEachMigrationOnce(t *testing.T) {
	pool := emptyPostgres(t)
	all := embedded(t)
	const replicas = 4
	applied := make([]int, replicas)
	errs := make([]error, replicas)

	var wg sync.WaitGroup
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			applied[i], errs[i] = postgres.Migrate(context.Background(), pool, all)
		}(i)
	}
	wg.Wait()

	total := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("replica %d: %v", i, errs[i])
		}
		total += applied[i]
	}
	if total != len(all) {
		t.Errorf("replicas applied %d migrations in total, want %d", total, len(all))
	}
}

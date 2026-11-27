//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
)

func TestDeploymentIDIsTheSameOnEveryReadOfADatabaseAndDiffersBetweenDatabases(t *testing.T) {
	ctx := context.Background()
	one, other := postgres.New(setupPostgres(t)), postgres.New(setupPostgres(t))

	first, err := one.DeploymentID(ctx)
	again, err2 := one.DeploymentID(ctx)
	elsewhere, err3 := other.DeploymentID(ctx)

	if err != nil || err2 != nil || err3 != nil {
		t.Fatalf("DeploymentID: %v, %v, %v", err, err2, err3)
	}
	if first == "" || first != again || first == elsewhere {
		t.Errorf("ids: %q, %q and %q in another database; want the first two equal and the third different", first, again, elsewhere)
	}
}

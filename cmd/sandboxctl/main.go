// Command sandboxctl is the admin CLI: create tenants and API keys (M1).
//
// Usage:
//
//	sandboxctl tenants create --name <name> [--plan starter|pro|team]
//	sandboxctl api-keys create --tenant <tenant-id> [--name <name>] [--scopes s1,s2]
//
// It connects via DATABASE_URL and prints the raw API key exactly once.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/egress"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
	"github.com/getloopscale/loop-sandbox/internal/tenantprov"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	// `tenant provision` renders k8s manifests only — no DB needed; handle it first.
	if os.Args[1]+" "+os.Args[2] == "tenant provision" {
		provisionTenant(os.Args[3:])
		return
	}
	// `template build` runs the Firecracker builder — needs FC assets + KVM, not the DB.
	if os.Args[1]+" "+os.Args[2] == "template build" {
		templateBuild(os.Args[3:])
		return
	}

	ctx := context.Background()
	dsn := envOr("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/loopsandbox?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer pool.Close()
	store := postgres.New(pool)
	now := time.Now().UTC()

	switch os.Args[1] + " " + os.Args[2] {
	case "tenants create":
		fs := flag.NewFlagSet("tenants create", flag.ExitOnError)
		name := fs.String("name", "", "tenant name")
		planName := fs.String("plan", string(domain.PlanStarter), "plan that sets the tenant's quotas: starter, pro or team")
		_ = fs.Parse(os.Args[3:])
		if *name == "" {
			fatal("--name required")
		}
		tenantPlan := domain.Plan(*planName)
		if !domain.ValidPlan(tenantPlan) {
			fatal("unknown plan %q: use starter, pro or team", *planName)
		}
		id := "tnt_" + uuid.NewString()
		tn, err := domain.NewTenant(id, *name, tenantPlan, now)
		if err != nil {
			fatal("new tenant: %v", err)
		}
		if err := store.SaveTenant(ctx, tn); err != nil {
			fatal("save tenant: %v", err)
		}
		fmt.Printf("tenant created: %s (%s)\n", id, *name)

	case "api-keys create":
		fs := flag.NewFlagSet("api-keys create", flag.ExitOnError)
		tenant := fs.String("tenant", "", "tenant id")
		name := fs.String("name", "default", "key name")
		scopes := fs.String("scopes", domain.ScopeSandboxExecute,
			"comma-separated scopes: "+domain.ScopeSandboxExecute+" (use sandboxes), "+domain.ScopeAPIKeysManage+
				" (create and revoke keys through the API) and "+domain.ScopeSecretsManage+" (write and delete secrets)")
		_ = fs.Parse(os.Args[3:])
		if *tenant == "" {
			fatal("--tenant required")
		}
		if _, err := store.GetTenant(ctx, *tenant); err != nil {
			fatal("tenant %q not found: %v", *tenant, err)
		}
		keyScopes := strings.Split(*scopes, ",")
		// The operator at the database holds every scope, so only the shape is checked.
		everyScope := domain.AllScopes()
		if err := domain.ValidateAPIKeyRequest(*name, keyScopes, everyScope); err != nil {
			fatal("invalid key: %v", err)
		}
		raw, key, err := domain.GenerateAPIKey("key_"+uuid.NewString(), *tenant, *name, keyScopes, now)
		if err != nil {
			fatal("generate key: %v", err)
		}
		if err := store.SaveAPIKey(ctx, key); err != nil {
			fatal("save key: %v", err)
		}
		fmt.Printf("api key created for %s (shown once):\n%s\n", *tenant, raw)

	default:
		usage()
	}
}

// provisionTenant emits the per-tenant Kubernetes provisioning manifests (M10):
// namespace, RBAC, quota, and the deny-egress NetworkPolicy (blocks IMDS/private).
func provisionTenant(args []string) {
	fs := flag.NewFlagSet("tenant provision", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant id")
	extra := fs.String("extra-blocked", "", "comma-separated extra CIDRs to block (control-plane/workers)")
	_ = fs.Parse(args)
	if *tenant == "" {
		fatal("--tenant required")
	}
	var extraCIDRs []string
	if *extra != "" {
		extraCIDRs = strings.Split(*extra, ",")
	}
	pol, err := egress.New(extraCIDRs...)
	if err != nil {
		fatal("egress policy: %v", err)
	}
	manifests, err := tenantprov.Manifests(pol, tenantprov.Options{TenantID: *tenant})
	if err != nil {
		fatal("render manifests: %v", err)
	}
	fmt.Print(manifests)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sandboxctl tenants create --name <name> [--plan starter|pro|team] | sandboxctl api-keys create --tenant <id>")
	os.Exit(2)
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "sandboxctl: "+format+"\n", a...)
	os.Exit(1)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

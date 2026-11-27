package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/metering"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- TenantStore ---

func (s *Store) SaveTenant(ctx context.Context, t *domain.Tenant) error {
	const q = `INSERT INTO tenants (id, name, plan, status, cell_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, plan=EXCLUDED.plan, status=EXCLUDED.status`
	_, err := s.pool.Exec(ctx, q, t.ID, t.Name, string(t.Plan), string(t.Status), t.CellID, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("save tenant: %w", err)
	}
	return nil
}

func (s *Store) GetTenant(ctx context.Context, id string) (*domain.Tenant, error) {
	const q = `SELECT id, name, plan, status, cell_id, created_at FROM tenants WHERE id=$1`
	var t domain.Tenant
	var plan, status string
	err := s.pool.QueryRow(ctx, q, id).Scan(&t.ID, &t.Name, &plan, &status, &t.CellID, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get tenant: %w", err)
	}
	t.Plan, t.Status = domain.Plan(plan), domain.TenantStatus(status)
	return &t, nil
}

// --- APIKeyStore ---

func (s *Store) SaveAPIKey(ctx context.Context, k *domain.APIKey) error {
	const q = `INSERT INTO api_keys (id, tenant_id, name, prefix, key_hash, scopes, created_at, revoked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	_, err := s.pool.Exec(ctx, q, k.ID, k.TenantID, k.Name, k.Prefix, k.KeyHash, k.Scopes, k.CreatedAt, k.RevokedAt)
	if err != nil {
		return fmt.Errorf("save api key: %w", err)
	}
	return nil
}

// ResolveByRawKey hashes the presented key and looks it up. Returns ErrNotFound
// for unknown keys (the caller maps that to 401 without revealing which part failed).
func (s *Store) ResolveByRawKey(ctx context.Context, raw string) (*domain.APIKey, error) {
	hash := domain.HashAPIKey(raw)
	const q = `SELECT id, tenant_id, name, prefix, key_hash, scopes, created_at, revoked_at
		FROM api_keys WHERE key_hash=$1`
	var k domain.APIKey
	err := s.pool.QueryRow(ctx, q, hash).
		Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &k.KeyHash, &k.Scopes, &k.CreatedAt, &k.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve api key: %w", err)
	}
	return &k, nil
}

// Revoke marks a tenant's key as revoked. A key already revoked keeps its first
// revocation time; a key the tenant does not own is domain.ErrNotFound.
func (s *Store) Revoke(ctx context.Context, tenantID, id string, at time.Time) error {
	const q = `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, $3) WHERE tenant_id=$1 AND id=$2`
	tag, err := s.pool.Exec(ctx, q, tenantID, id, at)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ListAPIKeys returns a tenant's keys, revoked ones included, oldest first.
func (s *Store) ListAPIKeys(ctx context.Context, tenantID string) ([]*domain.APIKey, error) {
	const q = `SELECT id, tenant_id, name, prefix, key_hash, scopes, created_at, revoked_at
		FROM api_keys WHERE tenant_id=$1 ORDER BY created_at ASC, id ASC`
	rows, err := s.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var out []*domain.APIKey
	for rows.Next() {
		var k domain.APIKey
		if err := rows.Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &k.KeyHash, &k.Scopes, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("list api keys: scan: %w", err)
		}
		out = append(out, &k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return out, nil
}

// --- AuditStore ---

func (s *Store) Append(ctx context.Context, e *domain.AuditEvent) error {
	const q = `INSERT INTO audit_events
		(id, tenant_id, actor_type, actor_id, action, resource_type, resource_id, request_id, ts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	_, err := s.pool.Exec(ctx, q, e.ID, e.TenantID, e.ActorType, e.ActorID, e.Action,
		e.ResourceType, e.ResourceID, e.RequestID, e.Timestamp)
	if err != nil {
		return fmt.Errorf("append audit: %w", err)
	}
	return nil
}

// --- IdempotencyStore (reserve-before-act) ---

func (s *Store) Reserve(ctx context.Context, tenantID, key, requestHash string, nowUnix int64) (domain.ReserveResult, error) {
	// Atomic claim: INSERT wins the reservation; on conflict, inspect the holder.
	const ins = `INSERT INTO idempotency_keys (tenant_id, key, request_hash, created_at)
		VALUES ($1,$2,$3, to_timestamp($4))
		ON CONFLICT (tenant_id, key) DO NOTHING`
	tag, err := s.pool.Exec(ctx, ins, tenantID, key, requestHash, nowUnix)
	if err != nil {
		return domain.ReserveResult{}, fmt.Errorf("reserve idempotency: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return domain.ReserveResult{Outcome: domain.Claimed}, nil
	}
	// Someone else holds it — decide replay / conflict / in-progress.
	const sel = `SELECT request_hash, completed, response_status, response_body
		FROM idempotency_keys WHERE tenant_id=$1 AND key=$2`
	var storedHash string
	var completed bool
	var rec domain.IdempotencyRecord
	if err := s.pool.QueryRow(ctx, sel, tenantID, key).Scan(&storedHash, &completed, &rec.Status, &rec.Body); err != nil {
		return domain.ReserveResult{}, fmt.Errorf("inspect idempotency: %w", err)
	}
	if storedHash != requestHash {
		return domain.ReserveResult{Outcome: domain.Conflict}, nil
	}
	if !completed {
		return domain.ReserveResult{Outcome: domain.InProgress}, nil
	}
	rec.RequestHash = storedHash
	return domain.ReserveResult{Outcome: domain.Replay, Record: &rec}, nil
}

func (s *Store) Complete(ctx context.Context, tenantID, key string, status int, body []byte) error {
	const q = `UPDATE idempotency_keys SET completed=true, response_status=$3, response_body=$4
		WHERE tenant_id=$1 AND key=$2`
	if _, err := s.pool.Exec(ctx, q, tenantID, key, status, body); err != nil {
		return fmt.Errorf("complete idempotency: %w", err)
	}
	return nil
}

func (s *Store) Release(ctx context.Context, tenantID, key string) error {
	const q = `DELETE FROM idempotency_keys WHERE tenant_id=$1 AND key=$2 AND completed=false`
	if _, err := s.pool.Exec(ctx, q, tenantID, key); err != nil {
		return fmt.Errorf("release idempotency: %w", err)
	}
	return nil
}

// --- CostStore ---

func (s *Store) Increment(ctx context.Context, tenantID string, metric domain.CostMetric, n, periodStart, periodEnd int64) error {
	// Atomic outbox: bump the usage counter AND append a meter_event in ONE tx, so a
	// crash can never leave usage counted-but-unbilled (or vice-versa).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("increment begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	const incQ = `INSERT INTO cost_counters (tenant_id, metric, period_start, period_end, value)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, metric, period_start) DO UPDATE SET value = cost_counters.value + EXCLUDED.value`
	if _, err := tx.Exec(ctx, incQ, tenantID, string(metric), periodStart, periodEnd, n); err != nil {
		return fmt.Errorf("increment cost: %w", err)
	}
	period := time.Unix(periodStart, 0).UTC().Format("2006-01")
	if _, err := tx.Exec(ctx, `INSERT INTO meter_events (tenant_id, metric, amount, period) VALUES ($1,$2,$3,$4)`,
		tenantID, string(metric), n, period); err != nil {
		return fmt.Errorf("append meter event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("increment commit: %w", err)
	}
	return nil
}

// Drain implements metering.Outbox: it claims up to limit pending meter_events under
// a row lock (FOR UPDATE SKIP LOCKED — so concurrent drainers never claim the same
// row), emits them via sink, and DELETES the claimed rows in the SAME transaction —
// only when the emit succeeds. A sink error rolls the tx back (rows survive, retried
// later). A crash after the emit but before commit re-claims the rows on the next
// pass (at-least-once → the sink dedupes by EventID). The tx is held across the emit,
// so the sink MUST be bounded (batched + timeout); the drain runs off the hot path.
func (s *Store) Drain(ctx context.Context, limit int, sink metering.Sink) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT id, tenant_id, metric, amount, period FROM meter_events
		ORDER BY id ASC LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var events []metering.MeterEvent
	for rows.Next() {
		var e metering.MeterEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Metric, &e.Amount, &e.Period); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	if err := sink.Emit(ctx, metering.ToInputs(events)); err != nil {
		return 0, err // rollback → rows stay for the next pass
	}
	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	if _, err := tx.Exec(ctx, `DELETE FROM meter_events WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err // rows survive → re-claimed + re-emitted (sink dedupes)
	}
	return len(events), nil
}

func (s *Store) Value(ctx context.Context, tenantID string, metric domain.CostMetric, periodStart int64) (int64, error) {
	const q = `SELECT value FROM cost_counters WHERE tenant_id=$1 AND metric=$2 AND period_start=$3`
	var v int64
	err := s.pool.QueryRow(ctx, q, tenantID, string(metric), periodStart).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get cost: %w", err)
	}
	return v, nil
}

// --- ExecutionStore ---

func (s *Store) SaveExecution(ctx context.Context, e *domain.Execution) error {
	const q = `INSERT INTO executions (id, sandbox_id, tenant_id, status, created_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id, id) DO UPDATE SET status=EXCLUDED.status`
	if _, err := s.pool.Exec(ctx, q, e.ID, e.SandboxID, e.TenantID, e.Status, e.CreatedAt); err != nil {
		return fmt.Errorf("save execution: %w", err)
	}
	return nil
}

func (s *Store) GetExecution(ctx context.Context, tenantID, id string) (*domain.Execution, error) {
	const q = `SELECT id, sandbox_id, tenant_id, status, created_at FROM executions WHERE tenant_id=$1 AND id=$2`
	var e domain.Execution
	err := s.pool.QueryRow(ctx, q, tenantID, id).Scan(&e.ID, &e.SandboxID, &e.TenantID, &e.Status, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get execution: %w", err)
	}
	return &e, nil
}

// --- TemplateStore ---

func (s *Store) SaveTemplate(ctx context.Context, t *domain.Template) error {
	const q = `INSERT INTO templates (id, tenant_id, name, image_ref, version, digest, public, scan_status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, id) DO UPDATE SET name=EXCLUDED.name, image_ref=EXCLUDED.image_ref,
			version=EXCLUDED.version, digest=EXCLUDED.digest, public=EXCLUDED.public, scan_status=EXCLUDED.scan_status`
	if _, err := s.pool.Exec(ctx, q, t.ID, t.TenantID, t.Name, t.ImageRef, t.Version, t.Digest, t.Public, t.ScanStatus, t.CreatedAt); err != nil {
		return fmt.Errorf("save template: %w", err)
	}
	return nil
}

func (s *Store) GetTemplate(ctx context.Context, tenantID, id string) (*domain.Template, error) {
	// A template is visible to a tenant if it owns it OR it is public.
	const q = `SELECT id, tenant_id, name, image_ref, version, digest, public, scan_status, created_at
		FROM templates WHERE id=$2 AND (tenant_id=$1 OR public=TRUE)`
	var t domain.Template
	err := s.pool.QueryRow(ctx, q, tenantID, id).Scan(&t.ID, &t.TenantID, &t.Name, &t.ImageRef, &t.Version, &t.Digest, &t.Public, &t.ScanStatus, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get template: %w", err)
	}
	return &t, nil
}

func (s *Store) ListTemplates(ctx context.Context, tenantID string) ([]*domain.Template, error) {
	const q = `SELECT id, tenant_id, name, image_ref, version, digest, public, scan_status, created_at
		FROM templates WHERE tenant_id=$1 OR public=TRUE ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()
	var out []*domain.Template
	for rows.Next() {
		var t domain.Template
		if err := rows.Scan(&t.ID, &t.TenantID, &t.Name, &t.ImageRef, &t.Version, &t.Digest, &t.Public, &t.ScanStatus, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// --- VolumeStore (M16) ---

func (s *Store) SaveVolume(ctx context.Context, v *domain.Volume) error {
	const q = `INSERT INTO volumes (id, tenant_id, cell_id, name, size_gb, sandbox_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (tenant_id, id) DO UPDATE SET sandbox_id=EXCLUDED.sandbox_id, size_gb=EXCLUDED.size_gb`
	if _, err := s.pool.Exec(ctx, q, v.ID, v.TenantID, v.CellID, v.Name, v.SizeGB, v.SandboxID, v.CreatedAt); err != nil {
		return fmt.Errorf("save volume: %w", err)
	}
	return nil
}

func (s *Store) GetVolume(ctx context.Context, tenantID, id string) (*domain.Volume, error) {
	const q = `SELECT id, tenant_id, cell_id, name, size_gb, sandbox_id, created_at FROM volumes WHERE tenant_id=$1 AND id=$2`
	var v domain.Volume
	err := s.pool.QueryRow(ctx, q, tenantID, id).Scan(&v.ID, &v.TenantID, &v.CellID, &v.Name, &v.SizeGB, &v.SandboxID, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get volume: %w", err)
	}
	return &v, nil
}

func (s *Store) GetVolumeByName(ctx context.Context, tenantID, name string) (*domain.Volume, error) {
	const q = `SELECT id, tenant_id, cell_id, name, size_gb, sandbox_id, created_at FROM volumes WHERE tenant_id=$1 AND name=$2`
	var v domain.Volume
	err := s.pool.QueryRow(ctx, q, tenantID, name).Scan(&v.ID, &v.TenantID, &v.CellID, &v.Name, &v.SizeGB, &v.SandboxID, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get volume by name: %w", err)
	}
	return &v, nil
}

func (s *Store) ListVolumes(ctx context.Context, tenantID, sandboxID string) ([]*domain.Volume, error) {
	const q = `SELECT id, tenant_id, cell_id, name, size_gb, sandbox_id, created_at FROM volumes WHERE tenant_id=$1 AND sandbox_id=$2 ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q, tenantID, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	defer rows.Close()
	var out []*domain.Volume
	for rows.Next() {
		var v domain.Volume
		if err := rows.Scan(&v.ID, &v.TenantID, &v.CellID, &v.Name, &v.SizeGB, &v.SandboxID, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan volume: %w", err)
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

func (s *Store) TotalSizeGB(ctx context.Context, tenantID string) (int64, error) {
	const q = `SELECT COALESCE(SUM(size_gb),0) FROM volumes WHERE tenant_id=$1`
	var total int64
	if err := s.pool.QueryRow(ctx, q, tenantID).Scan(&total); err != nil {
		return 0, fmt.Errorf("total volume size: %w", err)
	}
	return total, nil
}

func (s *Store) DeleteVolume(ctx context.Context, tenantID, id string) error {
	const q = `DELETE FROM volumes WHERE tenant_id=$1 AND id=$2`
	if _, err := s.pool.Exec(ctx, q, tenantID, id); err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}
	return nil
}

// --- SnapshotStore ---

const snapshotColumns = `id, tenant_id, name, sandbox_id, image, metadata, env, size_bytes, created_at`

func scanSnapshot(row pgx.Row) (*domain.Snapshot, error) {
	var (
		snap          domain.Snapshot
		metadata, env []byte
	)
	if err := row.Scan(&snap.ID, &snap.TenantID, &snap.Name, &snap.SandboxID, &snap.Image, &metadata, &env, &snap.SizeBytes, &snap.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(metadata, &snap.Metadata); err != nil {
		return nil, fmt.Errorf("decode metadata of snapshot %s: %w", snap.ID, err)
	}
	if err := json.Unmarshal(env, &snap.Env); err != nil {
		return nil, fmt.Errorf("decode env of snapshot %s: %w", snap.ID, err)
	}
	return &snap, nil
}

// SaveSnapshot implements domain.SnapshotStore.
func (s *Store) SaveSnapshot(ctx context.Context, snap *domain.Snapshot) error {
	metadata, err := json.Marshal(nonNilMap(snap.Metadata))
	if err != nil {
		return fmt.Errorf("encode metadata of snapshot %s: %w", snap.ID, err)
	}
	env, err := json.Marshal(nonNilMap(snap.Env))
	if err != nil {
		return fmt.Errorf("encode env of snapshot %s: %w", snap.ID, err)
	}
	const q = `INSERT INTO snapshots (` + snapshotColumns + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	_, err = s.pool.Exec(ctx, q, snap.ID, snap.TenantID, snap.Name, snap.SandboxID, snap.Image, metadata, env, snap.SizeBytes, snap.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation: the name is taken
		return domain.ErrAlreadyExists
	}
	if err != nil {
		return fmt.Errorf("save snapshot %s: %w", snap.ID, err)
	}
	return nil
}

// GetSnapshot implements domain.SnapshotStore.
func (s *Store) GetSnapshot(ctx context.Context, tenantID, id string) (*domain.Snapshot, error) {
	const q = `SELECT ` + snapshotColumns + ` FROM snapshots WHERE tenant_id=$1 AND id=$2`
	snap, err := scanSnapshot(s.pool.QueryRow(ctx, q, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get snapshot %s: %w", id, err)
	}
	return snap, nil
}

// ListSnapshots implements domain.SnapshotStore.
func (s *Store) ListSnapshots(ctx context.Context, tenantID string) ([]*domain.Snapshot, error) {
	const q = `SELECT ` + snapshotColumns + ` FROM snapshots WHERE tenant_id=$1 ORDER BY created_at, id`
	rows, err := s.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer rows.Close()
	var out []*domain.Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// DeleteSnapshot implements domain.SnapshotStore.
func (s *Store) DeleteSnapshot(ctx context.Context, tenantID, id string) error {
	const q = `DELETE FROM snapshots WHERE tenant_id=$1 AND id=$2`
	if _, err := s.pool.Exec(ctx, q, tenantID, id); err != nil {
		return fmt.Errorf("delete snapshot %s: %w", id, err)
	}
	return nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

var _ domain.SnapshotStore = (*Store)(nil)

// --- SecretStore ---

// PutSecret implements domain.SecretStore. xmax is zero on a row the statement
// inserted and non-zero on one it updated.
func (s *Store) PutSecret(ctx context.Context, secret *domain.Secret) (bool, error) {
	const q = `INSERT INTO secrets (tenant_id, name, value, key_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (tenant_id, name) DO UPDATE
		SET value = EXCLUDED.value, key_id = EXCLUDED.key_id, updated_at = EXCLUDED.updated_at
		RETURNING (xmax = 0)`
	var created bool
	err := s.pool.QueryRow(ctx, q, secret.TenantID, secret.Name, secret.Ciphertext, secret.KeyID, secret.CreatedAt, secret.UpdatedAt).Scan(&created)
	if err != nil {
		return false, fmt.Errorf("put secret %s: %w", secret.Name, err)
	}
	return created, nil
}

const secretColumns = `tenant_id, name, value, key_id, created_at, updated_at`

func scanSecret(row pgx.Row) (*domain.Secret, error) {
	var secret domain.Secret
	if err := row.Scan(&secret.TenantID, &secret.Name, &secret.Ciphertext, &secret.KeyID, &secret.CreatedAt, &secret.UpdatedAt); err != nil {
		return nil, err
	}
	return &secret, nil
}

// GetSecret implements domain.SecretStore.
func (s *Store) GetSecret(ctx context.Context, tenantID, name string) (*domain.Secret, error) {
	const q = `SELECT ` + secretColumns + ` FROM secrets WHERE tenant_id=$1 AND name=$2`
	secret, err := scanSecret(s.pool.QueryRow(ctx, q, tenantID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get secret %s: %w", name, err)
	}
	return secret, nil
}

// ListSecrets implements domain.SecretStore.
func (s *Store) ListSecrets(ctx context.Context, tenantID string) ([]*domain.Secret, error) {
	const q = `SELECT ` + secretColumns + ` FROM secrets WHERE tenant_id=$1 ORDER BY name`
	rows, err := s.pool.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	var out []*domain.Secret
	for rows.Next() {
		secret, err := scanSecret(rows)
		if err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		out = append(out, secret)
	}
	return out, rows.Err()
}

// DeleteSecret implements domain.SecretStore.
func (s *Store) DeleteSecret(ctx context.Context, tenantID, name string) error {
	const q = `DELETE FROM secrets WHERE tenant_id=$1 AND name=$2`
	tag, err := s.pool.Exec(ctx, q, tenantID, name)
	if err != nil {
		return fmt.Errorf("delete secret %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

var _ domain.SecretStore = (*Store)(nil)

// DeploymentID returns the random id of this database, written once by a
// migration. Every process of a deployment reads the same value.
func (s *Store) DeploymentID(ctx context.Context) (string, error) {
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM deployment_identity`).Scan(&id); err != nil {
		return "", fmt.Errorf("read the deployment id: %w", err)
	}
	return id, nil
}

// Compile-time assertions that Store satisfies the M1 ports.
var (
	_ domain.TenantStore      = (*Store)(nil)
	_ domain.APIKeyStore      = (*Store)(nil)
	_ domain.AuditStore       = (*Store)(nil)
	_ domain.IdempotencyStore = (*Store)(nil)
	_ domain.CostStore        = (*Store)(nil)
	_ domain.ExecutionStore   = (*Store)(nil)
	_ domain.TemplateStore    = (*Store)(nil)
	_ domain.VolumeStore      = (*Store)(nil)
)

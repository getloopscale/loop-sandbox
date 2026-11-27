// Package postgres is the production Repository implementation over PostgreSQL
// (pgx). It satisfies domain.Repository; the domain never imports pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is a PostgreSQL-backed repository.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store over an existing pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// sandboxColumns is the column list every sandbox read selects, in scanSandbox order.
const sandboxColumns = `id, tenant_id, cell_id, image, gpu, state, created_at, updated_at, expires_at, metadata, env,
	auto_pause_seconds, last_active_at, auto_paused, secrets, network_allow_out, network_deny_out`

// rowScanner is the part of pgx.Row and pgx.Rows that scanSandbox needs.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSandbox(row rowScanner) (*domain.Sandbox, error) {
	var sb domain.Sandbox
	var (
		state        string
		autoPause    int64
		lastActiveAt *time.Time // NULL on rows written before the column existed
	)
	if err := row.Scan(&sb.ID, &sb.TenantID, &sb.CellID, &sb.Image, &sb.GPU, &state,
		&sb.CreatedAt, &sb.UpdatedAt, &sb.ExpiresAt, &sb.Metadata, &sb.Env,
		&autoPause, &lastActiveAt, &sb.AutoPaused, &sb.Secrets, &sb.NetworkAllowOut, &sb.NetworkDenyOut); err != nil {
		return nil, err
	}
	sb.State = domain.State(state)
	sb.AutoPauseAfter = time.Duration(autoPause) * time.Second
	if lastActiveAt != nil {
		sb.LastActiveAt = *lastActiveAt
	}
	// An absent attribute set reads back as nil, the same as it was before the save.
	if len(sb.Metadata) == 0 {
		sb.Metadata = nil
	}
	if len(sb.Env) == 0 {
		sb.Env = nil
	}
	if len(sb.Secrets) == 0 {
		sb.Secrets = nil
	}
	if len(sb.NetworkAllowOut) == 0 {
		sb.NetworkAllowOut = nil
	}
	if len(sb.NetworkDenyOut) == 0 {
		sb.NetworkDenyOut = nil
	}
	return &sb, nil
}

// jsonObject returns m as a value that encodes to a JSON object, never to null.
func jsonObject(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// jsonArray returns v as a value that encodes to a JSON array, never to null.
func jsonArray(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// SetSandboxNetwork implements domain.NetworkStore.
func (s *Store) SetSandboxNetwork(ctx context.Context, tenantID, id string, allowOut, denyOut []string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sandboxes SET network_allow_out = $3, network_deny_out = $4 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, jsonArray(allowOut), jsonArray(denyOut))
	if err != nil {
		return fmt.Errorf("set sandbox network: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Save upserts a sandbox. Metadata, env and the idle limit are written on insert
// only: they are immutable after creation, and a state-transition save must not
// rewrite them. The last activity only moves forward, so a save made from a copy
// read earlier cannot undo a touch recorded since.
func (s *Store) Save(ctx context.Context, sb *domain.Sandbox) error {
	cell := sb.CellID
	if cell == "" {
		cell = domain.DefaultCell // never write an empty partition key
	}
	// Conflict target is the partitioned PK (cell_id, tenant_id, id): a partitioned
	// table's unique constraint must include the partition key. A sandbox's cell is set
	// once at creation, so state-transition upserts match the same key.
	const q = `
		INSERT INTO sandboxes (id, tenant_id, cell_id, image, gpu, state, created_at, updated_at, expires_at, metadata, env,
			auto_pause_seconds, last_active_at, auto_paused, secrets, network_allow_out, network_deny_out)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (cell_id, tenant_id, id) DO UPDATE
		SET state = EXCLUDED.state, updated_at = EXCLUDED.updated_at, expires_at = EXCLUDED.expires_at,
			auto_paused = EXCLUDED.auto_paused,
			last_active_at = GREATEST(sandboxes.last_active_at, EXCLUDED.last_active_at)`
	var lastActiveAt *time.Time
	if !sb.LastActiveAt.IsZero() {
		lastActiveAt = &sb.LastActiveAt
	}
	_, err := s.pool.Exec(ctx, q, sb.ID, sb.TenantID, cell, sb.Image, sb.GPU, string(sb.State),
		sb.CreatedAt, sb.UpdatedAt, sb.ExpiresAt, jsonObject(sb.Metadata), jsonObject(sb.Env),
		int64(sb.AutoPauseAfter/time.Second), lastActiveAt, sb.AutoPaused, jsonArray(sb.Secrets),
		jsonArray(sb.NetworkAllowOut), jsonArray(sb.NetworkDenyOut))
	if err != nil {
		return fmt.Errorf("save sandbox: %w", err)
	}
	return nil
}

// TouchSandbox implements domain.ActivityStore.
func (s *Store) TouchSandbox(ctx context.Context, tenantID, id string, at time.Time) error {
	const q = `UPDATE sandboxes SET last_active_at = $3
		WHERE tenant_id = $1 AND id = $2 AND (last_active_at IS NULL OR last_active_at < $3)`
	if _, err := s.pool.Exec(ctx, q, tenantID, id, at); err != nil {
		return fmt.Errorf("touch sandbox %s: %w", id, err)
	}
	return nil
}

// ListIdle implements domain.ActivityStore.
func (s *Store) ListIdle(ctx context.Context, now time.Time, limit int) ([]*domain.Sandbox, error) {
	const q = `
		SELECT ` + sandboxColumns + `
		FROM sandboxes
		WHERE state = 'running' AND auto_pause_seconds > 0
		  AND last_active_at + make_interval(secs => auto_pause_seconds) <= $1
		ORDER BY last_active_at ASC LIMIT $2`
	rows, err := s.pool.Query(ctx, q, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list idle: %w", err)
	}
	return collectSandboxes(rows, "list idle")
}

var _ domain.ActivityStore = (*Store)(nil)

// ListExpired returns non-destroyed sandboxes past their TTL (reconciler work list).
func (s *Store) ListExpired(ctx context.Context, now time.Time, limit int) ([]*domain.Sandbox, error) {
	const q = `
		SELECT ` + sandboxColumns + `
		FROM sandboxes
		WHERE state <> 'destroyed' AND expires_at IS NOT NULL AND expires_at < $1
		ORDER BY expires_at ASC LIMIT $2`
	rows, err := s.pool.Query(ctx, q, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired: %w", err)
	}
	return collectSandboxes(rows, "list expired")
}

// List returns one page of a tenant's sandboxes, newest first. The tenant_id filter
// is mandatory, so another tenant's sandboxes can never appear.
func (s *Store) List(ctx context.Context, tenantID string, q domain.SandboxQuery) ([]*domain.Sandbox, error) {
	if q.Limit <= 0 {
		return nil, fmt.Errorf("list sandboxes: limit must be positive, got %d", q.Limit)
	}
	var states []string // nil means every state
	for _, st := range q.States {
		states = append(states, string(st))
	}
	var afterTime *time.Time
	afterID := ""
	if q.After != nil {
		afterTime, afterID = &q.After.CreatedAt, q.After.ID
	}
	const query = `
		SELECT ` + sandboxColumns + `
		FROM sandboxes
		WHERE tenant_id = $1
		  AND ($2::text[] IS NULL OR state = ANY($2))
		  AND metadata @> $3::jsonb
		  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5))
		ORDER BY created_at DESC, id DESC
		LIMIT $6`
	rows, err := s.pool.Query(ctx, query, tenantID, states, jsonObject(q.Metadata), afterTime, afterID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return collectSandboxes(rows, "list sandboxes")
}

func collectSandboxes(rows pgx.Rows, what string) ([]*domain.Sandbox, error) {
	defer rows.Close()
	var out []*domain.Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", what, err)
		}
		out = append(out, sb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// Get returns a tenant's sandbox or domain.ErrNotFound. The tenant_id filter is
// mandatory so cross-tenant reads are indistinguishable from not-found (US-05).
func (s *Store) Get(ctx context.Context, tenantID, id string) (*domain.Sandbox, error) {
	// (tenant_id, id) is globally unique by convention (cell_id immutable per sandbox;
	// see domain.Sandbox.CellID), so this matches at most one row. LIMIT 1 keeps the read
	// deterministic even if a rebalance bug ever stranded a duplicate across partitions.
	const q = `SELECT ` + sandboxColumns + ` FROM sandboxes WHERE tenant_id = $1 AND id = $2 LIMIT 1`
	sb, err := scanSandbox(s.pool.QueryRow(ctx, q, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	return sb, nil
}

// Delete removes a tenant's sandbox row (idempotent).
func (s *Store) Delete(ctx context.Context, tenantID, id string) error {
	const q = `DELETE FROM sandboxes WHERE tenant_id = $1 AND id = $2`
	if _, err := s.pool.Exec(ctx, q, tenantID, id); err != nil {
		return fmt.Errorf("delete sandbox: %w", err)
	}
	return nil
}

var _ domain.Repository = (*Store)(nil)

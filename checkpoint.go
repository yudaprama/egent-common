package egentcommon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const CheckpointTTL = 15 * time.Minute

type CheckpointOwner struct {
	TenantID string
	ActorID  string
	AgentID  string
}

type checkpointOwnerKey struct{}

func WithCheckpointOwner(ctx context.Context, owner CheckpointOwner) context.Context {
	return context.WithValue(ctx, checkpointOwnerKey{}, owner)
}

func CheckpointOwnerFromContext(ctx context.Context) (CheckpointOwner, bool) {
	owner, ok := ctx.Value(checkpointOwnerKey{}).(CheckpointOwner)
	return owner, ok && owner.ActorID != "" && owner.AgentID != ""
}

// PostgresCheckpointStore implements the Eino checkpoint store and persists
// interrupted agent state so any replica of the same egent can resume it.
type PostgresCheckpointStore struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// MemoryCheckpointStore is the local-development fallback when Postgres is
// unavailable. It has the same ownership contract as the durable store.
type MemoryCheckpointStore struct {
	mu     sync.Mutex
	ttl    time.Duration
	items  map[string]memoryCheckpoint
	owners map[string]CheckpointOwner
}

type memoryCheckpoint struct {
	data      []byte
	expiresAt time.Time
}

func NewMemoryCheckpointStore(ttl time.Duration) *MemoryCheckpointStore {
	return &MemoryCheckpointStore{ttl: ttl, items: make(map[string]memoryCheckpoint), owners: make(map[string]CheckpointOwner)}
}

func (s *MemoryCheckpointStore) BindOwner(id string, owner CheckpointOwner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners[id] = owner
}

func (s *MemoryCheckpointStore) OwnerMatches(_ context.Context, id string, owner CheckpointOwner) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bound, ok := s.owners[id]
	return ok && bound == owner, nil
}

func (s *MemoryCheckpointStore) Get(_ context.Context, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	if !ok || time.Now().After(item.expiresAt) {
		delete(s.items, id)
		delete(s.owners, id)
		return nil, false, nil
	}
	return append([]byte(nil), item.data...), true, nil
}

func (s *MemoryCheckpointStore) Set(_ context.Context, id string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = memoryCheckpoint{data: append([]byte(nil), data...), expiresAt: time.Now().Add(s.ttl)}
	return nil
}

func (s *MemoryCheckpointStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	delete(s.owners, id)
	return nil
}

func NewPostgresCheckpointStore(ctx context.Context, ttl time.Duration) (*PostgresCheckpointStore, func(), error) {
	dsn := os.Getenv("KAWAI_PG_DSN")
	if dsn == "" {
		return nil, func() {}, fmt.Errorf("KAWAI_PG_DSN is unset")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, func() {}, fmt.Errorf("create checkpoint pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, func() {}, fmt.Errorf("ping checkpoint database: %w", err)
	}
	return &PostgresCheckpointStore{pool: pool, ttl: ttl}, pool.Close, nil
}

func (s *PostgresCheckpointStore) Get(ctx context.Context, id string) ([]byte, bool, error) {
	owner, ok := CheckpointOwnerFromContext(ctx)
	if !ok {
		return nil, false, nil
	}
	var data []byte
	err := s.pool.QueryRow(ctx, `
		SELECT state
		FROM agent_checkpoints
		WHERE checkpoint_id = $1 AND tenant_id = $2 AND actor_id = $3 AND agent_id = $4
		  AND expires_at > now() AND consumed_at IS NULL`,
		id, owner.TenantID, owner.ActorID, owner.AgentID).Scan(&data)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (s *PostgresCheckpointStore) Set(ctx context.Context, id string, data []byte) error {
	owner, ok := CheckpointOwnerFromContext(ctx)
	if !ok {
		return fmt.Errorf("checkpoint owner is missing")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agent_checkpoints
			(checkpoint_id, tenant_id, actor_id, agent_id, state, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6::interval)
		ON CONFLICT (checkpoint_id) DO UPDATE SET
			state = EXCLUDED.state,
			expires_at = EXCLUDED.expires_at,
			updated_at = now()`,
		id, owner.TenantID, owner.ActorID, owner.AgentID, data, s.ttl.String())
	return err
}

func (s *PostgresCheckpointStore) Delete(ctx context.Context, id string) error {
	owner, ok := CheckpointOwnerFromContext(ctx)
	if !ok {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM agent_checkpoints
		WHERE checkpoint_id = $1 AND tenant_id = $2 AND actor_id = $3 AND agent_id = $4`,
		id, owner.TenantID, owner.ActorID, owner.AgentID)
	return err
}

func (s *PostgresCheckpointStore) BindOwner(string, CheckpointOwner) {}

func (s *PostgresCheckpointStore) OwnerMatches(ctx context.Context, id string, owner CheckpointOwner) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM agent_checkpoints
			WHERE checkpoint_id = $1 AND tenant_id = $2 AND actor_id = $3 AND agent_id = $4
			  AND expires_at > now() AND consumed_at IS NULL
		)`, id, owner.TenantID, owner.ActorID, owner.AgentID).Scan(&exists)
	return exists, err
}

func (s *PostgresCheckpointStore) Close() { s.pool.Close() }

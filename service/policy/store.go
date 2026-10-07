package policy

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store persists policies and execution logs so they survive an engine
// restart.
type Store interface {
	SavePolicy(p *Policy) error
	DeletePolicy(id string) error
	LoadPolicies() ([]*Policy, error)

	AppendLog(l ExecutionLog) error
	// GetLogs returns up to limit ExecutionLogs, newest first. If policyID
	// is "", it returns logs across every policy; limit <= 0 means no cap.
	GetLogs(policyID string, limit int) ([]ExecutionLog, error)
	// LastLogs returns the most recent ExecutionLog for every policy that
	// has at least one persisted row, keyed by policy ID.
	LastLogs() (map[string]ExecutionLog, error)
}

// defaultLogRetention bounds how long an execution_logs row survives:
// AppendLog prunes anything older than this, so the table stays bounded
// the same way Engine's in-memory logs tail is capped at maxInMemoryLogs -
// unlike that cap, Store is the durable copy, so it's bounded by age
// rather than by count. Override via WithLogRetention.
const defaultLogRetention = 30 * 24 * time.Hour

// pruneInterval throttles AppendLog's retention prune to at most once per
// interval rather than running an extra DELETE on every single insert -
// without this, a frequently-firing policy would pay a second write
// transaction on every trigger just to re-check a cutoff that only
// actually changes on the scale of days.
const pruneInterval = time.Hour

// StoreOption configures optional SQLiteStore behaviour at construction
// time, matching the EngineOption pattern in engine.go.
type StoreOption func(*SQLiteStore)

// WithLogRetention overrides defaultLogRetention. A retention of 0 disables
// pruning entirely (every execution_logs row is kept forever).
func WithLogRetention(d time.Duration) StoreOption {
	return func(s *SQLiteStore) { s.logRetention = d }
}

// SQLiteStore is a Store backed by SQLite via modernc.org/sqlite (pure Go,
// no cgo), following the same golang-migrate/embed.FS convention as
// service/house/db.
type SQLiteStore struct {
	logger       *zap.Logger
	db           *sql.DB
	registry     *ConditionRegistry
	logRetention time.Duration

	pruneMu     sync.Mutex
	lastPruneAt time.Time
}

// NewSQLiteStore creates a SQLiteStore backed by db, running any pending
// migrations before returning. db's lifecycle (opening/closing the
// underlying connection) is the caller's responsibility, matching
// service/house/db.NewDatabase. registry is used to resolve a persisted
// policy's condition_expr JSON back into a typed ConditionExpr tree on
// LoadPolicies.
//
// db is restricted to a single open connection: SQLite allows only one
// writer at a time regardless, and without this, a second connection
// racing a write (e.g. two concurrent Engine.trigger runs both calling
// AppendLog) hits SQLITE_BUSY immediately rather than queueing - the pool
// would return an error instead of just blocking the caller, silently
// dropping whichever write lost the race. Capping the pool at one
// connection makes database/sql itself queue that second caller instead.
func NewSQLiteStore(logger *zap.Logger, db *sql.DB, registry *ConditionRegistry, opts ...StoreOption) (*SQLiteStore, error) {
	db.SetMaxOpenConns(1)

	migrations, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		logger.Error("unable to open embedded migrations", zap.Error(err))
		return nil, err
	}
	driver, err := sqlite.WithInstance(db, &sqlite.Config{})
	if err != nil {
		logger.Error("unable to create migration driver", zap.Error(err))
		return nil, err
	}
	m, err := migrate.NewWithInstance("iofs", migrations, "sqlite", driver)
	if err != nil {
		logger.Error("unable to create migration", zap.Error(err))
		return nil, err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		logger.Error("error running migration", zap.Error(err))
		return nil, err
	}

	s := &SQLiteStore{
		logger:       logger,
		db:           db,
		registry:     registry,
		logRetention: defaultLogRetention,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SavePolicy inserts or updates p, keyed by ID.
func (s *SQLiteStore) SavePolicy(p *Policy) error {
	exprJSON, err := MarshalConditionExpr(p.ConditionExpr)
	if err != nil {
		return fmt.Errorf("policy: marshalling condition expression for %q: %w", p.ID, err)
	}

	isSystem := 0
	if strings.HasPrefix(p.ID, "sys.") {
		isSystem = 1
	}

	now := time.Now().UTC()
	_, err = s.db.Exec(`
		INSERT INTO policies (id, condition_expr, script, on_condition_false, is_system, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			condition_expr = excluded.condition_expr,
			script = excluded.script,
			on_condition_false = excluded.on_condition_false,
			is_system = excluded.is_system,
			updated_at = excluded.updated_at
	`, p.ID, string(exprJSON), p.Script, int(p.OnConditionFalse), isSystem, now, now)
	if err != nil {
		s.logger.Error("unable to save policy", zap.String("policy_id", p.ID), zap.Error(err))
		return err
	}
	return nil
}

// DeletePolicy removes id. It is not an error if id doesn't exist.
func (s *SQLiteStore) DeletePolicy(id string) error {
	_, err := s.db.Exec("DELETE FROM policies WHERE id = ?", id)
	if err != nil {
		s.logger.Error("unable to delete policy", zap.String("policy_id", id), zap.Error(err))
		return err
	}
	return nil
}

// LoadPolicies returns every persisted policy, with each one's
// condition_expr resolved back into a ConditionExpr tree via the registry
// passed to NewSQLiteStore.
func (s *SQLiteStore) LoadPolicies() ([]*Policy, error) {
	rows, err := s.db.Query("SELECT id, condition_expr, script, on_condition_false FROM policies")
	if err != nil {
		s.logger.Error("unable to load policies", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var policies []*Policy
	for rows.Next() {
		var id, exprJSON, script string
		var onConditionFalse int
		if err := rows.Scan(&id, &exprJSON, &script, &onConditionFalse); err != nil {
			s.logger.Error("unable to scan policy row", zap.Error(err))
			return nil, err
		}

		expr, err := UnmarshalConditionExpr([]byte(exprJSON), s.registry)
		if err != nil {
			return nil, fmt.Errorf("policy: loading policy %q: %w", id, err)
		}

		policies = append(policies, &Policy{
			ID:               id,
			ConditionExpr:    expr,
			Script:           script,
			OnConditionFalse: OnConditionFalse(onConditionFalse),
		})
	}
	return policies, rows.Err()
}

// AppendLog inserts a single execution log record, then - at most once per
// pruneInterval - prunes anything older than s.logRetention so the table
// doesn't grow unbounded. The prune is cheap once the table is caught up:
// it's an index-range delete (see migration 000002) that touches zero rows
// once nothing is past the cutoff, but it's still a second write
// transaction, so it's throttled rather than run on every single insert.
func (s *SQLiteStore) AppendLog(l ExecutionLog) error {
	var endedAt sql.NullTime
	if !l.EndedAt.IsZero() {
		endedAt = sql.NullTime{Time: l.EndedAt, Valid: true}
	}

	_, err := s.db.Exec(`
		INSERT INTO execution_logs (policy_id, started_at, ended_at, status, error)
		VALUES (?, ?, ?, ?, ?)
	`, l.PolicyID, l.StartedAt, endedAt, string(l.Status), l.Error)
	if err != nil {
		s.logger.Error("unable to append execution log", zap.String("policy_id", l.PolicyID), zap.Error(err))
		return err
	}

	if s.logRetention > 0 && s.shouldPrune() {
		cutoff := time.Now().UTC().Add(-s.logRetention)
		if _, err := s.db.Exec("DELETE FROM execution_logs WHERE started_at < ?", cutoff); err != nil {
			// The log itself is already committed above; a failed prune just
			// means the table grows a little more before the next attempt, not
			// a lost write, so it's a warning rather than a returned error.
			s.logger.Warn("unable to prune old execution logs", zap.Error(err))
		}
	}
	return nil
}

// shouldPrune reports whether at least pruneInterval has passed since the
// last prune, and if so, immediately marks a prune as starting now - so
// two AppendLog calls racing this check can't both decide to prune at
// once. lastPruneAt's zero value is far enough in the past that the very
// first call always prunes.
func (s *SQLiteStore) shouldPrune() bool {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()

	if time.Since(s.lastPruneAt) < pruneInterval {
		return false
	}
	s.lastPruneAt = time.Now()
	return true
}

// GetLogs returns up to limit ExecutionLogs, newest first, optionally
// filtered to a single policy.
func (s *SQLiteStore) GetLogs(policyID string, limit int) ([]ExecutionLog, error) {
	query := "SELECT policy_id, started_at, ended_at, status, error FROM execution_logs"
	var args []any
	if policyID != "" {
		query += " WHERE policy_id = ?"
		args = append(args, policyID)
	}
	query += " ORDER BY id DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		s.logger.Error("unable to get execution logs", zap.String("policy_id", policyID), zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	logs, err := scanExecutionLogs(rows)
	if err != nil {
		s.logger.Error("unable to scan execution log row", zap.Error(err))
		return nil, err
	}
	return logs, nil
}

// LastLogs returns the most recent ExecutionLog for every policy that has
// at least one persisted row, keyed by policy ID - the SQLite-backed
// counterpart to Engine's in-memory LastLogs, used so a restarted policyd
// still shows each policy's last run instead of nothing until it next
// fires. The subquery picks each policy_id's MAX(id) (ids are assigned in
// insertion order, so highest id is newest) rather than MAX(started_at),
// which a caller-supplied clock could make ambiguous across policies.
func (s *SQLiteStore) LastLogs() (map[string]ExecutionLog, error) {
	rows, err := s.db.Query(`
		SELECT el.policy_id, el.started_at, el.ended_at, el.status, el.error
		FROM execution_logs el
		JOIN (
			SELECT policy_id, MAX(id) AS max_id
			FROM execution_logs
			GROUP BY policy_id
		) latest ON el.policy_id = latest.policy_id AND el.id = latest.max_id
	`)
	if err != nil {
		s.logger.Error("unable to get last execution logs", zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	logs, err := scanExecutionLogs(rows)
	if err != nil {
		s.logger.Error("unable to scan last execution log row", zap.Error(err))
		return nil, err
	}

	out := make(map[string]ExecutionLog, len(logs))
	for _, l := range logs {
		out[l.PolicyID] = l
	}
	return out, nil
}

// scanExecutionLogs scans every remaining row of rows into ExecutionLogs.
// rows must be a query selecting exactly (policy_id, started_at, ended_at,
// status, error), the shared shape GetLogs and LastLogs both query.
func scanExecutionLogs(rows *sql.Rows) ([]ExecutionLog, error) {
	var logs []ExecutionLog
	for rows.Next() {
		var l ExecutionLog
		var status string
		var endedAt sql.NullTime
		var errStr sql.NullString

		if err := rows.Scan(&l.PolicyID, &l.StartedAt, &endedAt, &status, &errStr); err != nil {
			return nil, err
		}

		l.Status = ExecutionStatus(status)
		if endedAt.Valid {
			l.EndedAt = endedAt.Time
		}
		if errStr.Valid {
			l.Error = errStr.String
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// LoadPersistedPolicies loads every policy previously saved to store and
// registers it on e via e.register(p, false) — not Register — so reloading
// a policy on startup doesn't re-persist it as a redundant no-op write.
//
// Startup order matters here: call RegisterSystemConditionTypes(e),
// RegisterBuiltinConditionTypes(e), and RegisterLocationConditionTypes(e)
// before this, since a persisted policy using one of those condition types
// needs it registered to rebuild its condition tree.
func LoadPersistedPolicies(e *Engine, store Store) error {
	policies, err := store.LoadPolicies()
	if err != nil {
		return fmt.Errorf("policy: loading persisted policies: %w", err)
	}

	for _, p := range policies {
		if err := e.register(p, false); err != nil {
			return fmt.Errorf("policy: registering persisted policy %q: %w", p.ID, err)
		}
	}
	return nil
}

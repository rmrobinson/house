package policy

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
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
}

// SQLiteStore is a Store backed by SQLite via github.com/mattn/go-sqlite3,
// following the same golang-migrate/embed.FS convention as
// service/house/db.
type SQLiteStore struct {
	logger   *zap.Logger
	db       *sql.DB
	registry *ConditionRegistry
}

// NewSQLiteStore creates a SQLiteStore backed by db, running any pending
// migrations before returning. db's lifecycle (opening/closing the
// underlying connection) is the caller's responsibility, matching
// service/house/db.NewDatabase. registry is used to resolve a persisted
// policy's condition_expr JSON back into a typed ConditionExpr tree on
// LoadPolicies.
func NewSQLiteStore(logger *zap.Logger, db *sql.DB, registry *ConditionRegistry) (*SQLiteStore, error) {
	migrations, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		logger.Error("unable to open embedded migrations", zap.Error(err))
		return nil, err
	}
	driver, err := sqlite3.WithInstance(db, &sqlite3.Config{})
	if err != nil {
		logger.Error("unable to create migration driver", zap.Error(err))
		return nil, err
	}
	m, err := migrate.NewWithInstance("iofs", migrations, "sqlite3", driver)
	if err != nil {
		logger.Error("unable to create migration", zap.Error(err))
		return nil, err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		logger.Error("error running migration", zap.Error(err))
		return nil, err
	}

	return &SQLiteStore{
		logger:   logger,
		db:       db,
		registry: registry,
	}, nil
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

// AppendLog inserts a single execution log record.
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
	return nil
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

	var logs []ExecutionLog
	for rows.Next() {
		var l ExecutionLog
		var status string
		var endedAt sql.NullTime
		var errStr sql.NullString

		if err := rows.Scan(&l.PolicyID, &l.StartedAt, &endedAt, &status, &errStr); err != nil {
			s.logger.Error("unable to scan execution log row", zap.Error(err))
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
// registers it on e.
//
// Startup order matters here: call RegisterSystemConditionTypes(e) and
// RegisterBuiltinConditionTypes(e), then this, then
// LoadDefaultSystemPolicies(e) — never LoadSystemPolicies, which would
// re-register the shipped defaults unconditionally and, since
// Register persists to store whenever one is configured, silently
// overwrite a persisted override in the database before it's ever read
// back. LoadDefaultSystemPolicies skips any ID this step already
// registered, which is what lets a persisted policy — a genuine user
// override, or simply a previous run's own copy of a default — take
// precedence.
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

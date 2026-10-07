package policy

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// newTestSQLiteStore opens a fresh on-disk SQLite database under t.TempDir()
// and returns a migrated SQLiteStore backed by it.
func newTestSQLiteStore(t *testing.T, registry *ConditionRegistry) *SQLiteStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "policy.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	store, err := NewSQLiteStore(zaptest.NewLogger(t), db, registry)
	require.NoError(t, err)
	return store
}

func TestSQLiteStoreSaveLoadDeletePolicy(t *testing.T) {
	type modeParams struct{ Mode string }

	r := NewConditionRegistry()
	RegisterConditionType(r, "house-mode-is", func(p modeParams) Condition {
		return NewPollingCondition(func() bool { return p.Mode == "vacation" }, time.Hour)
	})
	store := newTestSQLiteStore(t, r)

	p := &Policy{
		ID:               "test.vacation-alert",
		ConditionExpr:    Use("house-mode-is", modeParams{Mode: "vacation"}),
		Script:           `home.notify("vacation", {})`,
		OnConditionFalse: Complete,
	}
	require.NoError(t, store.SavePolicy(p))

	loaded, err := store.LoadPolicies()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "test.vacation-alert", loaded[0].ID)
	assert.Equal(t, p.Script, loaded[0].Script)
	assert.Equal(t, Complete, loaded[0].OnConditionFalse)

	cond, err := loaded[0].ConditionExpr.build(r)
	require.NoError(t, err)
	assert.True(t, cond.Evaluate())

	// Saving again with the same ID updates in place rather than duplicating.
	p.Script = `home.notify("vacation-v2", {})`
	require.NoError(t, store.SavePolicy(p))
	loaded, err = store.LoadPolicies()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, p.Script, loaded[0].Script)

	require.NoError(t, store.DeletePolicy(p.ID))
	loaded, err = store.LoadPolicies()
	require.NoError(t, err)
	assert.Empty(t, loaded)

	// Deleting an already-absent ID is not an error.
	require.NoError(t, store.DeletePolicy("does-not-exist"))
}

func TestSQLiteStoreIsSystemFlag(t *testing.T) {
	r := NewConditionRegistry()
	staticConditionType(r, "true-cond", true)
	store := newTestSQLiteStore(t, r)

	require.NoError(t, store.SavePolicy(&Policy{
		ID:            "sys.occupancy",
		ConditionExpr: Use("true-cond", struct{}{}),
		Script:        "x=1",
	}))
	require.NoError(t, store.SavePolicy(&Policy{
		ID:            "user.custom",
		ConditionExpr: Use("true-cond", struct{}{}),
		Script:        "x=1",
	}))

	var isSystem int
	require.NoError(t, store.db.QueryRow("SELECT is_system FROM policies WHERE id = ?", "sys.occupancy").Scan(&isSystem))
	assert.Equal(t, 1, isSystem)

	require.NoError(t, store.db.QueryRow("SELECT is_system FROM policies WHERE id = ?", "user.custom").Scan(&isSystem))
	assert.Equal(t, 0, isSystem)
}

func TestSQLiteStoreAppendAndGetLogs(t *testing.T) {
	r := NewConditionRegistry()
	store := newTestSQLiteStore(t, r)

	base := time.Now().UTC().Truncate(time.Second)
	logs := []ExecutionLog{
		{PolicyID: "a", StartedAt: base, EndedAt: base.Add(time.Second), Status: StatusSuccess},
		{PolicyID: "b", StartedAt: base.Add(2 * time.Second), Status: StatusRunning},
		{PolicyID: "a", StartedAt: base.Add(3 * time.Second), EndedAt: base.Add(4 * time.Second), Status: StatusFailure, Error: "boom"},
	}
	for _, l := range logs {
		require.NoError(t, store.AppendLog(l))
	}

	onlyA, err := store.GetLogs("a", 0)
	require.NoError(t, err)
	require.Len(t, onlyA, 2)
	// Newest first.
	assert.Equal(t, StatusFailure, onlyA[0].Status)
	assert.Equal(t, "boom", onlyA[0].Error)
	assert.Equal(t, StatusSuccess, onlyA[1].Status)
	assert.True(t, onlyA[1].EndedAt.Equal(base.Add(time.Second)))

	all, err := store.GetLogs("", 0)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	limited, err := store.GetLogs("", 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, "a", limited[0].PolicyID)
	assert.Equal(t, StatusFailure, limited[0].Status)

	// A running log has no EndedAt yet: it must round-trip as zero, not a
	// garbage time.
	runningLogs, err := store.GetLogs("b", 0)
	require.NoError(t, err)
	require.Len(t, runningLogs, 1)
	assert.True(t, runningLogs[0].EndedAt.IsZero())
}

// TestSQLiteStoreAppendLogConcurrentNoLostRows confirms concurrent AppendLog
// calls queue rather than one losing its row to SQLITE_BUSY - the race that
// sqlitestore-busy-timeout-gap flagged (two concurrent Engine.trigger runs
// both writing an execution log). db.SetMaxOpenConns(1) in NewSQLiteStore is
// what makes database/sql itself serialize these instead of opening a
// second connection that collides on SQLite's single-writer lock.
func TestSQLiteStoreAppendLogConcurrentNoLostRows(t *testing.T) {
	r := NewConditionRegistry()
	store := newTestSQLiteStore(t, r)
	store.logRetention = 0 // isolate this test from the retention prune

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.AppendLog(ExecutionLog{
				PolicyID:  "concurrent",
				StartedAt: time.Now().UTC(),
				Status:    StatusSuccess,
			})
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	logs, err := store.GetLogs("concurrent", 0)
	require.NoError(t, err)
	assert.Len(t, logs, n)
}

// TestSQLiteStoreAppendLogPrunesOldRows confirms AppendLog prunes rows older
// than its configured retention (policy-execution-log-retention-todo) so
// execution_logs doesn't grow unbounded forever.
func TestSQLiteStoreAppendLogPrunesOldRows(t *testing.T) {
	r := NewConditionRegistry()
	store := newTestSQLiteStore(t, r)
	store.logRetention = time.Hour

	now := time.Now().UTC()
	// Pruning runs opportunistically on every AppendLog, including the one
	// that just inserted this row: a row already older than the retention
	// window at insert time (e.g. a backfill, or a long-delayed write) is
	// pruned immediately rather than surviving until some later call.
	require.NoError(t, store.AppendLog(ExecutionLog{
		PolicyID:  "old",
		StartedAt: now.Add(-2 * time.Hour),
		Status:    StatusSuccess,
	}))
	logs, err := store.GetLogs("", 0)
	require.NoError(t, err)
	assert.Empty(t, logs)

	require.NoError(t, store.AppendLog(ExecutionLog{
		PolicyID:  "new",
		StartedAt: now,
		Status:    StatusSuccess,
	}))

	logs, err = store.GetLogs("", 0)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, "new", logs[0].PolicyID)
}

// TestSQLiteStoreLogRetentionDisabled confirms a zero WithLogRetention keeps
// every row forever instead of pruning.
func TestSQLiteStoreLogRetentionDisabled(t *testing.T) {
	r := NewConditionRegistry()
	store := newTestSQLiteStore(t, r)
	store.logRetention = 0

	require.NoError(t, store.AppendLog(ExecutionLog{
		PolicyID:  "ancient",
		StartedAt: time.Now().UTC().AddDate(-1, 0, 0),
		Status:    StatusSuccess,
	}))
	require.NoError(t, store.AppendLog(ExecutionLog{
		PolicyID:  "new",
		StartedAt: time.Now().UTC(),
		Status:    StatusSuccess,
	}))

	logs, err := store.GetLogs("", 0)
	require.NoError(t, err)
	assert.Len(t, logs, 2)
}

func TestEngineWithStorePersistsRegisterAndUnregister(t *testing.T) {
	r := NewConditionRegistry()
	trigger := registerManualTrigger(t, r, "trigger")
	store := newTestSQLiteStore(t, r)

	e := NewEngine(newFakeHomeAPI(), r, zaptest.NewLogger(t), WithStore(store))
	t.Cleanup(e.Close)
	_ = trigger

	require.NoError(t, e.Register(&Policy{
		ID:            "test.persisted",
		ConditionExpr: Use("trigger", struct{}{}),
		Script:        "x=1",
	}))

	loaded, err := store.LoadPolicies()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, "test.persisted", loaded[0].ID)

	require.NoError(t, e.Unregister("test.persisted"))

	loaded, err = store.LoadPolicies()
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

// TestLoadPersistedPoliciesSurvivesRestart proves a policy persisted on one
// Engine/Store (including one built on a RegisterSystemConditionTypes
// condition type, same as sys.occupancy used to be) comes back correctly
// on a fresh Engine/registry after RegisterSystemConditionTypes +
// LoadPersistedPolicies, simulating a process restart.
func TestLoadPersistedPoliciesSurvivesRestart(t *testing.T) {
	r := NewConditionRegistry()
	store := newTestSQLiteStore(t, r)

	home1 := newFakeHomeAPI()
	e1 := NewEngine(home1, r, zaptest.NewLogger(t), WithStore(store))
	RegisterSystemConditionTypes(e1)
	require.NoError(t, e1.Register(&Policy{
		ID:            "user.occupancy",
		ConditionExpr: Use("sys.any-motion-detected", struct{}{}),
		Script:        `home.setHouseState("occupancy", "motion-seen")`,
	}))
	e1.Close()

	r2 := NewConditionRegistry()
	home2 := newFakeHomeAPI()
	store2, err := NewSQLiteStore(zaptest.NewLogger(t), store.db, r2)
	require.NoError(t, err)

	e2 := NewEngine(home2, r2, zaptest.NewLogger(t), WithStore(store2))
	t.Cleanup(e2.Close)
	RegisterSystemConditionTypes(e2)
	require.NoError(t, LoadPersistedPolicies(e2, store2))

	e2.Bus().Publish(Event{Topic: "motion.detected"})

	require.Eventually(t, func() bool {
		return home2.getHouseState("occupancy") == "motion-seen"
	}, time.Second, 10*time.Millisecond)
}

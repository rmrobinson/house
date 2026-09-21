package policy

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// newTestSQLiteStore opens a fresh on-disk SQLite database under t.TempDir()
// and returns a migrated SQLiteStore backed by it.
func newTestSQLiteStore(t *testing.T, registry *ConditionRegistry) *SQLiteStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "policy.db")
	db, err := sql.Open("sqlite3", dbPath)
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

func TestLoadPersistedPoliciesOverridesSystemDefault(t *testing.T) {
	r := NewConditionRegistry()
	store := newTestSQLiteStore(t, r)

	// First "run": load defaults, then a user overrides sys.occupancy, both
	// persisted.
	home1 := newFakeHomeAPI()
	e1 := NewEngine(home1, r, zaptest.NewLogger(t), WithStore(store))
	require.NoError(t, LoadSystemPolicies(e1))
	require.NoError(t, e1.Register(&Policy{
		ID:            "sys.occupancy",
		ConditionExpr: Use("sys.any-motion-detected", struct{}{}),
		Script:        `home.setHouseState("occupancy", "override")`,
	}))
	e1.Close()

	// Second "run" on a fresh Engine/registry, simulating a restart: types
	// first, then persisted policies (which brings back the override),
	// then defaults — which must skip sys.occupancy since it's already
	// registered, rather than clobbering the override.
	r2 := NewConditionRegistry()
	home2 := newFakeHomeAPI()
	store2, err := NewSQLiteStore(zaptest.NewLogger(t), store.db, r2)
	require.NoError(t, err)

	e2 := NewEngine(home2, r2, zaptest.NewLogger(t), WithStore(store2))
	t.Cleanup(e2.Close)
	RegisterSystemConditionTypes(e2)
	require.NoError(t, LoadPersistedPolicies(e2, store2))
	require.NoError(t, LoadDefaultSystemPolicies(e2))

	e2.Bus().Publish(Event{Topic: "motion.detected"})

	require.Eventually(t, func() bool {
		return home2.getHouseState("occupancy") == "override"
	}, time.Second, 10*time.Millisecond)
}

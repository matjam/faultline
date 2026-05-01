package prompts

import (
	"strings"
	"testing"
	"time"
)

// migration tests reuse the memStore type from prompts_test.go.

func TestLoadMigrationsReturnsShipped(t *testing.T) {
	migs, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatalf("expected at least one shipped migration, got 0")
	}
	// Sanity: IDs are monotonically increasing.
	for i := 1; i < len(migs); i++ {
		if migs[i].ID <= migs[i-1].ID {
			t.Errorf("migration IDs not strictly ascending: %d then %d", migs[i-1].ID, migs[i].ID)
		}
	}
	// First shipped migration should be the untrusted-content one.
	if migs[0].ID != 0 {
		t.Errorf("expected first migration id 0, got %d", migs[0].ID)
	}
	if !strings.Contains(migs[0].Body, "UNTRUSTED_CONTENT") {
		t.Errorf("first migration body does not reference UNTRUSTED_CONTENT; got body of length %d", len(migs[0].Body))
	}
}

func TestPendingMigrationsFiltersApplied(t *testing.T) {
	all := []Migration{
		{ID: 0, Slug: "a", Body: "x"},
		{ID: 1, Slug: "b", Body: "y"},
		{ID: 2, Slug: "c", Body: "z"},
	}
	applied := map[int]struct{}{0: {}, 2: {}}
	pending := PendingMigrations(all, applied)
	if len(pending) != 1 || pending[0].ID != 1 {
		t.Fatalf("expected only id 1 pending, got %+v", pending)
	}
}

func TestLoadAppliedMigrationsEmpty(t *testing.T) {
	store := newMemStore()
	applied, err := LoadAppliedMigrations(store)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("expected empty applied set, got %v", applied)
	}
}

func TestLoadAppliedMigrationsParsesRecords(t *testing.T) {
	store := newMemStore()
	store.data[migrationsLogPath] = `# Prompt migrations applied

Whatever explanation.

## Applied

- 000 add-untrusted-content-convention 2026-05-01T12:34:56Z
- 002 something-else 2026-06-15T08:00:00Z error: turn cap reached
- 003 unparseable line without a timestamp
- 005 looks-fine 2026-07-01T00:00:00Z
`
	applied, err := LoadAppliedMigrations(store)
	if err != nil {
		t.Fatalf("LoadApplied: %v", err)
	}
	want := []int{0, 2, 5}
	for _, id := range want {
		if _, ok := applied[id]; !ok {
			t.Errorf("missing applied id %d in %v", id, applied)
		}
	}
	if _, ok := applied[3]; ok {
		t.Errorf("id 3 should not parse (no timestamp)")
	}
	if len(applied) != 3 {
		t.Errorf("expected 3 applied entries, got %d: %v", len(applied), applied)
	}
}

func TestLoadAppliedMigrationsIgnoresOtherSections(t *testing.T) {
	// Lines under "## Notes" must not contribute to the applied set
	// even if they happen to look like valid records.
	store := newMemStore()
	store.data[migrationsLogPath] = `# Migrations

## Notes

- 000 do-not-count 2026-01-01T00:00:00Z

## Applied

- 001 real 2026-02-01T00:00:00Z
`
	applied, err := LoadAppliedMigrations(store)
	if err != nil {
		t.Fatalf("LoadApplied: %v", err)
	}
	if _, ok := applied[0]; ok {
		t.Errorf("id 0 from ## Notes should not be counted as applied")
	}
	if _, ok := applied[1]; !ok {
		t.Errorf("id 1 from ## Applied should be counted as applied")
	}
}

func TestRecordMigrationAppliedFreshFile(t *testing.T) {
	store := newMemStore()
	m := Migration{ID: 42, Slug: "the-answer", Body: ""}
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := RecordMigrationApplied(store, m, when, ""); err != nil {
		t.Fatalf("RecordMigrationApplied: %v", err)
	}

	written := store.data[migrationsLogPath]
	if !strings.Contains(written, migrationsLogHeader) {
		t.Errorf("missing canonical header in fresh file:\n%s", written)
	}
	if !strings.Contains(written, "- 042 the-answer 2026-05-01T12:00:00Z") {
		t.Errorf("missing record line:\n%s", written)
	}

	// Round-trip: LoadAppliedMigrations should now see id 42.
	applied, err := LoadAppliedMigrations(store)
	if err != nil {
		t.Fatalf("LoadAppliedMigrations: %v", err)
	}
	if _, ok := applied[42]; !ok {
		t.Errorf("id 42 missing after round-trip; applied=%v", applied)
	}
}

func TestRecordMigrationAppliedAppendsToExisting(t *testing.T) {
	store := newMemStore()
	m1 := Migration{ID: 0, Slug: "first", Body: ""}
	m2 := Migration{ID: 1, Slug: "second", Body: ""}
	when := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	if err := RecordMigrationApplied(store, m1, when, ""); err != nil {
		t.Fatal(err)
	}
	if err := RecordMigrationApplied(store, m2, when.Add(time.Hour), "with-note"); err != nil {
		t.Fatal(err)
	}

	written := store.data[migrationsLogPath]
	if strings.Count(written, "- 000 first") != 1 {
		t.Errorf("first record not preserved exactly once:\n%s", written)
	}
	if !strings.Contains(written, "- 001 second 2026-05-01T13:00:00Z with-note") {
		t.Errorf("second record missing or note dropped:\n%s", written)
	}

	applied, err := LoadAppliedMigrations(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := applied[0]; !ok {
		t.Errorf("id 0 missing after second record")
	}
	if _, ok := applied[1]; !ok {
		t.Errorf("id 1 missing after second record")
	}
}

package verdictlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestJournal(t *testing.T, now *time.Time) *Journal {
	t.Helper()
	journal, err := New(Config{
		Path:      filepath.Join(t.TempDir(), "diagnostic_verdicts.jsonl"),
		Retention: 24 * time.Hour,
		Now:       func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func TestJournalRecordsAndQueriesNewestFirst(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	journal := newTestJournal(t, &now)
	for index, verdict := range []string{"reproduced", "path_limited", "not_reproduced"} {
		if err := journal.Record(Entry{
			At: now.Add(time.Duration(index) * time.Minute), Source: SourceSpeed,
			StableID: "node-" + string(rune('a'+index)), Verdict: verdict,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	entries, err := journal.Query(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Verdict != "not_reproduced" || entries[2].Verdict != "reproduced" {
		t.Fatalf("entries = %+v, want all three newest first", entries)
	}
	filtered, _ := journal.Query(Filter{StableID: "node-b"})
	if len(filtered) != 1 || filtered[0].Verdict != "path_limited" {
		t.Fatalf("filtered = %+v, want the one node", filtered)
	}
	ranged, _ := journal.Query(Filter{From: now.Add(30 * time.Second), To: now.Add(90 * time.Second)})
	if len(ranged) != 1 || ranged[0].StableID != "node-b" {
		t.Fatalf("ranged = %+v, want the middle verdict", ranged)
	}
}

// A verdict without a node, a source or a verdict is refused rather than
// written as a line nobody can read back.
func TestJournalRefusesAnIncompleteEntry(t *testing.T) {
	now := time.Now()
	journal := newTestJournal(t, &now)
	if err := journal.Record(Entry{Source: SourceSpeed, Verdict: "reproduced"}); err == nil {
		t.Fatal("an entry without a node was recorded")
	}
}

func TestJournalCompactionForgetsWhatRetentionExcludes(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	journal := newTestJournal(t, &now)
	_ = journal.Record(Entry{At: now.Add(-48 * time.Hour), Source: SourceSpeed, StableID: "old", Verdict: "reproduced"})
	_ = journal.Record(Entry{At: now.Add(-time.Hour), Source: SourceSpeed, StableID: "fresh", Verdict: "reproduced"})
	if err := journal.Compact(); err != nil {
		t.Fatal(err)
	}
	entries, _ := journal.Query(Filter{})
	if len(entries) != 1 || entries[0].StableID != "fresh" {
		t.Fatalf("entries after compaction = %+v, want only the fresh one", entries)
	}
}

// A torn line — a crash in the middle of an append — must not hide the rest of
// the history.
func TestJournalSkipsAnUnreadableLine(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	journal := newTestJournal(t, &now)
	_ = journal.Record(Entry{At: now, Source: SourceAvailability, StableID: "node", Verdict: "not_reproduced"})
	file, err := os.OpenFile(journal.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"at":"2026-09-25T12:01:00Z","source":"avail`)
	_ = file.Close()
	entries, err := journal.Query(Filter{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v; want the one readable line", entries, err)
	}
}

func TestJournalBoundsFreeText(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	journal := newTestJournal(t, &now)
	_ = journal.Record(Entry{At: now, Source: SourceSpeed, StableID: "node", Verdict: "reproduced", Detail: strings.Repeat("x", 5000)})
	entries, _ := journal.Query(Filter{})
	if len(entries) != 1 || len([]rune(entries[0].Detail)) > maxTextRunes {
		t.Fatalf("detail length = %d, want at most %d", len([]rune(entries[0].Detail)), maxTextRunes)
	}
}

// A journal nobody configured forgets quietly: the automations hold a nil one
// and must not have to check.
func TestANilJournalIsANoOp(t *testing.T) {
	var journal *Journal
	if err := journal.Record(Entry{Source: SourceSpeed, StableID: "node", Verdict: "reproduced"}); err != nil {
		t.Fatalf("record on a nil journal: %v", err)
	}
	if entries, err := journal.Query(Filter{}); err != nil || entries != nil {
		t.Fatalf("query on a nil journal = %+v, %v", entries, err)
	}
}

package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestScraperCRUDAndOwnerScope(t *testing.T) {
	st := openTemp(t)

	sc, err := st.AddScraper(Scraper{
		Owner: "alice", Name: "Wikipedia DM", Model: "website",
		Source: "https://de.wikipedia.org/wiki/Diskrete_Mathematik", ScheduleKind: "manual",
		Enabled: true, Allow: []string{"wikipedia.org"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if sc.ID == "" {
		t.Fatal("no id assigned")
	}
	if sc.LastRunStatus != "never" {
		t.Errorf("LastRunStatus = %q, want never", sc.LastRunStatus)
	}

	// Owner scoping: bob sees nothing.
	if got, _ := st.Scrapers("bob"); len(got) != 0 {
		t.Errorf("bob sees %d scrapers, want 0", len(got))
	}
	if _, err := st.Scraper("bob", sc.ID); err != ErrNotFound {
		t.Errorf("bob loading alice's scraper: err = %v, want ErrNotFound", err)
	}

	got, err := st.Scrapers("alice")
	if err != nil || len(got) != 1 {
		t.Fatalf("alice scrapers: n=%d err=%v", len(got), err)
	}
	if len(got[0].Allow) != 1 || got[0].Allow[0] != "wikipedia.org" {
		t.Errorf("allow roundtrip = %v", got[0].Allow)
	}

	// Partial update.
	newName := "Wikipedia — DM II"
	disabled := false
	upd, err := st.UpdateScraper("alice", sc.ID, ScraperPatch{Name: &newName, Enabled: &disabled})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != newName || upd.Enabled {
		t.Errorf("update not applied: %+v", upd)
	}
	if upd.Source != sc.Source {
		t.Errorf("unpatched field changed: source = %q", upd.Source)
	}
}

func TestRunsAndDocuments(t *testing.T) {
	st := openTemp(t)
	sc, _ := st.AddScraper(Scraper{Owner: "alice", Name: "S", Source: "https://x.example/"})

	runID, err := st.StartRun("alice", sc.ID)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := st.InsertDocument(Document{
			RunID: runID, ScraperID: sc.ID, Owner: "alice",
			Title: "Doc", Kategorie: "Foliensatz", Source: "scraper",
			URL: "https://x.example/doc", MediaType: "text/markdown", Bytes: 10, LakearchRef: "ref",
		}); err != nil {
			t.Fatalf("insert doc: %v", err)
		}
	}

	if err := st.FinishRun(Run{ID: runID, ScraperID: sc.ID, Owner: "alice", Status: "ok", Added: 3, Pages: 2, Bytes: 30}); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	// last_run_* memoized on the scraper.
	got, _ := st.Scraper("alice", sc.ID)
	if got.LastRunStatus != "ok" || got.LastRunAdded != 3 {
		t.Errorf("last run not memoized: status=%q added=%d", got.LastRunStatus, got.LastRunAdded)
	}

	docs, err := st.Documents("alice", 100)
	if err != nil || len(docs) != 3 {
		t.Fatalf("documents: n=%d err=%v", len(docs), err)
	}
	runDocs, _ := st.DocumentsForRun("alice", runID)
	if len(runDocs) != 3 {
		t.Errorf("run documents = %d, want 3", len(runDocs))
	}

	// Owner scoping on documents.
	if got, _ := st.Documents("bob", 100); len(got) != 0 {
		t.Errorf("bob sees %d documents, want 0", len(got))
	}
}

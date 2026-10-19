package appserver

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestReadStateCatalogReadsProjectsAndThreads(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state_7.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, position INTEGER NOT NULL)`,
		`CREATE TABLE project_roots (project_id TEXT NOT NULL, position INTEGER NOT NULL, path TEXT NOT NULL)`,
		`CREATE TABLE threads (
			id TEXT PRIMARY KEY, title TEXT, name TEXT, cwd TEXT,
			updated_at INTEGER, updated_at_ms INTEGER, archived INTEGER,
			source TEXT, thread_source TEXT, agent_path TEXT, project_id TEXT,
			preview TEXT, first_user_message TEXT)`,
		`INSERT INTO projects(id, name, position) VALUES ('p-alpha', 'alpha', 0)`,
		`INSERT INTO project_roots(project_id, position, path) VALUES ('p-alpha', 0, '/Users/example/work/alpha')`,
		`INSERT INTO threads(id, title, name, cwd, updated_at, archived, source, thread_source, agent_path, project_id, preview, first_user_message)
			VALUES ('t-alpha', 'Alpha title', 'Alpha work', '/Users/example/work/alpha/pkg', 100, 0, 'vscode', 'user', NULL, 'p-alpha', 'alpha preview', 'alpha request')`,
		`INSERT INTO threads(id, title, name, cwd, updated_at, archived, source, thread_source, agent_path, project_id, preview, first_user_message)
			VALUES ('t-preview', '', '', '/Users/example/work/loose', 90, 0, 'vscode', 'user', NULL, NULL, '', 'hello world')`,
		`INSERT INTO threads(id, title, name, cwd, updated_at, archived, source, thread_source, agent_path, project_id, preview, first_user_message)
			VALUES ('t-sub', 'Subagent', '', '/Users/example/work/alpha', 80, 0, '{"subagent":{}}', 'subagent', '/root/review', 'p-alpha', '', '')`,
		`INSERT INTO threads(id, title, name, cwd, updated_at, archived, source, thread_source, agent_path, project_id, preview, first_user_message)
			VALUES ('t-exec', 'Exec run', '', '/tmp/scratch', 75, 0, 'exec', 'user', NULL, NULL, '', '')`,
		`INSERT INTO threads(id, title, name, cwd, updated_at, archived, source, thread_source, agent_path, project_id, preview, first_user_message)
			VALUES ('t-archived', 'Archived', '', '/Users/example/work/alpha', 70, 1, 'vscode', 'user', NULL, 'p-alpha', '', '')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q failed: %v", statement, err)
		}
	}

	catalog, err := ReadStateCatalog(context.Background(), dir)
	if err != nil {
		t.Fatalf("ReadStateCatalog failed: %v", err)
	}
	if len(catalog.Projects) != 1 || catalog.Projects[0].Name != "alpha" {
		t.Fatalf("projects = %#v, want alpha", catalog.Projects)
	}
	if roots := catalog.Projects[0].RootPaths; len(roots) != 1 || roots[0] != "/Users/example/work/alpha" {
		t.Fatalf("roots = %#v, want alpha root", roots)
	}
	if len(catalog.Threads) != 2 {
		t.Fatalf("threads = %#v, want subagent, exec and archived excluded", catalog.Threads)
	}
	byID := map[string]string{}
	for _, thread := range catalog.Threads {
		byID[thread.ID] = thread.Title
		if thread.ID == "t-alpha" && thread.ProjectName != "alpha" {
			t.Fatalf("t-alpha project = %q, want alpha", thread.ProjectName)
		}
	}
	if byID["t-alpha"] != "Alpha work" || byID["t-preview"] != "hello world" {
		t.Fatalf("titles = %#v, want name then first user message fallback", byID)
	}
}

func TestFindStateDBPrefersNewestVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"state_3.sqlite", "state_12.sqlite", "state.db"} {
		db, err := sql.Open("sqlite", filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("sql.Open(%s) failed: %v", name, err)
		}
		if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatalf("create table in %s failed: %v", name, err)
		}
		_ = db.Close()
	}
	path, err := FindStateDB(dir)
	if err != nil {
		t.Fatalf("FindStateDB failed: %v", err)
	}
	if filepath.Base(path) != "state_12.sqlite" {
		t.Fatalf("state db = %s, want state_12.sqlite", filepath.Base(path))
	}
}

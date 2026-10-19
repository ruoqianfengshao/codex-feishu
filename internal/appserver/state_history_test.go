package appserver

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestReadRecentCompletedTurnsFromHistoryDB(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "thread_history_3.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE thread_turns (
			thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL,
			status TEXT NOT NULL, first_user_item_id TEXT, final_agent_item_id TEXT)`,
		`CREATE TABLE thread_items (
			thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL,
			rollout_ordinal INTEGER NOT NULL, item_type TEXT NOT NULL DEFAULT '', item_json TEXT NOT NULL)`,
		`INSERT INTO thread_turns VALUES ('t1', 'turn-1', 10, 'completed', 'u1', 'a1')`,
		`INSERT INTO thread_turns VALUES ('t1', 'turn-2', 20, 'completed', 'u2', 'a2')`,
		`INSERT INTO thread_turns VALUES ('t1', 'turn-3', 30, 'inProgress', 'u3', 'a3')`,
		`INSERT INTO thread_items VALUES ('t1', 'turn-1', 'u1', 1, 'userMessage', '{"type":"userMessage","id":"u1","content":[{"type":"text","text":"first request"}]}')`,
		`INSERT INTO thread_items VALUES ('t1', 'turn-1', 'a1', 2, 'agentMessage', '{"type":"agentMessage","id":"a1","text":"first answer"}')`,
		`INSERT INTO thread_items VALUES ('t1', 'turn-2', 'u2', 3, 'userMessage', '{"type":"userMessage","id":"u2","content":[{"type":"text","text":"second request"}]}')`,
		`INSERT INTO thread_items VALUES ('t1', 'turn-2', 'a2', 4, 'agentMessage', '{"type":"agentMessage","id":"a2","text":"second answer"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q failed: %v", statement, err)
		}
	}

	turns, err := ReadRecentCompletedTurns(context.Background(), dir, "t1", 2)
	if err != nil {
		t.Fatalf("ReadRecentCompletedTurns failed: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %#v, want two completed turns", turns)
	}
	if turns[0].TurnID != "turn-1" || turns[1].TurnID != "turn-2" {
		t.Fatalf("turn order = %#v, want chronological order", turns)
	}
	if turns[1].UserText != "second request" || turns[1].FinalText != "second answer" {
		t.Fatalf("turn text = %#v, want user and final texts", turns[1])
	}
	if turns[1].UserItemID != "u2" {
		t.Fatalf("user item id = %q, want u2", turns[1].UserItemID)
	}
}

func TestHydrateThreadReadItemsFillsLatestTurn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "thread_history_5.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE thread_turns (
			thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL,
			status TEXT NOT NULL, first_user_item_id TEXT, final_agent_item_id TEXT)`,
		`CREATE TABLE thread_items (
			thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL,
			rollout_ordinal INTEGER NOT NULL, item_type TEXT NOT NULL DEFAULT '', item_json TEXT NOT NULL)`,
		`INSERT INTO thread_turns VALUES ('t1', 'turn-9', 10, 'completed', 'u9', 'a9')`,
		`INSERT INTO thread_items VALUES ('t1', 'turn-9', 'u9', 1, 'userMessage', '{"type":"userMessage","id":"u9","content":[{"type":"text","text":"panel request"}]}')`,
		`INSERT INTO thread_items VALUES ('t1', 'turn-9', 'a9', 2, 'agentMessage', '{"type":"agentMessage","id":"a9","text":"panel answer"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q failed: %v", statement, err)
		}
	}
	payload := map[string]any{
		"thread": map[string]any{
			"id":     "t1",
			"status": "completed",
			"turns": []any{
				map[string]any{"id": "turn-9", "status": "completed", "items": []any{}},
			},
		},
	}

	hydrated := HydrateThreadReadItems(context.Background(), dir, payload)
	thread, _ := hydrated["thread"].(map[string]any)
	turns, _ := thread["turns"].([]any)
	turn, _ := turns[0].(map[string]any)
	items, _ := turn["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %#v, want latest turn items hydrated", items)
	}
	snapshot := SnapshotFromThreadRead(hydrated)
	if snapshot.LatestFinalText != "panel answer" || snapshot.LatestUserMessageText != "panel request" {
		t.Fatalf("snapshot = %#v, want hydrated final and user text", snapshot)
	}
}

func TestReadTurnResultUsesSharedHistoryStatusAndFinalItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "thread_history_4.sqlite"))
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE thread_turns (
			thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL,
			status TEXT NOT NULL, first_user_item_id TEXT, final_agent_item_id TEXT)`,
		`CREATE TABLE thread_items (
			thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL,
			rollout_ordinal INTEGER NOT NULL, item_type TEXT NOT NULL DEFAULT '', item_json TEXT NOT NULL)`,
		`INSERT INTO thread_turns VALUES ('t1', 'turn-done', 10, 'completed', 'u1', 'a1')`,
		`INSERT INTO thread_turns VALUES ('t1', 'turn-running', 20, 'inProgress', 'u2', '')`,
		`INSERT INTO thread_items VALUES ('t1', 'turn-done', 'a1', 2, 'agentMessage', '{"type":"agentMessage","id":"a1","text":"real final answer"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q failed: %v", statement, err)
		}
	}

	done, err := ReadTurnResult(context.Background(), dir, "t1", "turn-done")
	if err != nil {
		t.Fatalf("ReadTurnResult(done) failed: %v", err)
	}
	if done == nil || done.Status != "completed" || done.FinalText != "real final answer" {
		t.Fatalf("done = %#v, want completed turn with final item", done)
	}

	running, err := ReadTurnResult(context.Background(), dir, "t1", "turn-running")
	if err != nil {
		t.Fatalf("ReadTurnResult(running) failed: %v", err)
	}
	if running == nil || running.Status != "inProgress" || running.FinalText != "" {
		t.Fatalf("running = %#v, want inProgress without final text", running)
	}

	missing, err := ReadTurnResult(context.Background(), dir, "t1", "turn-missing")
	if err != nil {
		t.Fatalf("ReadTurnResult(missing) failed: %v", err)
	}
	if missing != nil {
		t.Fatalf("missing = %#v, want nil", missing)
	}
}

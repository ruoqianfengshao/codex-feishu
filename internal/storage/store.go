package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ruoqianfengshao/codex-feishu/internal/model"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return err
	}
	schema := `
	CREATE TABLE IF NOT EXISTS threads (
		thread_id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		cwd TEXT,
		project_name TEXT NOT NULL,
		directory_name TEXT,
		updated_at INTEGER NOT NULL,
		status TEXT,
		last_preview TEXT,
		active_turn_id TEXT,
		preferred_model TEXT,
		permissions_mode TEXT,
		archived INTEGER NOT NULL DEFAULT 0,
		listed INTEGER NOT NULL DEFAULT 1,
		raw_json TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS thread_snapshots (
		thread_id TEXT PRIMARY KEY,
		last_live_event_at TEXT,
		last_poll_at TEXT,
		next_poll_after TEXT,
		last_seen_thread_status TEXT,
		last_seen_turn_id TEXT,
		last_seen_turn_status TEXT,
		last_progress_fp TEXT,
		last_progress_sent_at TEXT,
		last_final_fp TEXT,
		last_completion_fp TEXT,
		last_approval_fp TEXT,
		snapshot_json TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS message_routes (
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		item_id TEXT,
		event_id TEXT,
		created_at TEXT NOT NULL,
		PRIMARY KEY(chat_id, topic_id, message_id)
	);

	CREATE TABLE IF NOT EXISTS callback_routes (
		route_token TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		request_id TEXT,
		chat_message_id INTEGER,
		status TEXT NOT NULL,
		expires_at TEXT,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS pending_approvals (
		request_id TEXT PRIMARY KEY,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		item_id TEXT,
		prompt_kind TEXT NOT NULL,
		question TEXT,
		status TEXT NOT NULL,
		chat_message_id INTEGER,
		payload_json TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS delivery_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id TEXT NOT NULL,
		chat_key TEXT NOT NULL,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		thread_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		status TEXT NOT NULL,
		retry_count INTEGER NOT NULL DEFAULT 0,
		available_at TEXT NOT NULL,
		last_error TEXT,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_delivery_queue_event_target
		ON delivery_queue(event_id, chat_key);

	CREATE TABLE IF NOT EXISTS delivery_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		queue_id INTEGER NOT NULL,
		attempt_no INTEGER NOT NULL,
		status TEXT NOT NULL,
		error_text TEXT,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS daemon_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS thread_panels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		project_name TEXT NOT NULL,
		thread_id TEXT NOT NULL,
		source_mode TEXT NOT NULL DEFAULT 'explicit',
		summary_message_id INTEGER NOT NULL DEFAULT 0,
		tool_message_id INTEGER NOT NULL DEFAULT 0,
		output_message_id INTEGER NOT NULL DEFAULT 0,
		current_turn_id TEXT,
		status TEXT,
		archive_enabled INTEGER NOT NULL DEFAULT 1,
		last_summary_hash TEXT,
		last_tool_hash TEXT,
		last_output_hash TEXT,
		last_final_notice_fp TEXT,
		user_message_id INTEGER NOT NULL DEFAULT 0,
		last_user_notice_fp TEXT,
		plan_prompt_message_id INTEGER NOT NULL DEFAULT 0,
		last_plan_prompt_fp TEXT,
		details_view_json TEXT,
		last_final_card_hash TEXT,
		is_current INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS chat_steer_state (
		chat_key TEXT PRIMARY KEY,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		panel_id INTEGER NOT NULL DEFAULT 0,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS external_id_maps (
		namespace TEXT NOT NULL,
		external_id TEXT NOT NULL,
		numeric_id INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(namespace, external_id),
		UNIQUE(namespace, numeric_id)
	);

	CREATE TABLE IF NOT EXISTS feishu_message_maps (
		message_id INTEGER PRIMARY KEY,
		open_message_id TEXT NOT NULL UNIQUE,
		chat_id INTEGER NOT NULL,
		open_chat_id TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS feishu_thread_topics (
		chat_id INTEGER NOT NULL,
		open_chat_id TEXT NOT NULL,
		codex_thread_id TEXT NOT NULL,
		root_message_id INTEGER NOT NULL,
		root_open_message_id TEXT NOT NULL,
		feishu_thread_id TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(chat_id, codex_thread_id)
	);

	CREATE INDEX IF NOT EXISTS idx_threads_updated_at ON threads(updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_threads_project_updated_at ON threads(project_name, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_delivery_queue_status_available_at ON delivery_queue(status, available_at);
	CREATE INDEX IF NOT EXISTS idx_pending_approvals_status_updated_at ON pending_approvals(status, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_thread_panels_thread_current ON thread_panels(chat_id, topic_id, thread_id, is_current, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_chat_steer_expires_at ON chat_steer_state(expires_at);
	CREATE INDEX IF NOT EXISTS idx_feishu_message_maps_open_chat_id ON feishu_message_maps(open_chat_id);
	CREATE INDEX IF NOT EXISTS idx_feishu_thread_topics_open_root ON feishu_thread_topics(open_chat_id, root_open_message_id);
	CREATE INDEX IF NOT EXISTS idx_feishu_thread_topics_thread ON feishu_thread_topics(feishu_thread_id);
	`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := s.dropLegacyChatTables(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS thread_bindings`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM daemon_state WHERE key = 'ui.panel_mode'`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "source_mode", `ALTER TABLE thread_panels ADD COLUMN source_mode TEXT NOT NULL DEFAULT 'explicit'`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_final_notice_fp", `ALTER TABLE thread_panels ADD COLUMN last_final_notice_fp TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "user_message_id", `ALTER TABLE thread_panels ADD COLUMN user_message_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_user_notice_fp", `ALTER TABLE thread_panels ADD COLUMN last_user_notice_fp TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "plan_prompt_message_id", `ALTER TABLE thread_panels ADD COLUMN plan_prompt_message_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_plan_prompt_fp", `ALTER TABLE thread_panels ADD COLUMN last_plan_prompt_fp TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "details_view_json", `ALTER TABLE thread_panels ADD COLUMN details_view_json TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_final_card_hash", `ALTER TABLE thread_panels ADD COLUMN last_final_card_hash TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "threads", "listed", `ALTER TABLE threads ADD COLUMN listed INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	return nil
}

func (s *Store) dropLegacyChatTables(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS telegram_message_routes`); err != nil {
		return err
	}
	legacyCallback, err := s.hasColumn(ctx, "callback_routes", "telegram_message_id")
	if err != nil {
		return err
	}
	if legacyCallback {
		if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS callback_routes`); err != nil {
			return err
		}
	}
	legacyApproval, err := s.hasColumn(ctx, "pending_approvals", "telegram_message_id")
	if err != nil {
		return err
	}
	if legacyApproval {
		if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS pending_approvals`); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS callback_routes (
		route_token TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		request_id TEXT,
		chat_message_id INTEGER,
		status TEXT NOT NULL,
		expires_at TEXT,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS pending_approvals (
		request_id TEXT PRIMARY KEY,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		item_id TEXT,
		prompt_kind TEXT NOT NULL,
		question TEXT,
		status TEXT NOT NULL,
		chat_message_id INTEGER,
		payload_json TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_pending_approvals_status_updated_at ON pending_approvals(status, updated_at DESC);
	`); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, tableName, columnName, alterSQL string) error {
	exists, err := s.hasColumn(ctx, tableName, columnName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, alterSQL); err != nil {
		return err
	}
	exists, err = s.hasColumn(ctx, tableName, columnName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("column %s.%s was not created", tableName, columnName)
	}
	return nil
}

func (s *Store) hasColumn(ctx context.Context, tableName, columnName string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, columnName) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) UpsertThread(ctx context.Context, thread model.Thread) error {
	raw := thread.Raw
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO threads(thread_id, title, cwd, project_name, directory_name, updated_at, status, last_preview, active_turn_id, preferred_model, permissions_mode, archived, listed, raw_json)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(thread_id) DO UPDATE SET
		title = excluded.title,
		cwd = excluded.cwd,
		project_name = excluded.project_name,
		directory_name = excluded.directory_name,
		updated_at = excluded.updated_at,
		status = excluded.status,
		last_preview = excluded.last_preview,
		active_turn_id = excluded.active_turn_id,
		preferred_model = excluded.preferred_model,
		permissions_mode = excluded.permissions_mode,
		archived = excluded.archived,
		raw_json = excluded.raw_json`,
		thread.ID, thread.Title, nullable(thread.CWD), thread.ProjectName, nullable(thread.DirectoryName), thread.UpdatedAt,
		nullable(thread.Status), nullable(thread.LastPreview), nullable(thread.ActiveTurnID), nullable(thread.PreferredModel),
		nullable(thread.PermissionsMode), boolToInt(thread.Archived), boolToInt(true), string(raw),
	)
	return err
}

func (s *Store) GetThread(ctx context.Context, threadID string) (*model.Thread, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT thread_id, title, cwd, project_name, directory_name, updated_at, status, last_preview, active_turn_id, preferred_model, permissions_mode, archived, listed, raw_json
	FROM threads WHERE thread_id = ?`, threadID)
	return scanThread(row)
}

func (s *Store) ListThreads(ctx context.Context, limit int, search string) ([]model.Thread, error) {
	if limit <= 0 {
		limit = 10
	}
	query := `
	SELECT thread_id, title, cwd, project_name, directory_name, updated_at, status, last_preview, active_turn_id, preferred_model, permissions_mode, archived, listed, raw_json
	FROM threads WHERE ` + visibleThreadPredicateSQL
	args := make([]any, 0, 2)
	if trimmed := strings.TrimSpace(search); trimmed != "" {
		query += ` AND (lower(title) LIKE ? OR lower(project_name) LIKE ? OR lower(last_preview) LIKE ? OR lower(thread_id) LIKE ?)`
		pattern := "%" + strings.ToLower(trimmed) + "%"
		args = append(args, pattern, pattern, pattern, pattern)
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Thread{}
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *thread)
	}
	return out, rows.Err()
}

const visibleThreadPredicateSQL = `(
	archived = 0
	AND listed = 1
	AND lower(trim(cast(coalesce(json_extract(raw_json, '$.archived'), json_extract(raw_json, '$.thread.archived'), json_extract(raw_json, '$.isArchived'), json_extract(raw_json, '$.thread.isArchived'), '') as text))) NOT IN ('1', 'true', 'yes')
	AND lower(trim(cast(coalesce(json_extract(raw_json, '$.deleted'), json_extract(raw_json, '$.thread.deleted'), json_extract(raw_json, '$.isDeleted'), json_extract(raw_json, '$.thread.isDeleted'), '') as text))) NOT IN ('1', 'true', 'yes')
	AND lower(trim(cast(coalesce(json_extract(raw_json, '$.ephemeral'), json_extract(raw_json, '$.thread.ephemeral'), '') as text))) NOT IN ('1', 'true', 'yes')
	AND trim(cast(coalesce(json_extract(raw_json, '$.source.subAgent'), json_extract(raw_json, '$.thread.source.subAgent'), '') as text)) = ''
)`

func (s *Store) ReconcileListedThreads(ctx context.Context, listedIDs map[string]struct{}) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET listed = 0`); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `UPDATE threads SET listed = 1 WHERE thread_id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for threadID := range listedIDs {
		if _, err := stmt.ExecContext(ctx, threadID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CountThreads(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT count(*) FROM threads WHERE `+visibleThreadPredicateSQL)
	var count int
	return count, row.Scan(&count)
}

func (s *Store) ListProjectGroups(ctx context.Context) (map[string][]model.Thread, error) {
	rows, err := s.ListThreads(ctx, 500, "")
	if err != nil {
		return nil, err
	}
	grouped := map[string][]model.Thread{}
	for _, thread := range rows {
		grouped[thread.ProjectName] = append(grouped[thread.ProjectName], thread)
	}
	return grouped, nil
}

func (s *Store) UpsertSnapshot(ctx context.Context, threadID string, snapshot model.ThreadSnapshotState) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	updatedAt := string(model.NowString())
	_, err = s.db.ExecContext(ctx, `
	INSERT INTO thread_snapshots(
		thread_id, last_live_event_at, last_poll_at, next_poll_after, last_seen_thread_status, last_seen_turn_id, last_seen_turn_status,
		last_progress_fp, last_progress_sent_at, last_final_fp, last_completion_fp, last_approval_fp, snapshot_json, updated_at
	)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(thread_id) DO UPDATE SET
		last_live_event_at = excluded.last_live_event_at,
		last_poll_at = excluded.last_poll_at,
		next_poll_after = excluded.next_poll_after,
		last_seen_thread_status = excluded.last_seen_thread_status,
		last_seen_turn_id = excluded.last_seen_turn_id,
		last_seen_turn_status = excluded.last_seen_turn_status,
		last_progress_fp = excluded.last_progress_fp,
		last_progress_sent_at = excluded.last_progress_sent_at,
		last_final_fp = excluded.last_final_fp,
		last_completion_fp = excluded.last_completion_fp,
		last_approval_fp = excluded.last_approval_fp,
		snapshot_json = excluded.snapshot_json,
		updated_at = excluded.updated_at`,
		threadID,
		nullable(string(snapshot.LastRichLiveEventAt)),
		nullable(string(snapshot.LastPollAt)),
		nullable(string(snapshot.NextPollAfter)),
		nullable(snapshot.LastSeenThreadStatus),
		nullable(snapshot.LastSeenTurnID),
		nullable(snapshot.LastSeenTurnStatus),
		nullable(snapshot.LastProgressFP),
		nullable(string(snapshot.LastProgressSentAt)),
		nullable(snapshot.LastFinalFP),
		nullable(snapshot.LastCompletionFP),
		nullable(snapshot.LastApprovalFP),
		string(payload),
		updatedAt,
	)
	return err
}

func (s *Store) GetSnapshot(ctx context.Context, threadID string) (*model.ThreadSnapshotState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT snapshot_json FROM thread_snapshots WHERE thread_id = ?`, threadID)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var snapshot model.ThreadSnapshotState
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *Store) MarkLiveEvent(ctx context.Context, threadID string, when model.TimeString) error {
	snapshot, err := s.GetSnapshot(ctx, threadID)
	if err != nil {
		return err
	}
	if snapshot == nil {
		snapshot = &model.ThreadSnapshotState{}
	}
	snapshot.LastRichLiveEventAt = when
	return s.UpsertSnapshot(ctx, threadID, *snapshot)
}

func (s *Store) PutMessageRoute(ctx context.Context, route model.MessageRoute) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT OR REPLACE INTO message_routes(chat_id, topic_id, message_id, thread_id, turn_id, item_id, event_id, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		route.ChatID, route.TopicID, route.MessageID, route.ThreadID, nullable(route.TurnID), nullable(route.ItemID), nullable(route.EventID), route.CreatedAt,
	)
	return err
}

func (s *Store) ResolveMessageRoute(ctx context.Context, chatID, topicID, messageID int64) (*model.MessageRoute, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, topic_id, message_id, thread_id, coalesce(turn_id, ''), coalesce(item_id, ''), coalesce(event_id, ''), created_at
	FROM message_routes WHERE chat_id = ? AND topic_id = ? AND message_id = ?`,
		chatID, topicID, messageID,
	)
	var route model.MessageRoute
	err := row.Scan(&route.ChatID, &route.TopicID, &route.MessageID, &route.ThreadID, &route.TurnID, &route.ItemID, &route.EventID, &route.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) FindMessageRouteByEvent(ctx context.Context, threadID, turnID, itemID, eventID string) (*model.MessageRoute, error) {
	threadID = strings.TrimSpace(threadID)
	eventID = strings.TrimSpace(eventID)
	if threadID == "" || eventID == "" {
		return nil, nil
	}
	query := `
	SELECT chat_id, topic_id, message_id, thread_id, coalesce(turn_id, ''), coalesce(item_id, ''), coalesce(event_id, ''), created_at
	FROM message_routes
	WHERE thread_id = ? AND event_id = ?`
	args := []any{threadID, eventID}
	if strings.TrimSpace(turnID) != "" {
		query += ` AND coalesce(turn_id, '') = ?`
		args = append(args, strings.TrimSpace(turnID))
	}
	if strings.TrimSpace(itemID) != "" {
		query += ` AND coalesce(item_id, '') = ?`
		args = append(args, strings.TrimSpace(itemID))
	}
	query += ` ORDER BY created_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, args...)
	var route model.MessageRoute
	err := row.Scan(&route.ChatID, &route.TopicID, &route.MessageID, &route.ThreadID, &route.TurnID, &route.ItemID, &route.EventID, &route.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) ResolveExternalID(ctx context.Context, namespace, externalID string) (int64, error) {
	namespace = strings.TrimSpace(namespace)
	externalID = strings.TrimSpace(externalID)
	if namespace == "" || externalID == "" {
		return 0, nil
	}
	row := s.db.QueryRowContext(ctx, `
	SELECT numeric_id FROM external_id_maps WHERE namespace = ? AND external_id = ?`, namespace, externalID)
	var numericID int64
	err := row.Scan(&numericID)
	if err == nil {
		return numericID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	now := string(model.NowString())
	for attempt := uint64(0); attempt < 1024; attempt++ {
		numericID = stableExternalNumericID(namespace, externalID, attempt)
		_, err := s.db.ExecContext(ctx, `
		INSERT INTO external_id_maps(namespace, external_id, numeric_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, namespace, externalID, numericID, now, now)
		if err == nil {
			return numericID, nil
		}
		existing, lookupErr := s.externalIDForNumeric(ctx, namespace, numericID)
		if lookupErr != nil {
			return 0, errors.Join(err, lookupErr)
		}
		if existing == externalID {
			return numericID, nil
		}
		if existing != "" {
			continue
		}
		return 0, err
	}
	return 0, fmt.Errorf("external id mapping collision exhausted for %s:%s", namespace, externalID)
}

func (s *Store) externalIDForNumeric(ctx context.Context, namespace string, numericID int64) (string, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT external_id FROM external_id_maps WHERE namespace = ? AND numeric_id = ?`, namespace, numericID)
	var externalID string
	err := row.Scan(&externalID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return externalID, err
}

func stableExternalNumericID(namespace, externalID string, attempt uint64) int64 {
	var attemptBytes [8]byte
	binary.BigEndian.PutUint64(attemptBytes[:], attempt)
	sum := sha256.Sum256([]byte(namespace + "\x00" + externalID + "\x00" + string(attemptBytes[:])))
	value := int64(binary.BigEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
	if value == 0 {
		return 1
	}
	return value
}

func (s *Store) ExternalIDForNumeric(ctx context.Context, namespace string, numericID int64) (string, error) {
	return s.externalIDForNumeric(ctx, strings.TrimSpace(namespace), numericID)
}

func (s *Store) PutFeishuMessageMap(ctx context.Context, messageID int64, openMessageID string, chatID int64, openChatID string) error {
	openMessageID = strings.TrimSpace(openMessageID)
	openChatID = strings.TrimSpace(openChatID)
	if messageID == 0 || openMessageID == "" {
		return nil
	}
	now := string(model.NowString())
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO feishu_message_maps(message_id, open_message_id, chat_id, open_chat_id, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(message_id) DO UPDATE SET
		open_message_id = excluded.open_message_id,
		chat_id = excluded.chat_id,
		open_chat_id = excluded.open_chat_id,
		updated_at = excluded.updated_at`,
		messageID, openMessageID, chatID, openChatID, now, now)
	return err
}

func (s *Store) GetFeishuMessageByNumericID(ctx context.Context, messageID int64) (*model.FeishuMessageMap, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT message_id, open_message_id, chat_id, open_chat_id, created_at, updated_at
	FROM feishu_message_maps WHERE message_id = ?`, messageID)
	return scanFeishuMessageMap(row)
}

func (s *Store) GetFeishuMessageByOpenID(ctx context.Context, openMessageID string) (*model.FeishuMessageMap, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT message_id, open_message_id, chat_id, open_chat_id, created_at, updated_at
	FROM feishu_message_maps WHERE open_message_id = ?`, strings.TrimSpace(openMessageID))
	return scanFeishuMessageMap(row)
}

func scanFeishuMessageMap(scanner interface{ Scan(...any) error }) (*model.FeishuMessageMap, error) {
	var item model.FeishuMessageMap
	err := scanner.Scan(&item.MessageID, &item.OpenMessageID, &item.ChatID, &item.OpenChatID, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Store) UpsertFeishuThreadTopic(ctx context.Context, topic model.FeishuThreadTopic) error {
	topic.OpenChatID = strings.TrimSpace(topic.OpenChatID)
	topic.ThreadID = strings.TrimSpace(topic.ThreadID)
	topic.RootOpenMessageID = strings.TrimSpace(topic.RootOpenMessageID)
	topic.FeishuThreadID = strings.TrimSpace(topic.FeishuThreadID)
	if topic.ChatID == 0 || topic.ThreadID == "" || topic.RootMessageID == 0 || topic.RootOpenMessageID == "" {
		return nil
	}
	now := string(model.NowString())
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO feishu_thread_topics(chat_id, open_chat_id, codex_thread_id, root_message_id, root_open_message_id, feishu_thread_id, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(chat_id, codex_thread_id) DO UPDATE SET
		open_chat_id = excluded.open_chat_id,
		root_message_id = excluded.root_message_id,
		root_open_message_id = excluded.root_open_message_id,
		feishu_thread_id = excluded.feishu_thread_id,
		updated_at = excluded.updated_at`,
		topic.ChatID, topic.OpenChatID, topic.ThreadID, topic.RootMessageID, topic.RootOpenMessageID, nullable(topic.FeishuThreadID), now, now)
	return err
}

func (s *Store) GetFeishuThreadTopicByCodexThread(ctx context.Context, chatID int64, threadID string) (*model.FeishuThreadTopic, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, open_chat_id, codex_thread_id, root_message_id, root_open_message_id, coalesce(feishu_thread_id, ''), created_at, updated_at
	FROM feishu_thread_topics WHERE chat_id = ? AND codex_thread_id = ?`, chatID, strings.TrimSpace(threadID))
	return scanFeishuThreadTopic(row)
}

func (s *Store) GetAnyFeishuThreadTopicByCodexThread(ctx context.Context, threadID string) (*model.FeishuThreadTopic, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, open_chat_id, codex_thread_id, root_message_id, root_open_message_id, coalesce(feishu_thread_id, ''), created_at, updated_at
	FROM feishu_thread_topics WHERE codex_thread_id = ?
	ORDER BY updated_at DESC, created_at DESC LIMIT 1`, strings.TrimSpace(threadID))
	return scanFeishuThreadTopic(row)
}

func (s *Store) ListFeishuThreadTopics(ctx context.Context, limit int) ([]model.FeishuThreadTopic, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT chat_id, open_chat_id, codex_thread_id, root_message_id, root_open_message_id, coalesce(feishu_thread_id, ''), created_at, updated_at
	FROM feishu_thread_topics
	ORDER BY updated_at DESC, created_at DESC
	LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.FeishuThreadTopic{}
	for rows.Next() {
		var item model.FeishuThreadTopic
		if err := rows.Scan(&item.ChatID, &item.OpenChatID, &item.ThreadID, &item.RootMessageID, &item.RootOpenMessageID, &item.FeishuThreadID, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetFeishuThreadTopicByRootOpenMessageID(ctx context.Context, openChatID, rootOpenMessageID string) (*model.FeishuThreadTopic, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, open_chat_id, codex_thread_id, root_message_id, root_open_message_id, coalesce(feishu_thread_id, ''), created_at, updated_at
	FROM feishu_thread_topics WHERE open_chat_id = ? AND root_open_message_id = ?`,
		strings.TrimSpace(openChatID), strings.TrimSpace(rootOpenMessageID))
	return scanFeishuThreadTopic(row)
}

func (s *Store) GetFeishuThreadTopicByFeishuThreadID(ctx context.Context, openChatID, feishuThreadID string) (*model.FeishuThreadTopic, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, open_chat_id, codex_thread_id, root_message_id, root_open_message_id, coalesce(feishu_thread_id, ''), created_at, updated_at
	FROM feishu_thread_topics WHERE open_chat_id = ? AND feishu_thread_id = ?`,
		strings.TrimSpace(openChatID), strings.TrimSpace(feishuThreadID))
	return scanFeishuThreadTopic(row)
}

func scanFeishuThreadTopic(scanner interface{ Scan(...any) error }) (*model.FeishuThreadTopic, error) {
	var item model.FeishuThreadTopic
	err := scanner.Scan(&item.ChatID, &item.OpenChatID, &item.ThreadID, &item.RootMessageID, &item.RootOpenMessageID, &item.FeishuThreadID, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Store) PutCallbackRoute(ctx context.Context, route model.CallbackRoute) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT OR REPLACE INTO callback_routes(route_token, action, thread_id, turn_id, request_id, chat_message_id, status, expires_at, payload_json, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		route.Token, route.Action, route.ThreadID, nullable(route.TurnID), nullable(route.RequestID), route.ChatMessageID, route.Status, nullable(route.ExpiresAt), route.PayloadJSON, route.CreatedAt,
	)
	return err
}

func (s *Store) GetCallbackRoute(ctx context.Context, token string) (*model.CallbackRoute, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT route_token, action, thread_id, coalesce(turn_id,''), coalesce(request_id,''), coalesce(chat_message_id,0), status, coalesce(expires_at,''), payload_json, created_at
	FROM callback_routes WHERE route_token = ?`, token)
	var route model.CallbackRoute
	err := row.Scan(&route.Token, &route.Action, &route.ThreadID, &route.TurnID, &route.RequestID, &route.ChatMessageID, &route.Status, &route.ExpiresAt, &route.PayloadJSON, &route.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) ExpireCallbackRoute(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE callback_routes SET status = ? WHERE route_token = ?`, model.CallbackStatusExpired, token)
	return err
}

func (s *Store) SavePendingApproval(ctx context.Context, approval model.PendingApproval) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO pending_approvals(request_id, thread_id, turn_id, item_id, prompt_kind, question, status, chat_message_id, payload_json, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(request_id) DO UPDATE SET
		status = excluded.status,
		chat_message_id = excluded.chat_message_id,
		payload_json = excluded.payload_json,
		updated_at = excluded.updated_at`,
		approval.RequestID, approval.ThreadID, nullable(approval.TurnID), nullable(approval.ItemID), approval.PromptKind, nullable(approval.Question), approval.Status,
		approval.ChatMessageID, approval.PayloadJSON, approval.UpdatedAt,
	)
	return err
}

func (s *Store) GetPendingApproval(ctx context.Context, requestID string) (*model.PendingApproval, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT request_id, thread_id, coalesce(turn_id,''), coalesce(item_id,''), prompt_kind, coalesce(question,''), status, coalesce(chat_message_id,0), payload_json, updated_at
	FROM pending_approvals WHERE request_id = ?`, requestID)
	var approval model.PendingApproval
	err := row.Scan(&approval.RequestID, &approval.ThreadID, &approval.TurnID, &approval.ItemID, &approval.PromptKind, &approval.Question, &approval.Status, &approval.ChatMessageID, &approval.PayloadJSON, &approval.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &approval, nil
}

func (s *Store) UpdatePendingApprovalStatus(ctx context.Context, requestID, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pending_approvals SET status = ?, updated_at = ? WHERE request_id = ?`, status, string(model.NowString()), requestID)
	return err
}

func (s *Store) MarkAllPendingApprovals(ctx context.Context, status string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE pending_approvals SET status = ?, updated_at = ? WHERE status = 'pending'`, status, string(model.NowString()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) EnqueueDelivery(ctx context.Context, item model.DeliveryQueueItem) error {
	now := string(model.NowString())
	if item.AvailableAt == "" {
		item.AvailableAt = model.TimeString(now)
	}
	if item.CreatedAt == "" {
		item.CreatedAt = model.TimeString(now)
	}
	if item.UpdatedAt == "" {
		item.UpdatedAt = model.TimeString(now)
	}
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO delivery_queue(event_id, chat_key, chat_id, topic_id, thread_id, kind, status, retry_count, available_at, last_error, payload_json, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(event_id, chat_key) DO NOTHING`,
		item.EventID, item.ChatKey, item.ChatID, item.TopicID, item.ThreadID, item.Kind, item.Status, item.RetryCount, item.AvailableAt, nullable(item.LastError), item.PayloadJSON, item.CreatedAt, item.UpdatedAt,
	)
	return err
}

func (s *Store) ClaimDeliveryBatch(ctx context.Context, limit int) ([]model.DeliveryQueueItem, error) {
	if limit <= 0 {
		limit = 10
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	rows, err := tx.QueryContext(ctx, `
	SELECT id, event_id, chat_key, chat_id, topic_id, thread_id, kind, status, retry_count, available_at, coalesce(last_error,''), payload_json, created_at, updated_at
	FROM delivery_queue
	WHERE status IN ('pending', 'retry') AND available_at <= ?
	ORDER BY id
	LIMIT ?`, string(model.NowString()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DeliveryQueueItem{}
	ids := []int64{}
	for rows.Next() {
		var item model.DeliveryQueueItem
		if err := rows.Scan(&item.ID, &item.EventID, &item.ChatKey, &item.ChatID, &item.TopicID, &item.ThreadID, &item.Kind, &item.Status, &item.RetryCount, &item.AvailableAt, &item.LastError, &item.PayloadJSON, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
		ids = append(ids, item.ID)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status = ?, updated_at = ? WHERE id = ?`, model.DeliveryStatusProcessing, string(model.NowString()), id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) CompleteDelivery(ctx context.Context, queueID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE delivery_queue SET status = ?, updated_at = ? WHERE id = ?`, model.DeliveryStatusDelivered, string(model.NowString()), queueID)
	return err
}

func (s *Store) FailDelivery(ctx context.Context, queueID int64, retryCount int, availableAt time.Time, errText string, dead bool) error {
	status := model.DeliveryStatusRetry
	if dead {
		status = model.DeliveryStatusDead
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE delivery_queue SET status = ?, retry_count = ?, available_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, retryCount, availableAt.UTC().Format(time.RFC3339Nano), nullable(errText), string(model.NowString()), queueID,
	)
	return err
}

func (s *Store) RecordDeliveryAttempt(ctx context.Context, queueID int64, attemptNo int, status, errText string) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO delivery_attempts(queue_id, attempt_no, status, error_text, created_at)
	VALUES (?, ?, ?, ?, ?)`,
		queueID, attemptNo, status, nullable(errText), string(model.NowString()),
	)
	return err
}

func (s *Store) DeliveryQueueBacklog(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT count(*) FROM delivery_queue WHERE status IN ('pending', 'retry', 'processing')`)
	var count int
	return count, row.Scan(&count)
}

func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO daemon_state(key, value, updated_at) VALUES (?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, string(model.NowString()),
	)
	return err
}

func (s *Store) SetStateIfAbsent(ctx context.Context, key, value string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
	INSERT INTO daemon_state(key, value, updated_at) VALUES (?, ?, ?)
	ON CONFLICT(key) DO NOTHING`,
		key, value, string(model.NowString()),
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Store) GetState(ctx context.Context, key string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT value FROM daemon_state WHERE key = ?`, key)
	var value string
	err := row.Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) DeleteState(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM daemon_state WHERE key = ?`, key)
	return err
}

func (s *Store) ListState(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM daemon_state ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

func (s *Store) GetChatContext(ctx context.Context, chatID, topicID int64) (*model.ChatContext, error) {
	return &model.ChatContext{Mode: "unbound"}, nil
}

func scanThread(scanner interface{ Scan(...any) error }) (*model.Thread, error) {
	var thread model.Thread
	var cwd, directoryName, status, lastPreview, activeTurnID, preferredModel, permissionsMode sql.NullString
	var raw string
	var archived, listed int
	if err := scanner.Scan(&thread.ID, &thread.Title, &cwd, &thread.ProjectName, &directoryName, &thread.UpdatedAt, &status, &lastPreview, &activeTurnID, &preferredModel, &permissionsMode, &archived, &listed, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	thread.CWD = cwd.String
	thread.DirectoryName = directoryName.String
	thread.Status = status.String
	thread.LastPreview = lastPreview.String
	thread.ActiveTurnID = activeTurnID.String
	thread.PreferredModel = preferredModel.String
	thread.PermissionsMode = permissionsMode.String
	thread.Archived = archived == 1
	thread.Listed = listed == 1
	thread.Raw = json.RawMessage(raw)
	return &thread, nil
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func MustJSON(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("marshal payload: %w", err))
	}
	return string(payload)
}

package appserver

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const threadHistoryReadTimeout = 5 * time.Second

// TurnResult is the authoritative lifecycle record for one turn. The
// thread_history database is shared by every app-server process, so it is a
// more reliable terminal signal than a thread/read projection from another
// client.
type TurnResult struct {
	TurnID    string
	Status    string
	FinalText string
}

// ReadTurnResult reads the turn status and, when present, the explicit final
// answer item from the shared thread history database.
func ReadTurnResult(ctx context.Context, codexHome, threadID, turnID string) (*TurnResult, error) {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" || turnID == "" {
		return nil, nil
	}
	dbPath, err := findVersionedDB(codexHome, "thread_history")
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithTimeout(ctx, threadHistoryReadTimeout)
	defer cancel()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(readCtx, `PRAGMA query_only=ON`); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(readCtx, `PRAGMA busy_timeout=5000`); err != nil {
		return nil, err
	}
	if !tableExists(readCtx, db, "thread_turns") {
		return nil, nil
	}
	var status string
	var finalItemID string
	err = db.QueryRowContext(readCtx, `
		SELECT status, coalesce(final_agent_item_id, '')
		FROM thread_turns
		WHERE thread_id = ? AND turn_id = ?`,
		threadID, turnID).Scan(&status, &finalItemID)
	if errorsIsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := &TurnResult{TurnID: turnID, Status: strings.TrimSpace(status)}
	if strings.EqualFold(result.Status, "inProgress") || strings.TrimSpace(finalItemID) == "" {
		return result, nil
	}
	if !tableExists(readCtx, db, "thread_items") {
		return result, nil
	}
	finalText, err := readThreadItemText(readCtx, db, threadID, turnID, finalItemID)
	if err != nil {
		return result, nil
	}
	result.FinalText = strings.TrimSpace(finalText)
	return result, nil
}

// ReadRecentCompletedTurns reads the last completed turns of a thread from the
// Codex app-server history database. The app-server stores turn items there
// even when thread/read returns turns without items, which is the case in
// recent CLI builds.
func ReadRecentCompletedTurns(ctx context.Context, codexHome, threadID string, limit int) ([]CompletedTurnMessage, error) {
	if limit <= 0 || strings.TrimSpace(threadID) == "" {
		return nil, nil
	}
	dbPath, err := findVersionedDB(codexHome, "thread_history")
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithTimeout(ctx, threadHistoryReadTimeout)
	defer cancel()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(readCtx, `PRAGMA query_only=ON`); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(readCtx, `PRAGMA busy_timeout=5000`); err != nil {
		return nil, err
	}
	if !tableExists(readCtx, db, "thread_turns") || !tableExists(readCtx, db, "thread_items") {
		return nil, nil
	}
	rows, err := db.QueryContext(readCtx, `
		SELECT turn_id, first_user_item_id, final_agent_item_id
		FROM thread_turns
		WHERE thread_id = ? AND status = 'completed'
			AND coalesce(first_user_item_id, '') != ''
			AND coalesce(final_agent_item_id, '') != ''
		ORDER BY rollout_ordinal DESC
		LIMIT ?`, strings.TrimSpace(threadID), limit*3)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type turnRef struct {
		turnID  string
		userID  string
		finalID string
	}
	refs := []turnRef{}
	for rows.Next() {
		var ref turnRef
		if err := rows.Scan(&ref.turnID, &ref.userID, &ref.finalID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	turns := make([]CompletedTurnMessage, 0, limit)
	for _, ref := range refs {
		if len(turns) >= limit {
			break
		}
		userText, err := readThreadItemText(readCtx, db, threadID, ref.turnID, ref.userID)
		if err != nil {
			return nil, err
		}
		finalText, err := readThreadItemText(readCtx, db, threadID, ref.turnID, ref.finalID)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(userText) == "" || strings.TrimSpace(finalText) == "" {
			continue
		}
		turns = append(turns, CompletedTurnMessage{
			TurnID:     ref.turnID,
			UserItemID: ref.userID,
			UserText:   userText,
			FinalText:  finalText,
		})
	}
	// Newest first from SQL; replay in chronological order.
	for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
		turns[left], turns[right] = turns[right], turns[left]
	}
	return turns, nil
}

// HydrateThreadReadItems fills the latest turn's items from the app-server
// history database when thread/read returns turns without items. Recent CLI
// builds do that, which otherwise leaves panels with status text only.
func HydrateThreadReadItems(ctx context.Context, codexHome string, result map[string]any) map[string]any {
	if len(result) == 0 {
		return result
	}
	thread, _ := result["thread"].(map[string]any)
	if thread == nil {
		thread = result
	}
	threadID := strings.TrimSpace(stringValue(thread["id"], stringValue(thread["threadId"], "")))
	turns, _ := thread["turns"].([]any)
	if threadID == "" || len(turns) == 0 {
		return result
	}
	last, _ := turns[len(turns)-1].(map[string]any)
	if last == nil {
		return result
	}
	if items, ok := last["items"].([]any); ok && len(items) > 0 {
		return result
	}
	turnID := strings.TrimSpace(stringValue(last["id"], ""))
	if turnID == "" {
		return result
	}
	dbPath, err := findVersionedDB(codexHome, "thread_history")
	if err != nil {
		return result
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return result
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(readCtx, `PRAGMA query_only=ON`); err != nil {
		return result
	}
	items := readThreadItems(readCtx, db, threadID, turnID)
	if len(items) == 0 {
		// The history projection can lag the live turn; the rollout file is
		// written as the turn runs.
		items = readTurnItemsFromRollout(stringValue(thread["path"], ""), turnID)
	}
	if len(items) == 0 {
		return result
	}
	if strings.EqualFold(strings.TrimSpace(stringValue(last["status"], "")), "completed") {
		markFinalAgentItem(items)
	}
	last["items"] = items
	return result
}

func markFinalAgentItem(items []any) {
	lastIndex := -1
	hasFinal := false
	for index, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil || stringValue(item["type"], "") != "agentMessage" {
			continue
		}
		lastIndex = index
		if strings.EqualFold(stringValue(item["phase"], ""), "final_answer") {
			hasFinal = true
		}
	}
	if hasFinal || lastIndex < 0 {
		return
	}
	if item, ok := items[lastIndex].(map[string]any); ok {
		item["phase"] = "final_answer"
	}
}

const rolloutHydrateTailBytes = 4 << 20

// readTurnItemsFromRollout rebuilds a turn's items from the rollout JSONL.
// Only the tail is scanned because the latest turn lives at the end.
func readTurnItemsFromRollout(path, turnID string) []any {
	path = strings.TrimSpace(path)
	turnID = strings.TrimSpace(turnID)
	if path == "" || turnID == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	offset := int64(0)
	if info.Size() > rolloutHydrateTailBytes {
		offset = info.Size() - rolloutHydrateTailBytes
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	reader := bufio.NewReaderSize(file, 256*1024)
	if offset > 0 {
		_, _ = reader.ReadString('\n')
	}
	items := []any{}
	index := map[string]int{}
	currentTurn := ""
	for {
		line, readErr := reader.ReadString('\n')
		if payload := parseRolloutEvent(line); payload != nil {
			switch stringValue(payload["type"], "") {
			case "task_started":
				if attribution, ok := payload["turn_attribution"].(map[string]any); ok {
					currentTurn = stringValue(attribution["turn_id"], "")
				}
			case "item_completed":
				if stringValue(payload["turn_id"], "") != turnID {
					continue
				}
				item, _ := payload["item"].(map[string]any)
				if item == nil {
					continue
				}
				normalized := normalizeRolloutItem(item)
				id := stringValue(normalized["id"], "")
				if position, exists := index[id]; exists && id != "" {
					items[position] = normalized
					continue
				}
				if id != "" {
					index[id] = len(items)
				}
				items = append(items, normalized)
			case "user_message":
				if currentTurn != "" && currentTurn != turnID {
					continue
				}
				text := strings.TrimSpace(stringValue(payload["message"], ""))
				if text == "" || itemsContainText(items, "userMessage", text) {
					continue
				}
				id := fmt.Sprintf("rollout-user-%d", len(items))
				items = append(items, map[string]any{
					"type":    "userMessage",
					"id":      id,
					"content": []any{map[string]any{"type": "text", "text": text}},
				})
			case "agent_message":
				if currentTurn != "" && currentTurn != turnID {
					continue
				}
				text := strings.TrimSpace(stringValue(payload["message"], ""))
				if text == "" || itemsContainText(items, "agentMessage", text) {
					continue
				}
				item := map[string]any{
					"type": "agentMessage",
					"id":   fmt.Sprintf("rollout-agent-%d", len(items)),
					"text": text,
				}
				if phase := stringValue(payload["phase"], ""); phase != "" {
					item["phase"] = phase
				}
				items = append(items, item)
			case "message":
				// response_item entries carry the assistant text in rollout
				// files where agent_message events are sparse.
				if !strings.EqualFold(stringValue(payload["role"], ""), "assistant") {
					continue
				}
				if currentTurn != "" && currentTurn != turnID {
					continue
				}
				text := assistantMessageText(payload)
				if text == "" || itemsContainText(items, "agentMessage", text) {
					continue
				}
				items = append(items, map[string]any{
					"type": "agentMessage",
					"id":   firstNonEmptyString(stringValue(payload["id"], ""), fmt.Sprintf("rollout-agent-%d", len(items))),
					"text": text,
				})
			}
		}
		if readErr != nil {
			break
		}
	}
	return items
}

func assistantMessageText(payload map[string]any) string {
	contents, _ := payload["content"].([]any)
	parts := make([]string, 0, len(contents))
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		if content == nil {
			continue
		}
		switch stringValue(content["type"], "") {
		case "output_text", "text":
			if text := strings.TrimSpace(stringValue(content["text"], "")); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func parseRolloutEvent(line string) map[string]any {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return nil
	}
	var entry struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil
	}
	if (entry.Type != "event_msg" && entry.Type != "response_item") || entry.Payload == nil {
		return nil
	}
	return entry.Payload
}

// normalizeRolloutItem maps rollout item type casing to the app-server shape.
func normalizeRolloutItem(item map[string]any) map[string]any {
	out := make(map[string]any, len(item))
	for key, value := range item {
		out[key] = value
	}
	itemType := strings.TrimSpace(stringValue(out["type"], ""))
	if itemType != "" {
		runes := []rune(itemType)
		runes[0] = []rune(strings.ToLower(string(runes[0])))[0]
		out["type"] = string(runes)
	}
	return out
}

func itemsContainText(items []any, itemType, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil || stringValue(item["type"], "") != itemType {
			continue
		}
		if itemType == "agentMessage" {
			if strings.TrimSpace(stringValue(item["text"], "")) == text {
				return true
			}
			continue
		}
		contents, _ := item["content"].([]any)
		for _, rawContent := range contents {
			content, _ := rawContent.(map[string]any)
			if content != nil && strings.TrimSpace(stringValue(content["text"], "")) == text {
				return true
			}
		}
	}
	return false
}

func readThreadItems(ctx context.Context, db *sql.DB, threadID, turnID string) []any {
	if !tableExists(ctx, db, "thread_items") {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT item_json FROM thread_items
		WHERE thread_id = ? AND turn_id = ?
		ORDER BY rollout_ordinal`, threadID, turnID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(raw), &item); err != nil || item == nil {
			continue
		}
		items = append(items, item)
	}
	return items
}

func readThreadItemText(ctx context.Context, db *sql.DB, threadID, turnID, itemID string) (string, error) {
	var raw string
	err := db.QueryRowContext(ctx, `
		SELECT item_json FROM thread_items
		WHERE thread_id = ? AND turn_id = ? AND item_id = ?`,
		threadID, turnID, itemID).Scan(&raw)
	if errorsIsNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return "", nil
	}
	items := []any{item}
	if strings.TrimSpace(stringValue(item["type"], "")) == "userMessage" {
		_, text, _, _ := latestUserMessage(items)
		return text, nil
	}
	text, _ := latestFinalAgentMessage(items)
	return text, nil
}

func errorsIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}

// findVersionedDB picks the newest <prefix>_<n>.sqlite under the Codex home.
func findVersionedDB(codexHome, prefix string) (string, error) {
	home := strings.TrimSpace(codexHome)
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".codex")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", err
	}
	bestPath := ""
	bestVersion := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix+"_") || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		middle := strings.TrimSuffix(strings.TrimPrefix(name, prefix+"_"), ".sqlite")
		version, err := strconv.Atoi(middle)
		if err != nil {
			continue
		}
		if version > bestVersion {
			bestVersion = version
			bestPath = filepath.Join(home, name)
		}
	}
	if bestPath == "" {
		return "", fmt.Errorf("%s_*.sqlite not found in %s", prefix, home)
	}
	return bestPath, nil
}

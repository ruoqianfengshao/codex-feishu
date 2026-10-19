package appserver

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/ruoqianfengshao/codex-feishu/internal/model"
)

const (
	stateCatalogTimeout      = 5 * time.Second
	stateCatalogPreviewLimit = 80
)

// StateProject is one project entry from the Codex app-server state database.
type StateProject struct {
	ID        string
	Name      string
	Position  int
	RootPaths []string
}

// StateCatalog is the app-server's own catalog of projects and threads.
type StateCatalog struct {
	DBPath   string
	Projects []StateProject
	Threads  []model.Thread
}

// ReadStateCatalog reads the Codex app-server state database that backs the
// desktop sidebar. It returns the active threads and the project registry.
func ReadStateCatalog(ctx context.Context, codexHome string) (StateCatalog, error) {
	dbPath, err := findAppServerStateDB(codexHome)
	if err != nil {
		return StateCatalog{}, err
	}
	readCtx, cancel := context.WithTimeout(ctx, stateCatalogTimeout)
	defer cancel()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return StateCatalog{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(readCtx, `PRAGMA query_only=ON`); err != nil {
		return StateCatalog{}, err
	}
	if _, err := db.ExecContext(readCtx, `PRAGMA busy_timeout=5000`); err != nil {
		return StateCatalog{}, err
	}
	projects, err := readStateProjects(readCtx, db)
	if err != nil {
		return StateCatalog{}, err
	}
	threads, err := readStateThreads(readCtx, db, projects)
	if err != nil {
		return StateCatalog{}, err
	}
	return StateCatalog{DBPath: dbPath, Projects: projects, Threads: threads}, nil
}

// FindStateDB returns the newest Codex app-server state database under home.
func FindStateDB(codexHome string) (string, error) {
	return findAppServerStateDB(codexHome)
}

func findAppServerStateDB(codexHome string) (string, error) {
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
	type candidate struct {
		path    string
		version int
		modTime time.Time
	}
	best := candidate{version: -1}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "state") || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		version := 0
		if middle := strings.TrimSuffix(strings.TrimPrefix(name, "state"), ".sqlite"); middle != "" {
			if !strings.HasPrefix(middle, "_") {
				continue
			}
			parsed, err := strconv.Atoi(strings.TrimPrefix(middle, "_"))
			if err != nil {
				continue
			}
			version = parsed
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if version > best.version || (version == best.version && info.ModTime().After(best.modTime)) {
			best = candidate{path: filepath.Join(home, name), version: version, modTime: info.ModTime()}
		}
	}
	if best.path == "" {
		return "", fmt.Errorf("codex app-server state database not found in %s", home)
	}
	return best.path, nil
}

func readStateProjects(ctx context.Context, db *sql.DB) ([]StateProject, error) {
	if !tableExists(ctx, db, "projects") {
		return nil, nil
	}
	roots := map[string][]string{}
	if tableExists(ctx, db, "project_roots") {
		rows, err := db.QueryContext(ctx, `SELECT project_id, path FROM project_roots ORDER BY project_id, position`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var projectID, root string
			if err := rows.Scan(&projectID, &root); err != nil {
				return nil, err
			}
			projectID = strings.TrimSpace(projectID)
			root = model.NormalizePath(root)
			if projectID == "" || root == "" {
				continue
			}
			roots[projectID] = append(roots[projectID], root)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	columns := tableColumns(ctx, db, "projects")
	if len(columns) == 0 {
		return nil, nil
	}
	nameExpr := "NULL"
	if columns["name"] {
		nameExpr = "name"
	}
	positionExpr := "NULL"
	if columns["position"] {
		positionExpr = "position"
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT id, %s, %s FROM projects ORDER BY %s, id`, nameExpr, positionExpr, positionExpr))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []StateProject{}
	for rows.Next() {
		var id string
		var name sql.NullString
		var position sql.NullInt64
		if err := rows.Scan(&id, &name, &position); err != nil {
			return nil, err
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		project := StateProject{
			ID:        id,
			Name:      strings.TrimSpace(name.String),
			Position:  int(position.Int64),
			RootPaths: roots[id],
		}
		if project.Name == "" && len(project.RootPaths) > 0 {
			project.Name = filepath.Base(project.RootPaths[0])
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(projects, func(i, j int) bool {
		if projects[i].Position != projects[j].Position {
			return projects[i].Position < projects[j].Position
		}
		return strings.ToLower(projects[i].Name) < strings.ToLower(projects[j].Name)
	})
	return projects, nil
}

func readStateThreads(ctx context.Context, db *sql.DB, projects []StateProject) ([]model.Thread, error) {
	columns := tableColumns(ctx, db, "threads")
	if len(columns) == 0 {
		return nil, fmt.Errorf("codex app-server state: threads table not found")
	}
	expr := func(name string) string {
		if columns[name] {
			return name
		}
		return "NULL"
	}
	archivedExpr := "0"
	if columns["archived"] {
		archivedExpr = "coalesce(archived, 0)"
	}
	orderExpr := "id"
	switch {
	case columns["updated_at_ms"] && columns["updated_at"]:
		orderExpr = "coalesce(updated_at_ms, updated_at * 1000)"
	case columns["updated_at_ms"]:
		orderExpr = "updated_at_ms"
	case columns["updated_at"]:
		orderExpr = "updated_at"
	}
	query := fmt.Sprintf(`SELECT id, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s FROM threads WHERE %s = 0 ORDER BY %s DESC`,
		expr("name"), expr("title"), expr("cwd"), expr("updated_at"), expr("updated_at_ms"),
		expr("source"), expr("thread_source"), expr("agent_path"), expr("project_id"),
		expr("preview"), expr("first_user_message"), archivedExpr, orderExpr)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	threads := []model.Thread{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var (
			id               string
			name             sql.NullString
			title            sql.NullString
			cwd              sql.NullString
			updatedAt        sql.NullInt64
			updatedAtMS      sql.NullInt64
			source           sql.NullString
			threadSource     sql.NullString
			agentPath        sql.NullString
			projectID        sql.NullString
			preview          sql.NullString
			firstUserMessage sql.NullString
		)
		if err := rows.Scan(&id, &name, &title, &cwd, &updatedAt, &updatedAtMS, &source, &threadSource,
			&agentPath, &projectID, &preview, &firstUserMessage); err != nil {
			return nil, err
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if stateThreadIsHidden(source.String, threadSource.String, agentPath.String) {
			continue
		}
		updated := updatedAt.Int64
		if updatedAtMS.Int64 > 0 {
			updated = updatedAtMS.Int64 / 1000
		}
		threadCWD := model.NormalizePath(cwd.String)
		label := firstNonEmptyString(strings.TrimSpace(name.String), strings.TrimSpace(title.String))
		if label == "" {
			label = previewLine(firstUserMessage.String)
		}
		if label == "" {
			label = previewLine(preview.String)
		}
		thread := model.Thread{
			ID:          id,
			Title:       label,
			CWD:         threadCWD,
			UpdatedAt:   updated,
			LastPreview: previewLine(firstNonEmptyString(preview.String, firstUserMessage.String)),
			Listed:      true,
		}
		if project := matchStateProject(projects, projectID.String, threadCWD); project != nil {
			thread.ProjectName = project.Name
			thread.DirectoryName = filepath.Base(project.RootPaths[0])
		}
		seen[id] = struct{}{}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return threads, nil
}

// stateThreadIsHidden matches the app-server's interactive-thread view: one-off
// exec runs and subagent workers are automation, not conversation history.
func stateThreadIsHidden(source, threadSource, agentPath string) bool {
	if strings.TrimSpace(agentPath) != "" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(threadSource), "subagent") {
		return true
	}
	source = strings.TrimSpace(source)
	if strings.HasPrefix(source, `{"subagent"`) {
		return true
	}
	return strings.EqualFold(source, "exec")
}

func matchStateProject(projects []StateProject, projectID, cwd string) *StateProject {
	projectID = strings.TrimSpace(projectID)
	if projectID != "" {
		for index := range projects {
			if projects[index].ID == projectID {
				return &projects[index]
			}
		}
	}
	cwd = model.NormalizePath(cwd)
	if cwd == "" {
		return nil
	}
	var best *StateProject
	bestLen := -1
	for index := range projects {
		for _, root := range projects[index].RootPaths {
			if !pathWithinRoot(cwd, root) {
				continue
			}
			if len(root) > bestLen {
				best = &projects[index]
				bestLen = len(root)
			}
		}
	}
	return best
}

func pathWithinRoot(value, root string) bool {
	normalized := strings.ToLower(strings.TrimRight(model.NormalizePath(value), "/"))
	normalizedRoot := strings.ToLower(strings.TrimRight(model.NormalizePath(root), "/"))
	if normalized == "" || normalizedRoot == "" {
		return false
	}
	return normalized == normalizedRoot || strings.HasPrefix(normalized, normalizedRoot+"/")
}

func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func tableColumns(ctx context.Context, db *sql.DB, table string) map[string]bool {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid       int
			name      string
			columnTyp string
			notNull   int
			defaultV  sql.NullString
			primary   int
		)
		if err := rows.Scan(&cid, &name, &columnTyp, &notNull, &defaultV, &primary); err != nil {
			return nil
		}
		columns[strings.TrimSpace(name)] = true
	}
	return columns
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func previewLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > stateCatalogPreviewLimit {
			runes := []rune(line)
			line = string(runes[:stateCatalogPreviewLimit]) + "…"
		}
		return line
	}
	return ""
}

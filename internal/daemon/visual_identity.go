package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ruoqianfengshao/codex-feishu/internal/model"
)

const (
	visualMarkerStatePrefix = "ui.thread_marker."
	visualMarkerTTL         = 30 * time.Minute
	visualProjectMaxRunes   = 18
	visualThreadMaxRunes    = 30
)

var visualMarkerPalette = []string{
	"🟦", "🟩", "🟨", "🟧", "🟥", "🟪", "⬛", "⬜", "🟫",
	"🔵", "🟢", "🟡", "🟠", "🔴", "🟣", "⚫", "⚪", "🟤",
	"💙", "💚", "💛", "🧡", "❤️", "💜", "🖤", "🤍", "🤎", "🩷", "🩵", "🩶",
}

type visualMarkerAssignment struct {
	Marker        string `json:"marker"`
	ExpiresAtUnix int64  `json:"expires_at_unix"`
}

func (s *Service) visualHeader(ctx context.Context, kind string, thread model.Thread, turnID string) string {
	marker := s.visualMarker(ctx, thread.ID)
	project := s.visualProjectLabel(thread)
	title := compactVisualLabel(firstNonEmpty(thread.Title, thread.ShortID()), visualThreadMaxRunes)
	parts := []string{
		marker,
	}
	if project != "" {
		parts = append(parts, fmt.Sprintf("[%s]", project))
	}
	parts = append(parts,
		fmt.Sprintf("[%s]", title),
	)
	if kind = strings.TrimSpace(kind); kind != "" {
		parts = append(parts, fmt.Sprintf("[%s]", kind))
	}
	return strings.Join(parts, " ")
}

func (s *Service) visualFileHeader(thread model.Thread, turnID, kind string) string {
	marker := visualMarkerPalette[visualHashIndex(thread.ID, len(visualMarkerPalette))]
	project := s.visualProjectLabel(thread)
	title := compactVisualLabel(firstNonEmpty(thread.Title, thread.ShortID()), visualThreadMaxRunes)
	parts := []string{
		marker,
	}
	if project != "" {
		parts = append(parts, fmt.Sprintf("[%s]", project))
	}
	parts = append(parts,
		fmt.Sprintf("[%s]", title),
	)
	if kind = strings.TrimSpace(kind); kind != "" {
		parts = append(parts, fmt.Sprintf("[%s]", kind))
	}
	return strings.Join(parts, " ")
}

func (s *Service) visualDividerText(ctx context.Context, thread model.Thread, turnID string) string {
	marker := s.visualMarker(ctx, thread.ID)
	project := s.visualProjectLabel(thread)
	title := compactVisualLabel(firstNonEmpty(thread.Title, thread.ShortID()), visualThreadMaxRunes)
	parts := []string{
		marker,
		"New run:",
	}
	if project != "" {
		parts = append(parts, fmt.Sprintf("[%s]", project))
	}
	parts = append(parts,
		fmt.Sprintf("[%s]", title),
	)
	return strings.Join(parts, " ")
}

func (s *Service) visualProjectLabel(thread model.Thread) string {
	if s.isCodexChatThread(thread) {
		return ""
	}
	project := strings.TrimSpace(thread.ProjectName)
	if project == "" || strings.EqualFold(project, "Project") || strings.EqualFold(project, "Shared/General") || strings.EqualFold(project, chatsProjectName) {
		return ""
	}
	return compactVisualLabel(project, visualProjectMaxRunes)
}

func (s *Service) visualMarker(ctx context.Context, threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return visualMarkerPalette[0]
	}
	fallback := visualMarkerPalette[visualHashIndex(threadID, len(visualMarkerPalette))]
	now := time.Now().UTC()
	states, err := s.store.ListState(ctx)
	if err != nil {
		return fallback
	}
	assignments := map[string]visualMarkerAssignment{}
	occupied := map[string]string{}
	for key, raw := range states {
		if !strings.HasPrefix(key, visualMarkerStatePrefix) {
			continue
		}
		owner := strings.TrimPrefix(key, visualMarkerStatePrefix)
		var assignment visualMarkerAssignment
		if err := json.Unmarshal([]byte(raw), &assignment); err != nil {
			continue
		}
		if strings.TrimSpace(assignment.Marker) == "" || assignment.ExpiresAtUnix <= now.Unix() {
			continue
		}
		assignments[owner] = assignment
		if owner != threadID {
			occupied[assignment.Marker] = owner
		}
	}
	marker := ""
	if existing, ok := assignments[threadID]; ok && occupied[existing.Marker] == "" {
		marker = existing.Marker
	}
	if marker == "" {
		marker = chooseVisualMarker(threadID, occupied)
	}
	payload, err := json.Marshal(visualMarkerAssignment{
		Marker:        marker,
		ExpiresAtUnix: now.Add(visualMarkerTTL).Unix(),
	})
	if err == nil {
		_ = s.store.SetState(ctx, visualMarkerStatePrefix+threadID, string(payload))
	}
	return marker
}

func chooseVisualMarker(threadID string, occupied map[string]string) string {
	start := visualHashIndex(threadID, len(visualMarkerPalette))
	for offset := 0; offset < len(visualMarkerPalette); offset++ {
		marker := visualMarkerPalette[(start+offset)%len(visualMarkerPalette)]
		if occupied[marker] == "" {
			return marker
		}
	}
	base := visualMarkerPalette[start]
	for suffix := 2; ; suffix++ {
		marker := fmt.Sprintf("%s#%d", base, suffix)
		if occupied[marker] == "" {
			return marker
		}
	}
}

func visualHashIndex(value string, modulo int) int {
	if modulo <= 0 {
		return 0
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(value))
	return int(hasher.Sum32() % uint32(modulo))
}

func compactVisualLabel(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" {
		return "-"
	}
	if maxRunes <= 3 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}

func visualShortID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if segment, _, ok := strings.Cut(value, "-"); ok && segment != "" {
		value = segment
	}
	if strings.HasPrefix(value, "019d") && len(value) >= 8 {
		return value[len(value)-4:]
	}
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

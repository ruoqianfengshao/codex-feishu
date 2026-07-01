package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ruoqianfengshao/codex-feishu/internal/model"
)

func TestVisualMarkerIsStableForSameThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	first := service.visualMarker(ctx, "thread-stable-marker")
	second := service.visualMarker(ctx, "thread-stable-marker")

	if first == "" || first != second {
		t.Fatalf("visual marker first=%q second=%q, want stable non-empty marker", first, second)
	}
}

func TestVisualMarkerAvoidsActiveCollision(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	firstID := "thread-collision-a"
	secondID := findThreadIDWithVisualBase(t, visualHashIndex(firstID, len(visualMarkerPalette)), firstID)

	first := service.visualMarker(ctx, firstID)
	second := service.visualMarker(ctx, secondID)

	if first == second {
		t.Fatalf("visual markers collided for active threads %q and %q: %q", firstID, secondID, first)
	}
}

func TestVisualMarkerUsesSuffixWhenPaletteIsExhausted(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	expires := time.Now().UTC().Add(visualMarkerTTL).Unix()
	for index, marker := range visualMarkerPalette {
		payload, err := json.Marshal(visualMarkerAssignment{Marker: marker, ExpiresAtUnix: expires})
		if err != nil {
			t.Fatalf("marshal assignment failed: %v", err)
		}
		if err := service.store.SetState(ctx, fmt.Sprintf("%sowner-%d", visualMarkerStatePrefix, index), string(payload)); err != nil {
			t.Fatalf("SetState failed: %v", err)
		}
	}

	marker := service.visualMarker(ctx, "thread-overflow-marker")
	if !strings.Contains(marker, "#2") {
		t.Fatalf("overflow marker = %q, want suffixed marker when palette is exhausted", marker)
	}
}

func TestVisualMarkerReusesExpiredAssignment(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	threadID := "thread-expired-marker"
	base := visualMarkerPalette[visualHashIndex(threadID, len(visualMarkerPalette))]
	payload, err := json.Marshal(visualMarkerAssignment{Marker: base, ExpiresAtUnix: time.Now().UTC().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatalf("marshal assignment failed: %v", err)
	}
	if err := service.store.SetState(ctx, visualMarkerStatePrefix+"expired-owner", string(payload)); err != nil {
		t.Fatalf("SetState failed: %v", err)
	}

	marker := service.visualMarker(ctx, threadID)
	if marker != base {
		t.Fatalf("marker = %q, want expired base marker %q to be reusable", marker, base)
	}
}

func TestVisualHeaderShowsProjectSegmentForWorkspaceThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	header := service.visualHeader(context.Background(), "Final", model.Thread{
		ID:          "thread-header-project",
		Title:       "Implement Feishu bridge",
		ProjectName: "codex-feishu-controller",
		CWD:         "/Users/example/workspace/codex-feishu-controller",
	}, "turn-header-project")

	if !strings.Contains(header, "[codex-feishu-co...]") {
		t.Fatalf("header = %q, want project segment", header)
	}
	if !strings.Contains(header, "[Implement Feishu bridge]") || strings.Contains(header, "[T:") || strings.Contains(header, "[R:") || !strings.Contains(header, "[Final]") {
		t.Fatalf("header = %q, want title/kind segments without thread/turn ids", header)
	}
}

func TestVisualHeaderOmitsProjectSegmentForCodexChatThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	header := service.visualHeader(context.Background(), "Final", model.Thread{
		ID:          "thread-header-chat",
		Title:       "019efe1a-c0e7-7722-adf5-f036e91d7ec1",
		ProjectName: "scratch",
		CWD:         "/Users/example/Documents/Codex/2026-06-25/scratch",
	}, "turn-header-chat")

	if strings.Contains(header, "[scratch]") {
		t.Fatalf("header = %q, want no Codex chat project segment", header)
	}
	if !strings.Contains(header, "[019efe1a-c0e7-7722-adf5-f03...]") || strings.Contains(header, "[T:") || strings.Contains(header, "[R:") || !strings.Contains(header, "[Final]") {
		t.Fatalf("header = %q, want title/kind segments without thread/turn ids", header)
	}
}

func TestVisualHeaderShowsSameNameWhenThreadIsRealWorkspace(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	header := service.visualHeader(context.Background(), "Final", model.Thread{
		ID:          "thread-header-real-sample",
		Title:       "IAM workspace",
		ProjectName: "sample-project",
		CWD:         "/Users/example/workspace/sample-project",
	}, "turn-header-real-sample")

	if !strings.Contains(header, "[sample-project]") {
		t.Fatalf("header = %q, want project segment for real workspace", header)
	}
}

func findThreadIDWithVisualBase(t *testing.T, baseIndex int, excluded string) string {
	t.Helper()
	for index := 0; index < 100000; index++ {
		candidate := fmt.Sprintf("thread-collision-candidate-%d", index)
		if candidate == excluded {
			continue
		}
		if visualHashIndex(candidate, len(visualMarkerPalette)) == baseIndex {
			return candidate
		}
	}
	t.Fatalf("could not find visual marker collision candidate for base index %d", baseIndex)
	return ""
}

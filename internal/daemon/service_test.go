package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruoqianfengshao/codex-feishu/internal/appserver"
	"github.com/ruoqianfengshao/codex-feishu/internal/config"
	"github.com/ruoqianfengshao/codex-feishu/internal/model"
	"github.com/ruoqianfengshao/codex-feishu/internal/updater"
)

func TestResolveRoutePrecedenceExplicitThenReplyThenNone(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 99,
		ThreadID:  "reply-thread",
		TurnID:    "reply-turn",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	explicit, err := service.resolveRouteFromSource(ctx, 123456789, 0, "explicit-thread", 99, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRoute(explicit) failed: %v", err)
	}
	if explicit.ThreadID != "explicit-thread" || explicit.Source != model.RouteSourceExplicit {
		t.Fatalf("explicit route = %#v, want explicit-thread / explicit", explicit)
	}

	reply, err := service.resolveRouteFromSource(ctx, 123456789, 0, "", 99, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRoute(reply) failed: %v", err)
	}
	if reply.ThreadID != "reply-thread" || reply.Source != model.RouteSourceReply {
		t.Fatalf("reply route = %#v, want reply-thread / reply", reply)
	}
	if reply.TurnID != "reply-turn" || reply.RequestID != "" {
		t.Fatalf("reply route turn/request = %#v, want reply-turn without request", reply)
	}

	none, err := service.resolveRouteFromSource(ctx, 123456789, 0, "", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRoute(none) failed: %v", err)
	}
	if none.ThreadID != "" || none.Source != model.RouteSourceNone {
		t.Fatalf("none route = %#v, want no route", none)
	}
}

func TestResolveRouteFallsBackToRootlessFeishuTopicReplyRoutesOnly(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 99,
		ThreadID:  "reply-thread",
		TurnID:    "reply-turn",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   555,
		MessageID: 99,
		ThreadID:  "stale-topic-thread",
		TurnID:    "stale-topic-turn",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute(stale topic) failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:        123456789,
		TopicID:       0,
		ProjectName:   "project",
		ThreadID:      "panel-thread",
		SourceMode:    model.PanelSourceFeishuInput,
		CurrentTurnID: "panel-turn",
		Status:        "completed",
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	reply, err := service.resolveRouteFromSource(ctx, 123456789, 555, "", 99, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRoute(reply fallback) failed: %v", err)
	}
	if reply.ThreadID != "reply-thread" || reply.TurnID != "reply-turn" || reply.Source != model.RouteSourceReply {
		t.Fatalf("reply route = %#v, want canonical topic_id=0 reply route", reply)
	}

	panel, err := service.resolveRouteFromSource(ctx, 123456789, 555, "", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRoute(panel isolation) failed: %v", err)
	}
	if panel.ThreadID != "" || panel.Source != model.RouteSourceNone {
		t.Fatalf("panel route = %#v, want no rootless topic panel fallback", panel)
	}
}

func TestResolveRouteFromFeishuPlainTextUsesLatestCurrentPanelOnly(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:        123456789,
		TopicID:       0,
		ProjectName:   "project",
		ThreadID:      "panel-thread",
		SourceMode:    model.PanelSourceExplicit,
		CurrentTurnID: "panel-turn",
		Status:        "inProgress",
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	feishu, err := service.resolveRouteFromSource(ctx, 123456789, 0, "", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRouteFromSource(feishu) failed: %v", err)
	}
	if feishu.ThreadID != "panel-thread" || feishu.Source != model.RouteSourcePanel || feishu.TurnID != "" {
		t.Fatalf("feishu route = %#v, want panel-thread / panel without turn route", feishu)
	}
}

func TestResolveRouteFromFeishuPlainTextKeepsExplicitReplyAndSteerPrecedence(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:      123456789,
		TopicID:     0,
		ProjectName: "project",
		ThreadID:    "panel-thread",
		SourceMode:  model.PanelSourceExplicit,
		Status:      "inProgress",
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 99,
		ThreadID:  "reply-thread",
		TurnID:    "reply-turn",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	if err := service.armSteer(ctx, 123456789, 0, "steer-thread", "steer-turn", 7); err != nil {
		t.Fatalf("armSteer failed: %v", err)
	}

	explicit, err := service.resolveRouteFromSource(ctx, 123456789, 0, "explicit-thread", 99, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRouteFromSource(explicit) failed: %v", err)
	}
	if explicit.ThreadID != "explicit-thread" || explicit.Source != model.RouteSourceExplicit {
		t.Fatalf("explicit route = %#v, want explicit-thread / explicit", explicit)
	}

	reply, err := service.resolveRouteFromSource(ctx, 123456789, 0, "", 99, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRouteFromSource(reply) failed: %v", err)
	}
	if reply.ThreadID != "reply-thread" || reply.TurnID != "reply-turn" || reply.Source != model.RouteSourceReply {
		t.Fatalf("reply route = %#v, want reply-thread / reply-turn / reply", reply)
	}

	steer, err := service.resolveRouteFromSource(ctx, 123456789, 0, "", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRouteFromSource(steer) failed: %v", err)
	}
	if steer.ThreadID != "steer-thread" || steer.Source != model.RouteSourceSteer {
		t.Fatalf("steer route = %#v, want steer-thread / steer", steer)
	}
}

func TestResolveRouteExtractsPlanRequestIDOnlyFromPlanRequestEvent(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 100,
		ThreadID:  "plan-thread",
		TurnID:    "plan-turn",
		EventID:   "plan_request:request-plan-1",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute(plan request) failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 101,
		ThreadID:  "synthetic-thread",
		TurnID:    "synthetic-turn",
		EventID:   "synthetic-plan-fp",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute(synthetic) failed: %v", err)
	}

	real, err := service.resolveRouteFromSource(ctx, 123456789, 0, "", 100, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRoute(real plan request) failed: %v", err)
	}
	if real.ThreadID != "plan-thread" || real.TurnID != "plan-turn" || real.RequestID != "request-plan-1" {
		t.Fatalf("real plan route = %#v, want thread/turn/request", real)
	}

	synthetic, err := service.resolveRouteFromSource(ctx, 123456789, 0, "", 101, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("resolveRoute(synthetic plan) failed: %v", err)
	}
	if synthetic.ThreadID != "synthetic-thread" || synthetic.TurnID != "synthetic-turn" || synthetic.RequestID != "" {
		t.Fatalf("synthetic plan route = %#v, want thread/turn without request", synthetic)
	}
}

func TestResolveArmedSteerReturnsActiveStateAndExpires(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	if err := service.armSteer(ctx, 123456789, 0, "steer-thread", "turn-9", 77); err != nil {
		t.Fatalf("armSteer failed: %v", err)
	}
	state, err := service.resolveArmedSteer(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("resolveArmedSteer(active) failed: %v", err)
	}
	if state == nil || state.ThreadID != "steer-thread" || state.TurnID != "turn-9" || state.PanelID != 77 {
		t.Fatalf("active steer state = %#v, want steer-thread/turn-9/panel 77", state)
	}

	if err := service.store.ArmSteerState(ctx, model.SteerState{
		ChatKey:   model.ChatKey(123456789, 0),
		ChatID:    123456789,
		TopicID:   0,
		ThreadID:  "expired-thread",
		TurnID:    "turn-old",
		PanelID:   88,
		ExpiresAt: model.TimeString(time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)),
		CreatedAt: model.NowString(),
		UpdatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("ArmSteerState(expired) failed: %v", err)
	}

	state, err = service.resolveArmedSteer(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("resolveArmedSteer(expired) failed: %v", err)
	}
	if state != nil {
		t.Fatalf("expired steer state = %#v, want nil", state)
	}
	loaded, err := service.store.GetSteerState(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("GetSteerState(after expired resolve) failed: %v", err)
	}
	if loaded != nil {
		t.Fatalf("stored steer state after expired resolve = %#v, want nil", loaded)
	}
}

func TestTrackedThreadsSkipsIdleRecentHistoryWithoutBindingsOrPanels(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC().Unix()

	idle := model.Thread{
		ID:          "idle-thread",
		Title:       "Idle history",
		ProjectName: "Codex",
		UpdatedAt:   now - 600,
		Status:      "idle",
	}
	active := model.Thread{
		ID:           "active-thread",
		Title:        "Active now",
		ProjectName:  "Codex",
		UpdatedAt:    now,
		Status:       "inProgress",
		ActiveTurnID: "turn-1",
	}
	if err := service.store.UpsertThread(ctx, idle); err != nil {
		t.Fatalf("UpsertThread(idle) failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, active); err != nil {
		t.Fatalf("UpsertThread(active) failed: %v", err)
	}

	tracked := service.trackedThreads(ctx, 10)
	ids := map[string]bool{}
	for _, thread := range tracked {
		ids[thread.ID] = true
	}

	if ids[idle.ID] {
		t.Fatalf("tracked threads unexpectedly include stale idle history: %#v", tracked)
	}
	if !ids[active.ID] {
		t.Fatalf("tracked threads do not include active thread: %#v", tracked)
	}
}

func TestThreadsOverviewHidesInternalSubAgentThreads(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.now = func() time.Time { return time.Unix(70, 0).UTC() }
	ctx := context.Background()
	visible := model.Thread{
		ID:            "visible-thread",
		Title:         "Visible work",
		ProjectName:   "codex-feishu",
		DirectoryName: "codex-feishu",
		UpdatedAt:     10,
		Status:        "idle",
		LastPreview:   "normal user request",
		Raw:           json.RawMessage(`{"id":"visible-thread","preview":"normal user request"}`),
	}
	internal := model.Thread{
		ID:            "01900000-0000-7000-8000-000000000014",
		Title:         "01900000-0000-7000-8000-000000000014",
		ProjectName:   "memories",
		DirectoryName: "memories",
		UpdatedAt:     20,
		Raw:           json.RawMessage(`{"thread":{"id":"01900000-0000-7000-8000-000000000014","ephemeral":true,"source":{"subAgent":"memory_consolidation"}}}`),
	}
	archived := model.Thread{
		ID:            "archived-thread",
		Title:         "Archived work",
		ProjectName:   "archive",
		DirectoryName: "archive",
		UpdatedAt:     30,
		Archived:      true,
		Raw:           json.RawMessage(`{"id":"archived-thread"}`),
	}
	notLoadedIDTitle := model.Thread{
		ID:            "019efe1a-c0e7-7722-adf5-f036e91d7ec1",
		Title:         "019efe1a-c0e7-7722-adf5-f036e91d7ec1",
		ProjectName:   "sample-app",
		DirectoryName: "sample-app",
		UpdatedAt:     60,
		Status:        "notLoaded",
		Raw:           json.RawMessage(`{"id":"019efe1a-c0e7-7722-adf5-f036e91d7ec1","name":null}`),
	}
	if err := service.store.UpsertThread(ctx, visible); err != nil {
		t.Fatalf("UpsertThread(visible) failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, internal); err != nil {
		t.Fatalf("UpsertThread(internal) failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, archived); err != nil {
		t.Fatalf("UpsertThread(archived) failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, notLoadedIDTitle); err != nil {
		t.Fatalf("UpsertThread(notLoadedIDTitle) failed: %v", err)
	}

	response, err := service.threadsOverview(ctx, "8")
	if err != nil {
		t.Fatalf("handleCommand(/chats) failed: %v", err)
	}
	if response == nil {
		t.Fatal("handleCommand(/chats) returned nil response")
	}
	if !strings.Contains(response.Text, "Visible work") {
		t.Fatalf("/chats text missing visible thread:\n%s", response.Text)
	}
	if strings.Contains(response.Text, "memories") || strings.Contains(response.Text, internal.ID) {
		t.Fatalf("/chats text contains internal thread:\n%s", response.Text)
	}
	if strings.Contains(response.Text, "Archived work") || strings.Contains(response.Text, archived.ID) {
		t.Fatalf("/chats text contains archived thread:\n%s", response.Text)
	}
	if strings.Contains(response.Text, "sample-app") || strings.Contains(response.Text, notLoadedIDTitle.ID) {
		t.Fatalf("/chats text contains unavailable id-title notLoaded thread:\n%s", response.Text)
	}
	if !strings.Contains(response.Text, "codex-feishu\nVisible work    1 分钟前") {
		t.Fatalf("/chats text missing grouped visible thread:\n%s", response.Text)
	}
	if strings.Contains(response.Text, "All chats") {
		t.Fatalf("/chats text contains removed heading:\n%s", response.Text)
	}
	if len(response.Buttons) != 1 || len(response.Buttons[0]) != 1 || response.Buttons[0][0].Text != "Open" {
		t.Fatalf("/chats buttons = %#v, want one Open button", response.Buttons)
	}
	if len(response.Sections) != 1 {
		t.Fatalf("/chats sections = %#v, want project section", response.Sections)
	}
	if response.Sections[0].Text != "codex-feishu" {
		t.Fatalf("/chats project section = %#v, want codex-feishu", response.Sections[0])
	}
	if !response.Sections[0].Heading {
		t.Fatalf("/chats project section = %#v, want heading section", response.Sections[0])
	}
	if len(response.Sections[0].Rows) != 1 || response.Sections[0].Rows[0].Title != "Visible work" || response.Sections[0].Rows[0].Trailing != "1 分钟前" || response.Sections[0].Rows[0].Button.Text != "Open" {
		t.Fatalf("/chats project section = %#v, want visible thread row with Open button", response.Sections[0])
	}
}

func TestThreadsOverviewGroupsThreadsByProject(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.now = func() time.Time { return time.Unix(200, 0).UTC() }
	ctx := context.Background()
	threads := []model.Thread{
		{
			ID:          "project-a-new",
			Title:       "A new",
			ProjectName: "project-a",
			UpdatedAt:   140,
			Raw:         json.RawMessage(`{"id":"project-a-new"}`),
		},
		{
			ID:          "temp-chat",
			Title:       "Temporary chat",
			CWD:         "/Users/example/Documents/Codex/2026-06-26/you",
			ProjectName: "you",
			UpdatedAt:   120,
			Raw:         json.RawMessage(`{"id":"temp-chat"}`),
		},
		{
			ID:          "project-a-old",
			Title:       "A old",
			ProjectName: "project-a",
			UpdatedAt:   80,
			Raw:         json.RawMessage(`{"id":"project-a-old"}`),
		},
		{
			ID:          "project-b",
			Title:       "B thread",
			ProjectName: "project-b",
			UpdatedAt:   60,
			Raw:         json.RawMessage(`{"id":"project-b"}`),
		},
	}
	for _, thread := range threads {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	response, err := service.threadsOverview(ctx, "8")
	if err != nil {
		t.Fatalf("handleCommand(/chats) failed: %v", err)
	}
	if response == nil {
		t.Fatal("handleCommand(/chats) returned nil response")
	}
	for _, needle := range []string{"project-a", "A new    1 分钟前", "A old    2 分钟前", "临时对话", "Temporary chat    1 分钟前", "project-b", "B thread    2 分钟前"} {
		if !strings.Contains(response.Text, needle) {
			t.Fatalf("/chats text missing %q:\n%s", needle, response.Text)
		}
	}
	requireTextOrder(t, response.Text, "project-a", "A new")
	requireTextOrder(t, response.Text, "A new", "A old")
	requireTextOrder(t, response.Text, "project-a", "临时对话")
	requireTextOrder(t, response.Text, "临时对话", "project-b")
	if len(response.Sections) != 3 {
		t.Fatalf("/chats sections = %#v, want three project sections", response.Sections)
	}
	if response.Sections[0].Divider || !response.Sections[1].Divider || !response.Sections[2].Divider {
		t.Fatalf("/chats sections = %#v, want dividers before second and later projects", response.Sections)
	}
	if len(response.Buttons) != 4 {
		t.Fatalf("/chats buttons = %#v, want one Open button per thread", response.Buttons)
	}
	for _, row := range response.Buttons {
		if len(row) != 1 || row[0].Text != "Open" {
			t.Fatalf("/chats button row = %#v, want single Open button", row)
		}
	}
}

func TestChatsCommandShowsGreenChatsProjectList(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.now = func() time.Time { return time.Unix(200, 0).UTC() }
	ctx := context.Background()
	stateBytes, err := json.Marshal(map[string]any{
		"pinned-thread-ids": []string{"chat-pinned"},
	})
	if err != nil {
		t.Fatalf("Marshal codex projects failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(service.codexGlobalStatePath), 0o700); err != nil {
		t.Fatalf("Mkdir codex global state dir failed: %v", err)
	}
	if err := os.WriteFile(service.codexGlobalStatePath, stateBytes, 0o600); err != nil {
		t.Fatalf("Write codex global state failed: %v", err)
	}
	for _, thread := range []model.Thread{
		{
			ID:          "project-thread",
			Title:       "Project thread",
			ProjectName: "project-a",
			CWD:         "/Users/example/project-a",
			UpdatedAt:   190,
			Raw:         json.RawMessage(`{"id":"project-thread"}`),
		},
		{
			ID:          "chat-regular",
			Title:       "Regular Chat",
			ProjectName: "Regular Chat",
			CWD:         filepath.Join(service.codexChatsRoot(), "2026-07-08", "regular-chat"),
			UpdatedAt:   180,
			Raw:         json.RawMessage(`{"id":"chat-regular"}`),
		},
		{
			ID:          "chat-pinned",
			Title:       "Pinned Chat",
			ProjectName: "Pinned Chat",
			CWD:         filepath.Join(service.codexChatsRoot(), "2026-07-08", "pinned-chat"),
			UpdatedAt:   170,
			Raw:         json.RawMessage(`{"id":"chat-pinned"}`),
		},
	} {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/chats", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/chats) failed: %v", err)
	}
	if response == nil {
		t.Fatal("handleCommand(/chats) returned nil response")
	}
	if !strings.Contains(response.Text, "Threads for [Pinned] Chats") || !strings.Contains(response.Text, "Regular Chat") || !strings.Contains(response.Text, "[Pinned] Pinned Chat") {
		t.Fatalf("/chats text =\n%s\nwant Chats list with pinned and regular chats", response.Text)
	}
	if strings.Contains(response.Text, "Project thread") {
		t.Fatalf("/chats text =\n%s\nwant project thread excluded", response.Text)
	}
	if len(response.Sections) != 1 || len(response.Sections[0].Rows) != 3 {
		t.Fatalf("/chats sections = %#v, want two chats plus new chat row", response.Sections)
	}
	rows := response.Sections[0].Rows
	if rows[0].BackgroundStyle != "cus-5" || rows[1].BackgroundStyle != "cus-6" || rows[2].BackgroundStyle != "cus-4" || rows[2].BorderColor != "cus-7" {
		t.Fatalf("/chats rows = %#v, want green chat rows and green-bordered new chat row", rows)
	}
}

func TestWorkspaceCommandReturnsDashboardCard(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "workspace-thread",
		Title:       "Workspace task",
		ProjectName: "Codex",
		UpdatedAt:   time.Now().UTC().Unix(),
		Raw:         json.RawMessage(`{"id":"workspace-thread"}`),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/start", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/start) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "Codex Workspace") || !strings.Contains(response.Text, "Cached threads: 1") {
		t.Fatalf("/start response = %#v, want dashboard card", response)
	}
	var labels []string
	for _, row := range response.Buttons {
		for _, button := range row {
			labels = append(labels, button.Text)
		}
	}
	for _, want := range []string{"Recent chats", "Projects", "Status", "Settings"} {
		if !containsString(labels, want) {
			t.Fatalf("/start buttons = %#v, missing %q", labels, want)
		}
	}
}

func TestWorkspaceCommandUsesEnglishLanguagePreference(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, botLanguageStateKey, botLanguageEnglish); err != nil {
		t.Fatalf("SetState(language) failed: %v", err)
	}
	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/start", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/start) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "Codex Workspace") || !strings.Contains(response.Text, "Cached threads: 0") {
		t.Fatalf("/start response = %#v, want English dashboard", response)
	}
}

func TestStatusCommandReturnsWorkspaceStatsCard(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	service.startedAt = time.Date(2026, time.June, 28, 9, 0, 0, 0, time.UTC)
	service.now = func() time.Time {
		return time.Date(2026, time.June, 28, 10, 2, 3, 0, time.UTC)
	}
	if err := service.store.SetState(ctx, botLanguageStateKey, botLanguageChinese); err != nil {
		t.Fatalf("SetState(language) failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:          "status-thread",
		Title:       "Status thread",
		CWD:         "/Users/example/status-project",
		ProjectName: "status-project",
		UpdatedAt:   service.now().Unix(),
		Raw:         json.RawMessage(`{"id":"status-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:          "status-chat",
		Title:       "Status chat",
		CWD:         config.DefaultCodexChatsRoot() + "/status-chat",
		ProjectName: "Chats",
		UpdatedAt:   service.now().Unix() - 1,
		Raw:         json.RawMessage(`{"id":"status-chat"}`),
	}); err != nil {
		t.Fatalf("UpsertThread(chat) failed: %v", err)
	}
	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/status", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/status) failed: %v", err)
	}
	if response == nil || response.Text != "Codex 状态总览" {
		t.Fatalf("/status response = %#v, want status card response", response)
	}
	if got, want := sectionTitles(response.Sections), []string{"健康", "会话", "Feishu"}; !equalStrings(got, want) {
		t.Fatalf("/status sections = %#v, want %#v", got, want)
	}
	if strings.Contains(response.Text, "Mode:") || strings.Contains(response.Text, "Thread ID:") || strings.Contains(response.Text, "status-thread") {
		t.Fatalf("/status text = %q, should not include current chat context", response.Text)
	}
	assertSectionRows(t, response.Sections[0], map[string]string{
		"核心状态": "未就绪",
		"运行时长": "1h 02m",
	})
	assertSectionRows(t, response.Sections[1], map[string]string{
		"缓存会话": "2",
		"项目":   "1",
		"临时":   "1",
		"跟踪会话": "2",
	})
	if response.Sections[1].Chart == nil || response.Sections[1].Chart.Spec["type"] != "pie" {
		t.Fatalf("thread mix chart = %#v, want pie chart", response.Sections[1].Chart)
	}
	if got, want := response.Sections[1].Chart.Spec["categoryField"], "label"; got != want {
		t.Fatalf("thread mix categoryField = %#v, want %q", got, want)
	}
	chartData, ok := response.Sections[1].Chart.Spec["data"].(map[string]any)
	if !ok {
		t.Fatalf("thread mix chart data = %#v, want map", response.Sections[1].Chart.Spec["data"])
	}
	chartValues, ok := chartData["values"].([]map[string]any)
	if !ok || len(chartValues) != 2 {
		t.Fatalf("thread mix chart values = %#v, want two values", chartData["values"])
	}
	if chartValues[0]["label"] != "项目 50%" || chartValues[1]["label"] != "临时 50%" {
		t.Fatalf("thread mix chart labels = %#v, want percentage labels", chartValues)
	}
	assertSectionRows(t, response.Sections[2], map[string]string{
		"话题模式": "单聊话题",
	})
	if got := len(response.Sections[2].Rows); got != 1 {
		t.Fatalf("Feishu rows = %d, want topic mode only", got)
	}
	if len(response.Sections[2].Buttons) != 0 {
		t.Fatalf("Feishu buttons = %#v, want no language switch in /status", response.Sections[2].Buttons)
	}
}

func TestPlainTextWithoutRouteReturnsWorkspaceHint(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "随便问一句", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "No Codex thread is selected yet") {
		t.Fatalf("response = %#v, want Chinese workspace routing hint", response)
	}
	var labels []string
	for _, row := range response.Buttons {
		for _, button := range row {
			labels = append(labels, button.Text)
		}
	}
	for _, want := range []string{"Workspace", "Recent chats", "Projects"} {
		if !containsString(labels, want) {
			t.Fatalf("hint buttons = %#v, missing %q", labels, want)
		}
	}
	token := callbackTokenForButton(response.Buttons, "Workspace")
	if token == "" {
		t.Fatalf("hint buttons = %#v, want workspace token", response.Buttons)
	}
	callbackResponse, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 903, 123456789, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(workspace) failed: %v", err)
	}
	if callbackResponse == nil || !strings.Contains(callbackResponse.Text, "Codex Workspace") {
		t.Fatalf("callback response = %#v, want workspace overview", callbackResponse)
	}
}

func TestFeishuTopicPlainTextWithoutRouteStaysInTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	response, err := service.HandleMessageFromSource(ctx, 123456789, 555, 123456789, "随便问一句", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleMessageFromSource failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "No Codex thread is selected yet") {
		t.Fatalf("response = %#v, want workspace routing hint", response)
	}
	if response.DeliveryTopicID != 555 || response.Options.FeishuReplyToMessageID != 555 || !response.Options.FeishuReplyInThread {
		t.Fatalf("response topic = delivery:%d options:%#v, want reply in topic 555", response.DeliveryTopicID, response.Options)
	}
}

func TestFeishuTopicSlashTextWithArgumentsUsesBoundThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 555,
		ThreadID:  "topic-thread",
		TurnID:    "topic-turn",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:           "topic-thread",
		Title:        "Topic thread",
		ActiveTurnID: "topic-turn",
		Raw:          json.RawMessage(`{"id":"topic-thread","activeTurn":{"id":"topic-turn"}}`),
	}); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	service.cfg.OpenCodexDesktopOnFeishu = true
	dispatcher := &stubDesktopInputDispatcher{}
	service.desktopInputDispatcher = dispatcher
	response, err := service.HandleMessageFromSource(ctx, 123456789, 555, 123456789, "/chats 出来的是会话列表，要变成绿色背景", 555, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleMessageFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != "topic-thread" {
		t.Fatalf("response = %#v, want bound thread input", response)
	}
	if len(dispatcher.steerInputs) != 1 || len(dispatcher.steerInputs[0]) != 1 || dispatcher.steerInputs[0][0]["text"] != "/chats 出来的是会话列表，要变成绿色背景" {
		t.Fatalf("steer inputs = %#v, want slash text sent to thread", dispatcher.steerInputs)
	}
	if strings.Contains(response.Text, "No cached threads") || strings.Contains(response.Text, "还没有缓存会话") {
		t.Fatalf("response text = %q, want slash text routed to thread", response.Text)
	}
}

func TestFeishuTopicStaleCallbackStaysInTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	response, err := service.HandleCallbackFromSource(ctx, 123456789, 555, 903, 123456789, "missing-token", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallbackFromSource failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "button is stale") {
		t.Fatalf("response = %#v, want stale button hint", response)
	}
	if response.DeliveryTopicID != 555 || response.Options.FeishuReplyToMessageID != 555 || !response.Options.FeishuReplyInThread {
		t.Fatalf("response topic = delivery:%d options:%#v, want reply in topic 555", response.DeliveryTopicID, response.Options)
	}
}

func TestSettingsMenuEditUsesSilentCallback(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/setting", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/setting) failed: %v", err)
	}
	if response.SettingsForm == nil || response.SettingsForm.SubmitToken == "" {
		t.Fatalf("settings response = %#v, want form with submit token", response)
	}
	callbackResponse, err := service.HandleCallbackPayloadFromSource(ctx, 123456789, 0, 902, 123456789, response.SettingsForm.SubmitToken, map[string]any{
		"form_value": map[string]any{
			"model":     "",
			"reasoning": "low",
			"language":  botLanguageEnglish,
		},
	}, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(settings submit) failed: %v", err)
	}
	if callbackResponse == nil || !callbackResponse.SilentCallback || callbackResponse.CallbackText != "Settings applied." {
		t.Fatalf("callback response = %#v, want silent settings callback", callbackResponse)
	}
	if callbackResponse.Text != "" {
		t.Fatalf("callback text = %q, want no direct delivery after edit", callbackResponse.Text)
	}
	if len(sender.edits) != 1 || !strings.Contains(sender.edits[0].text, "Settings applied.") || !strings.Contains(sender.edits[0].text, "Reasoning effort: low") {
		t.Fatalf("edits = %#v, want applied settings edit", sender.edits)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("messages = %#v, want no fallback message", sender.messages)
	}
}

func TestPlainTextWithoutRouteUsesEnglishWorkspaceHint(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, botLanguageStateKey, botLanguageEnglish); err != nil {
		t.Fatalf("SetState(language) failed: %v", err)
	}
	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "hello", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "No Codex thread is selected yet") {
		t.Fatalf("response = %#v, want English workspace routing hint", response)
	}
	if callbackTokenForButton(response.Buttons, "Workspace") == "" {
		t.Fatalf("hint buttons = %#v, want Workspace", response.Buttons)
	}
}

func TestFeishuShowThreadCallbackOpensTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "thread-feishu-open",
		Title:       "Feishu Open",
		ProjectName: "Codex",
		UpdatedAt:   time.Now().UTC().Unix(),
		Raw:         json.RawMessage(`{"id":"thread-feishu-open"}`),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-open",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-open",
		LatestFinalText:  "Done.",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	token := service.callbackButton(ctx, "Open 1", "show_thread", thread.ID, "", "", nil).CallbackData

	response, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallbackFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want thread response", response)
	}
	if strings.TrimSpace(response.Text) != "" || response.CallbackText == "" {
		t.Fatalf("response = %#v, want fast callback-only response", response)
	}
	messages := waitForRecordedMessages(t, sender)
	ensureTopicThreads := sender.ensureTopicThreadSnapshot()
	if len(ensureTopicThreads) == 0 || ensureTopicThreads[len(ensureTopicThreads)-1].ID != thread.ID {
		t.Fatalf("EnsureThreadTopic calls = %#v, want selected thread topic", ensureTopicThreads)
	}
	for _, message := range messages {
		if strings.Contains(message.text, "Codex thread topic opened") || strings.Contains(message.text, "已打开 Codex 会话话题") {
			t.Fatalf("recorded messages = %#v, want no duplicate open-topic activation text", messages)
		}
		if message.options.FeishuReplyToMessageID != 9001 || !message.options.FeishuReplyInThread {
			t.Fatalf("message options = %#v, want topic panel message under root", message.options)
		}
	}
}

func TestFeishuShowThreadCallbackForCodexChatOpensTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{
		ID:            "chat-show-thread",
		Title:         "Old Chat Button",
		CWD:           "/Users/example/Documents/Codex/2026-04-20/chat-show-thread",
		ProjectName:   "chat-show-thread",
		DirectoryName: "chat-show-thread",
		UpdatedAt:     time.Now().UTC().Unix(),
		Raw:           json.RawMessage(`{"id":"chat-show-thread"}`),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-chat-open",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-chat-open",
		LatestFinalText:  "Done.",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	token := service.callbackButton(ctx, "Open old chat", "show_thread", thread.ID, "", "", nil).CallbackData

	response, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallbackFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want chat thread response", response)
	}
	if strings.TrimSpace(response.Text) != "" || response.CallbackText == "" {
		t.Fatalf("response = %#v, want fast callback-only response", response)
	}
	messages := waitForRecordedMessages(t, sender)
	ensureTopicThreads := sender.ensureTopicThreadSnapshot()
	if len(ensureTopicThreads) == 0 || ensureTopicThreads[len(ensureTopicThreads)-1].ID != thread.ID {
		t.Fatalf("EnsureThreadTopic calls = %#v, want selected chat thread", ensureTopicThreads)
	}
	for _, message := range messages {
		if strings.Contains(message.text, "Codex thread topic opened") || strings.Contains(message.text, "已打开 Codex 会话话题") {
			t.Fatalf("recorded messages = %#v, want no duplicate open-topic activation text", messages)
		}
	}
}

func TestProjectsCommandShowsProjectButtonsGroupedByCWD(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	threads := []model.Thread{
		{
			ID:            "workspace-a-1",
			Title:         "A one",
			CWD:           "/Users/example/work/a/codex-feishu",
			ProjectName:   "codex-feishu",
			DirectoryName: "codex-feishu",
			UpdatedAt:     30,
			Raw:           json.RawMessage(`{"id":"workspace-a-1"}`),
		},
		{
			ID:            "workspace-a-2",
			Title:         "A two",
			CWD:           "/Users/example/work/a/codex-feishu",
			ProjectName:   "codex-feishu",
			DirectoryName: "codex-feishu",
			UpdatedAt:     40,
			Raw:           json.RawMessage(`{"id":"workspace-a-2"}`),
		},
		{
			ID:            "workspace-b-1",
			Title:         "B one",
			CWD:           "/Users/example/work/b/codex-feishu",
			ProjectName:   "codex-feishu",
			DirectoryName: "codex-feishu",
			UpdatedAt:     20,
			Raw:           json.RawMessage(`{"id":"workspace-b-1"}`),
		},
	}
	for _, thread := range threads {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	if response == nil {
		t.Fatal("handleCommand(/projects) returned nil response")
	}
	if !strings.Contains(response.Text, "codex-feishu") || len(response.Sections) == 0 || len(response.Sections[0].Rows) != 2 {
		t.Fatalf("/projects response missing grouped project rows: text=\n%s\nsections=%#v", response.Text, response.Sections)
	}
	if strings.Contains(response.Text, "key:") {
		t.Fatalf("/projects text renders internal project key:\n%s", response.Text)
	}
	if !strings.Contains(response.Text, "A two") || !strings.Contains(response.Text, "B one") {
		t.Fatalf("/projects text missing latest thread labels:\n%s", response.Text)
	}
	if got := countButtonsContaining(response.Buttons, "codex-feishu"); got != 2 {
		t.Fatalf("/projects buttons = %#v, want two named project workspace buttons", response.Buttons)
	}
}

func TestIsCodexChatsCWDMatchesGenericMacAndWindowsPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cwd  string
		want bool
	}{
		{cwd: "/Users/alice/Documents/Codex", want: true},
		{cwd: "/Users/alice/Documents/Codex/2026-04-29/new-chat", want: true},
		{cwd: `C:\Users\you\Documents\Codex`, want: true},
		{cwd: `C:\Users\you\Documents\Codex\2026-04-28\tool-call`, want: true},
		{cwd: "/Users/alice/Library/CloudStorage/OneDrive-Personal/Programming/AI/Codex", want: false},
		{cwd: "/Users/alice/Documents/Codexology", want: false},
		{cwd: `D:\Users\bob\Documents\Codex`, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.cwd, func(t *testing.T) {
			if got := isCodexChatsCWD(tc.cwd); got != tc.want {
				t.Fatalf("isCodexChatsCWD(%q) = %v, want %v", tc.cwd, got, tc.want)
			}
		})
	}
}

func TestProjectsCommandShowsChatsSectionAndSortsByRecency(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	threads := []model.Thread{
		{
			ID:            "old-project-thread",
			Title:         "Old project",
			CWD:           "/Users/example/work/old-project",
			ProjectName:   "old-project",
			DirectoryName: "old-project",
			UpdatedAt:     10,
			Raw:           json.RawMessage(`{"id":"old-project-thread"}`),
		},
		{
			ID:            "new-project-thread",
			Title:         "New project",
			CWD:           "/Users/example/work/new-project",
			ProjectName:   "new-project",
			DirectoryName: "new-project",
			UpdatedAt:     50,
			Raw:           json.RawMessage(`{"id":"new-project-thread"}`),
		},
		{
			ID:            "older-chat-thread",
			Title:         "Older Chat",
			CWD:           "/Users/example/Documents/Codex/2026-04-28/tool-call",
			ProjectName:   "tool-call",
			DirectoryName: "tool-call",
			UpdatedAt:     20,
			Raw:           json.RawMessage(`{"id":"older-chat-thread"}`),
		},
		{
			ID:            "newer-chat-thread",
			Title:         "Newer Chat",
			CWD:           "/Users/example/Documents/Codex/2026-04-29/new-chat",
			ProjectName:   "new-chat",
			DirectoryName: "new-chat",
			UpdatedAt:     60,
			Raw:           json.RawMessage(`{"id":"newer-chat-thread"}`),
		},
		{
			ID:            "windows-chat-thread",
			Title:         "Windows Chat",
			CWD:           `C:\Users\you\Documents\Codex\2026-04-30\win-chat`,
			ProjectName:   "win-chat",
			DirectoryName: "win-chat",
			UpdatedAt:     30,
			Raw:           json.RawMessage(`{"id":"windows-chat-thread"}`),
		},
	}
	for _, thread := range threads {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	if response == nil {
		t.Fatal("handleCommand(/projects) returned nil response")
	}
	for _, needle := range []string{"Projects", "Projects 2/2", "Chats", "Newer Chat"} {
		if !strings.Contains(response.Text, needle) {
			t.Fatalf("/projects text missing %q:\n%s", needle, response.Text)
		}
	}
	if len(response.Sections) != 2 || len(response.Sections[0].Rows) != 2 || len(response.Sections[1].Rows) != 1 || !response.Sections[1].Divider {
		t.Fatalf("/projects sections = %#v, want projects section and divider Chats section", response.Sections)
	}
	if strings.Contains(response.Text, "Windows Chat") || strings.Contains(response.Text, "Older Chat") {
		t.Fatalf("/projects text should not list chat threads at top level:\n%s", response.Text)
	}
	requireTextOrder(t, response.Text, "new-project", "old-project")
	requireTextOrder(t, response.Text, "old-project", "Chats")
	if strings.Contains(response.Text, "cwd: /Users/example/Documents/Codex") || strings.Contains(response.Text, `cwd: C:\Users\you\Documents\Codex`) {
		t.Fatalf("/projects text renders chat cwd as project cwd:\n%s", response.Text)
	}
	if strings.Contains(response.Text, "key:") {
		t.Fatalf("/projects text renders internal project key:\n%s", response.Text)
	}
	if !strings.Contains(response.Text, "New project") || !strings.Contains(response.Text, "Old project") {
		t.Fatalf("/projects text missing latest project thread labels:\n%s", response.Text)
	}
	for _, label := range []string{"1. new-project", "2. old-project", "3. Chats"} {
		if callbackTokenForButton(response.Buttons, label) == "" {
			t.Fatalf("/projects buttons = %#v, want named button %q", response.Buttons, label)
		}
	}
	chatsToken := callbackTokenForButton(response.Buttons, "3. Chats")
	chats, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, chatsToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(Chats project) failed: %v", err)
	}
	for _, needle := range []string{"Threads for Chats", "Newer Chat", "Windows Chat", "Older Chat", "New chat"} {
		if !strings.Contains(chats.Text, needle) {
			t.Fatalf("Chats project text missing %q:\n%s", needle, chats.Text)
		}
	}
	if callbackTokenForButton(chats.Buttons, "New chat") == "" {
		t.Fatalf("Chats project buttons = %#v, want New chat", chats.Buttons)
	}

	if err := service.store.SetState(ctx, botLanguageStateKey, botLanguageChinese); err != nil {
		t.Fatalf("SetState(language) failed: %v", err)
	}
	zh, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects zh) failed: %v", err)
	}
	if !strings.Contains(zh.Text, "对话") || strings.Contains(zh.Text, "临时对话") || strings.Contains(zh.Text, "\n3. Chats") {
		t.Fatalf("zh /projects text =\n%s\nwant localized 对话 project label", zh.Text)
	}
	requireTextOrder(t, zh.Text, "old-project", "对话")
}

func TestProjectsCommandPromotesPinnedProjectsAndThreads(t *testing.T) {
	t.Parallel()

	state := map[string]any{
		"pinned-project-ids": []string{"/Users/example/work/old-project"},
		"pinned-thread-ids":  []string{"older-chat-thread"},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state failed: %v", err)
	}
	statePath := filepath.Join(t.TempDir(), ".codex-global-state.json")
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write codex global state failed: %v", err)
	}

	service := newTestService(t)
	service.codexGlobalStatePath = statePath
	ctx := context.Background()
	for _, thread := range []model.Thread{
		{
			ID:            "old-project-thread",
			Title:         "Project pinned by workspace",
			CWD:           "/Users/example/work/old-project",
			ProjectName:   "old-project",
			DirectoryName: "old-project",
			UpdatedAt:     10,
			Raw:           json.RawMessage(`{"id":"old-project-thread"}`),
		},
		{
			ID:            "new-project-thread",
			Title:         "Newer project thread",
			CWD:           "/Users/example/work/new-project",
			ProjectName:   "new-project",
			DirectoryName: "new-project",
			UpdatedAt:     50,
			Raw:           json.RawMessage(`{"id":"new-project-thread"}`),
		},
		{
			ID:            "old-project-newer-thread",
			Title:         "Newer unpinned same project",
			CWD:           "/Users/example/work/old-project",
			ProjectName:   "old-project",
			DirectoryName: "old-project",
			UpdatedAt:     60,
			Raw:           json.RawMessage(`{"id":"old-project-newer-thread"}`),
		},
		{
			ID:            "older-chat-thread",
			Title:         "Pinned Chat",
			CWD:           "/Users/example/Documents/Codex/2026-04-28/pinned-chat",
			ProjectName:   "pinned-chat",
			DirectoryName: "pinned-chat",
			UpdatedAt:     20,
			Raw:           json.RawMessage(`{"id":"older-chat-thread"}`),
		},
		{
			ID:            "newer-chat-thread",
			Title:         "Newer Chat",
			CWD:           "/Users/example/Documents/Codex/2026-04-29/new-chat",
			ProjectName:   "new-chat",
			DirectoryName: "new-chat",
			UpdatedAt:     80,
			Raw:           json.RawMessage(`{"id":"newer-chat-thread"}`),
		},
	} {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	requireTextOrder(t, response.Text, "[Pinned] old-project", "new-project")
	requireTextOrder(t, response.Text, "new-project", "[Pinned] Chats")
	if len(response.Sections) < 2 || len(response.Sections[0].Rows) < 2 || response.Sections[0].Rows[0].BackgroundStyle != "cus-0" || response.Sections[0].Rows[1].BackgroundStyle != "cus-2" {
		t.Fatalf("/projects sections = %#v, want pinned project with cus-0 and regular project with cus-2", response.Sections)
	}
	if len(response.Sections[1].Rows) != 1 || response.Sections[1].Rows[0].BackgroundStyle != "cus-5" {
		t.Fatalf("/projects sections = %#v, want pinned Chats with cus-5", response.Sections)
	}
	if callbackTokenForButton(response.Buttons, "1. [Pinned] old-project") == "" {
		t.Fatalf("/projects buttons = %#v, want pinned project first", response.Buttons)
	}

	projectToken := callbackTokenForButton(response.Buttons, "1. [Pinned] old-project")
	projectThreads, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, projectToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(old-project) failed: %v", err)
	}
	requireTextOrder(t, projectThreads.Text, "Newer unpinned same project", "Project pinned by workspace")
	if len(projectThreads.Sections) != 1 || len(projectThreads.Sections[0].Rows) < 2 || projectThreads.Sections[0].Rows[0].BackgroundStyle != "cus-6" || projectThreads.Sections[0].Rows[1].BackgroundStyle != "cus-6" {
		t.Fatalf("project threads sections = %#v, want regular thread rows with cus-6", projectThreads.Sections)
	}

	chatsToken := callbackTokenForButton(response.Buttons, "3. [Pinned] Chats")
	chats, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 43, 123456789, chatsToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(Chats) failed: %v", err)
	}
	requireTextOrder(t, chats.Text, "[Pinned] Pinned Chat", "Newer Chat")
	if len(chats.Sections) != 1 || len(chats.Sections[0].Rows) < 2 || chats.Sections[0].Rows[0].BackgroundStyle != "cus-5" || chats.Sections[0].Rows[1].BackgroundStyle != "cus-6" {
		t.Fatalf("Chats sections = %#v, want pinned thread with cus-5 and regular thread with cus-6", chats.Sections)
	}
}

func TestProjectsCommandShowsRelativeProjectUpdatedAt(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.now = func() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	updatedAt := service.now().Add(-10 * 24 * time.Hour).Unix()
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "old-project-thread",
		Title:         "Old project thread",
		CWD:           "/Users/example/work/old-project",
		ProjectName:   "old-project",
		DirectoryName: "old-project",
		UpdatedAt:     updatedAt,
		Raw:           json.RawMessage(`{"id":"old-project-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	if !strings.Contains(response.Text, "1 chats · 10 天前") {
		t.Fatalf("/projects text =\n%s\nwant relative project updated time", response.Text)
	}
	if strings.Contains(response.Text, "2026-06-22") {
		t.Fatalf("/projects text =\n%s\nwant no absolute project date", response.Text)
	}
	if len(response.Sections) != 1 || len(response.Sections[0].Rows) != 1 || !strings.Contains(response.Sections[0].Rows[0].Trailing, "10 天前") {
		t.Fatalf("/projects sections = %#v, want relative project updated time", response.Sections)
	}
}

func TestProjectsCommandShowsAllProjectsAndKeepsChatsLast(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.ProjectsProjectPreviewLimit = 2
	service.cfg.ProjectsChatPreviewLimit = 1
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		thread := model.Thread{
			ID:            fmt.Sprintf("project-%d-thread", i),
			Title:         fmt.Sprintf("Project %d thread", i),
			CWD:           fmt.Sprintf("/Users/example/work/project-%d", i),
			ProjectName:   fmt.Sprintf("project-%d", i),
			DirectoryName: fmt.Sprintf("project-%d", i),
			UpdatedAt:     int64(i * 10),
			Raw:           json.RawMessage(fmt.Sprintf(`{"id":"project-%d-thread"}`, i)),
		}
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}
	for i := 1; i <= 2; i++ {
		thread := model.Thread{
			ID:            fmt.Sprintf("chat-%d-thread", i),
			Title:         fmt.Sprintf("Chat %d", i),
			CWD:           fmt.Sprintf("/Users/example/Documents/Codex/2026-04-2%d/chat-%d", i, i),
			ProjectName:   fmt.Sprintf("chat-%d", i),
			DirectoryName: fmt.Sprintf("chat-%d", i),
			UpdatedAt:     int64(100 + i),
			Raw:           json.RawMessage(fmt.Sprintf(`{"id":"chat-%d-thread"}`, i)),
		}
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	page1, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	for _, needle := range []string{"Projects", "project-5", "project-4", "project-3", "project-2", "project-1", "Chats"} {
		if !strings.Contains(page1.Text, needle) {
			t.Fatalf("page1 text =\n%s\nwant %q", page1.Text, needle)
		}
	}
	if strings.Contains(page1.Text, "Chat 1") {
		t.Fatalf("page1 text =\n%s\nwant no chat thread rows at top level", page1.Text)
	}
	for _, label := range []string{"1. project-5", "2. project-4", "3. project-3", "4. project-2", "5. project-1", "6. Chats"} {
		if callbackTokenForButton(page1.Buttons, label) == "" {
			t.Fatalf("page1 buttons = %#v, want named button %q", page1.Buttons, label)
		}
	}
	if callbackTokenForButton(page1.Buttons, ">") != "" || callbackTokenForButton(page1.Buttons, "<") != "" {
		t.Fatalf("page1 buttons = %#v, want no pagination controls", page1.Buttons)
	}
	requireTextOrder(t, page1.Text, "project-1", "Chats")
}

func TestProjectsCommandFiltersArchivedAndDeletedProjects(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	threads := []model.Thread{
		{
			ID:            "visible-thread",
			Title:         "Visible",
			CWD:           "/Users/example/work/visible",
			ProjectName:   "visible-project",
			DirectoryName: "visible",
			UpdatedAt:     50,
			Raw:           json.RawMessage(`{"id":"visible-thread"}`),
		},
		{
			ID:            "archived-thread",
			Title:         "Archived",
			CWD:           "/Users/example/work/archived",
			ProjectName:   "archived-project",
			DirectoryName: "archived",
			UpdatedAt:     60,
			Archived:      true,
			Raw:           json.RawMessage(`{"id":"archived-thread"}`),
		},
		{
			ID:            "raw-archived-thread",
			Title:         "Raw Archived",
			CWD:           "/Users/example/work/raw-archived",
			ProjectName:   "raw-archived-project",
			DirectoryName: "raw-archived",
			UpdatedAt:     70,
			Raw:           json.RawMessage(`{"thread":{"id":"raw-archived-thread","archived":true}}`),
		},
		{
			ID:            "deleted-thread",
			Title:         "Deleted",
			CWD:           "/Users/example/work/deleted",
			ProjectName:   "deleted-project",
			DirectoryName: "deleted",
			UpdatedAt:     80,
			Raw:           json.RawMessage(`{"thread":{"id":"deleted-thread","isDeleted":true}}`),
		},
	}
	for _, thread := range threads {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	if !strings.Contains(response.Text, "visible-project") {
		t.Fatalf("/projects text =\n%s\nwant visible project", response.Text)
	}
	for _, hidden := range []string{"archived-project", "raw-archived-project", "deleted-project"} {
		if strings.Contains(response.Text, hidden) || callbackTokenForButton(response.Buttons, "1. "+hidden) != "" {
			t.Fatalf("/projects includes hidden project %q:\n%s\nbuttons=%#v", hidden, response.Text, response.Buttons)
		}
	}
}

func TestProjectsCommandFollowsCodexSavedWorkspaceRoots(t *testing.T) {
	t.Parallel()

	state := map[string]any{
		"project-order": []string{"/Users/example/work/visible"},
		"electron-saved-workspace-roots": []string{
			"/Users/example/work/visible",
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state failed: %v", err)
	}
	statePath := filepath.Join(t.TempDir(), ".codex-global-state.json")
	if err := os.WriteFile(statePath, data, 0o644); err != nil {
		t.Fatalf("write codex global state failed: %v", err)
	}

	service := newTestService(t)
	service.codexGlobalStatePath = statePath
	ctx := context.Background()
	for _, thread := range []model.Thread{
		{
			ID:            "visible-thread",
			Title:         "Visible",
			CWD:           "/Users/example/work/visible",
			ProjectName:   "visible-project",
			DirectoryName: "visible",
			UpdatedAt:     50,
			Raw:           json.RawMessage(`{"id":"visible-thread"}`),
		},
		{
			ID:            "removed-thread",
			Title:         "Removed",
			CWD:           "/Users/example/work/removed",
			ProjectName:   "removed-project",
			DirectoryName: "removed",
			UpdatedAt:     60,
			Raw:           json.RawMessage(`{"id":"removed-thread"}`),
		},
	} {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	if !strings.Contains(response.Text, "visible-project") {
		t.Fatalf("/projects text =\n%s\nwant visible project", response.Text)
	}
	if strings.Contains(response.Text, "removed-project") {
		t.Fatalf("/projects text =\n%s\nwant removed project hidden", response.Text)
	}
}

func TestSyncThreadsHidesProjectsMissingFromCompleteCodexList(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	for _, thread := range []model.Thread{
		{
			ID:            "visible-thread",
			Title:         "Visible",
			CWD:           "/Users/example/work/visible",
			ProjectName:   "visible-project",
			DirectoryName: "visible",
			UpdatedAt:     50,
			Raw:           json.RawMessage(`{"id":"visible-thread"}`),
		},
		{
			ID:            "removed-thread",
			Title:         "Removed",
			CWD:           "/Users/example/work/removed",
			ProjectName:   "removed-project",
			DirectoryName: "removed",
			UpdatedAt:     60,
			Raw:           json.RawMessage(`{"id":"removed-thread"}`),
		},
	} {
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}
	stub := &stubSession{
		threadListResults: []map[string]any{
			{"threads": []any{map[string]any{
				"id":        "visible-thread",
				"title":     "Visible",
				"cwd":       "/Users/example/work/visible",
				"updatedAt": float64(70),
			}}},
		},
	}
	service.live = stub
	service.liveConnected = true

	service.syncThreads(ctx, 0)

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	if !strings.Contains(response.Text, "visible") {
		t.Fatalf("/projects text =\n%s\nwant visible project", response.Text)
	}
	if strings.Contains(response.Text, "removed-project") {
		t.Fatalf("/projects text =\n%s\nwant removed-project hidden", response.Text)
	}
	removed, err := service.store.GetThread(ctx, "removed-thread")
	if err != nil {
		t.Fatalf("GetThread(removed-thread) failed: %v", err)
	}
	if removed == nil || removed.Listed {
		t.Fatalf("removed thread = %#v, want listed=false", removed)
	}
}

func TestSyncThreadsDoesNotHideProjectsWhenThreadListFails(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "cached-thread",
		Title:         "Cached",
		CWD:           "/Users/example/work/cached",
		ProjectName:   "cached-project",
		DirectoryName: "cached",
		UpdatedAt:     50,
		Raw:           json.RawMessage(`{"id":"cached-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread(cached-thread) failed: %v", err)
	}
	stub := &stubSession{threadListErr: errors.New("thread list failed")}
	service.live = stub
	service.liveConnected = true

	service.syncThreads(ctx, 0)

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	if !strings.Contains(response.Text, "cached-project") {
		t.Fatalf("/projects text =\n%s\nwant cached-project kept after list failure", response.Text)
	}
}

func TestOpenChatsPaginatesAndChatSelectionBindsThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.ChatsPageSize = 3
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		thread := model.Thread{
			ID:            fmt.Sprintf("chat-%d-thread", i),
			Title:         fmt.Sprintf("Chat %d", i),
			CWD:           fmt.Sprintf("/Users/example/Documents/Codex/2026-04-2%d/chat-%d", i, i),
			ProjectName:   fmt.Sprintf("chat-%d", i),
			DirectoryName: fmt.Sprintf("chat-%d", i),
			UpdatedAt:     int64(i * 10),
			Raw:           json.RawMessage(fmt.Sprintf(`{"id":"chat-%d-thread"}`, i)),
		}
		if err := service.store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
		if i == 5 {
			snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
				Thread:             thread,
				LatestTurnID:       "turn-chat-5",
				LatestTurnStatus:   "completed",
				LatestFinalText:    "Done.",
				LatestFinalFP:      "final-chat-5",
				LatestProgressText: "Done.",
				LatestProgressFP:   "progress-chat-5",
			}, time.Now().UTC())
			if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
				t.Fatalf("UpsertSnapshot(%s) failed: %v", thread.ID, err)
			}
			if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
				ChatID:           123456789,
				TopicID:          0,
				ThreadID:         thread.ID,
				SourceMode:       model.PanelSourceFeishuInput,
				SummaryMessageID: 7777,
				CurrentTurnID:    "turn-chat-5",
				Status:           "completed",
				IsCurrent:        true,
			}); err != nil {
				t.Fatalf("UpsertThreadPanel(%s) failed: %v", thread.ID, err)
			}
		}
	}

	projects, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	openChats := callbackTokenForButton(projects.Buttons, "1. Chats")
	if openChats == "" {
		t.Fatalf("/projects buttons = %#v, want Chats project", projects.Buttons)
	}
	chats, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, openChats, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(Open Chats) failed: %v", err)
	}
	if !strings.Contains(chats.Text, "Threads for Chats") || !strings.Contains(chats.Text, "Chat 5") || !strings.Contains(chats.Text, "Chat 1") || !strings.Contains(chats.Text, "New chat") {
		t.Fatalf("chats text =\n%s\nwant Chats project threads with New chat", chats.Text)
	}
	if len(chats.Sections) != 1 || len(chats.Sections[0].Rows) == 0 || chats.Sections[0].Rows[len(chats.Sections[0].Rows)-1].BackgroundStyle != "cus-4" || chats.Sections[0].Rows[len(chats.Sections[0].Rows)-1].BorderColor != "cus-7" {
		t.Fatalf("chats sections = %#v, want New chat row with chat border", chats.Sections)
	}
	if callbackTokenForButton(chats.Buttons, "New chat") == "" {
		t.Fatalf("chats response = %#v, want New chat action", chats)
	}
	chatToken := callbackTokenForButton(chats.Buttons, "Open")
	if chatToken == "" {
		t.Fatalf("chats buttons = %#v, want open chat button", chats.Buttons)
	}
	opened, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, chatToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(Chat 1. Chat 5) failed: %v", err)
	}
	if opened == nil || opened.ThreadID != "chat-5-thread" {
		t.Fatalf("opened = %#v, want newest chat thread", opened)
	}
	if strings.TrimSpace(opened.Text) != "" || opened.CallbackText == "" {
		t.Fatalf("opened = %#v, want fast callback-only response", opened)
	}
	messages := waitForRecordedMessages(t, sender)
	ensureTopicThreads := sender.ensureTopicThreadSnapshot()
	if len(ensureTopicThreads) == 0 || ensureTopicThreads[len(ensureTopicThreads)-1].ID != "chat-5-thread" {
		t.Fatalf("EnsureThreadTopic threads = %#v, want selected chat activation", ensureTopicThreads)
	}
	for _, message := range messages {
		if strings.Contains(message.text, "Codex thread topic opened") || strings.Contains(message.text, "已打开 Codex 会话话题") {
			t.Fatalf("recorded messages = %#v, want no duplicate open-topic activation text", messages)
		}
		if message.options.FeishuReplyToMessageID != 9001 || !message.options.FeishuReplyInThread {
			t.Fatalf("message options = %#v, want topic panel message under root", message.options)
		}
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, 123456789, 0, "chat-5-thread")
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.SummaryMessageID == 7777 {
		t.Fatalf("panel = %#v, want new topic-backed current panel instead of old single-chat panel", panel)
	}
}

func TestProjectsCloseDeletesMenuMessage(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	response, err := service.closeProjectsMenu(ctx, 123456789, 0, 42)
	if err != nil {
		t.Fatalf("closeProjectsMenu failed: %v", err)
	}
	if response == nil || response.CallbackText != "Closed." || response.Text != "" {
		t.Fatalf("response = %#v, want callback-only closed response", response)
	}
	if len(sender.deletes) != 1 || sender.deletes[0].messageID != 42 {
		t.Fatalf("deletes = %#v, want deleted menu message 42", sender.deletes)
	}
}

func TestProjectOpenShowsInteractiveThreadList(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:            "project-menu-thread",
		Title:         "Menu thread",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     10,
		Raw:           json.RawMessage(`{"id":"project-menu-thread"}`),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "archived-project-thread",
		Title:         "Archived thread",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     9,
		Archived:      true,
		Raw:           json.RawMessage(`{"id":"archived-project-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread(archived) failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "deleted-project-thread",
		Title:         "Deleted thread",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     8,
		Raw:           json.RawMessage(`{"id":"deleted-project-thread","deleted":true}`),
	}); err != nil {
		t.Fatalf("UpsertThread(deleted) failed: %v", err)
	}

	projects, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	token := callbackTokenForButton(projects.Buttons, "1. project")
	if token == "" {
		t.Fatalf("/projects buttons = %#v, want project button", projects.Buttons)
	}

	menu, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(project_open) failed: %v", err)
	}
	if menu == nil || !strings.Contains(menu.Text, "Threads for project") {
		t.Fatalf("project threads = %#v, want project thread list", menu)
	}
	if strings.Contains(menu.Text, "Archived thread") {
		t.Fatalf("project threads text = %q, want archived thread hidden", menu.Text)
	}
	if strings.Contains(menu.Text, "Deleted thread") {
		t.Fatalf("project threads text = %q, want deleted thread hidden", menu.Text)
	}
	if len(menu.Sections) != 1 || menu.Sections[0].Text != "project" || len(menu.Sections[0].Rows) != 2 {
		t.Fatalf("project sections = %#v, want one project section with one visible row and new thread row", menu.Sections)
	}
	row := menu.Sections[0].Rows[0]
	if row.Title != "Menu thread" || row.Trailing == "" || row.Button.Text != "Open" || row.Button.CallbackData == "" {
		t.Fatalf("project row = %#v, want clickable visible thread row", row)
	}
	newThreadRow := menu.Sections[0].Rows[1]
	if newThreadRow.Title != "New thread" || newThreadRow.BackgroundStyle != "cus-4" || newThreadRow.BorderColor != "cus-7" || newThreadRow.Button.Text != "New thread" || newThreadRow.Button.CallbackData == "" {
		t.Fatalf("project new thread row = %#v, want clickable new thread row", newThreadRow)
	}
	if callbackTokenForButton(menu.Buttons, "New thread") == "" {
		t.Fatalf("project buttons = %#v, want New thread action", menu.Buttons)
	}
	if callbackTokenForButton(menu.Buttons, "Threads") != "" {
		t.Fatalf("project buttons = %#v, want no intermediate Threads action", menu.Buttons)
	}
}

func TestProjectNewThreadArmsThenFirstPromptCreatesThreadAndFeishuTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()
	project := model.Thread{
		ID:            "project-source-thread",
		Title:         "Source",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     10,
		Raw:           json.RawMessage(`{"id":"project-source-thread"}`),
	}
	if err := service.store.UpsertThread(ctx, project); err != nil {
		t.Fatalf("UpsertThread(project) failed: %v", err)
	}
	stub := &stubSession{
		threadStartResult: map[string]any{"thread": map[string]any{"id": "new-thread-id", "cwd": "/Users/example/project", "title": "新线程"}},
		threadReads: map[string]map[string]any{
			"new-thread-id": {
				"thread": map[string]any{
					"id":     "new-thread-id",
					"cwd":    "/Users/example/project",
					"title":  "Real title from first prompt",
					"status": "inProgress",
					"turns": []any{map[string]any{
						"id":     "started-turn",
						"status": "inProgress",
						"items":  []any{map[string]any{"id": "user-item", "type": "userMessage", "content": []any{map[string]any{"text": "first prompt"}}}},
					}},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true

	menu := openOnlyProjectMenu(t, service, ctx)
	armToken := callbackTokenForButton(menu.Buttons, "New thread")
	if armToken == "" {
		t.Fatalf("project menu buttons = %#v, want New thread", menu.Buttons)
	}
	armed, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, armToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(New thread) failed: %v", err)
	}
	if armed == nil || !strings.Contains(armed.Text, "Send the first prompt") {
		t.Fatalf("armed response = %#v, want prompt instruction", armed)
	}
	if len(stub.threadStartCalls) != 0 || len(stub.turnStartCalls) != 0 {
		t.Fatalf("calls after arm: threadStart=%#v turnStart=%#v, want none", stub.threadStartCalls, stub.turnStartCalls)
	}

	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "first prompt", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText(first prompt) failed: %v", err)
	}
	if response == nil || response.ThreadID != "new-thread-id" || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want new thread/turn", response)
	}
	if !strings.Contains(response.Text, "Codex thread topic opened") {
		t.Fatalf("response = %#v, want visible Feishu topic open message", response)
	}
	if len(stub.threadStartCalls) != 1 || stub.threadStartCalls[0] != "/Users/example/project" {
		t.Fatalf("threadStartCalls = %#v, want project cwd", stub.threadStartCalls)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want first prompt after arm", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.threadID != "new-thread-id" || got.message != "first prompt" || got.cwd != "/Users/example/project" {
		t.Fatalf("turnStartCall = %#v, want new thread first prompt in project cwd", got)
	}
	stored, err := service.store.GetThread(ctx, "new-thread-id")
	if err != nil {
		t.Fatalf("GetThread(new-thread-id) failed: %v", err)
	}
	if stored == nil || stored.Title != "Real title from first prompt" || stored.LastPreview != "first prompt" {
		t.Fatalf("stored thread = %#v, want refreshed Codex title and first prompt preview", stored)
	}
	if len(sender.messages) == 0 || sender.messages[0].style != model.MessageStyleCodexPanel {
		t.Fatalf("sender messages = %#v, want Codex panel created for refreshed topic", sender.messages)
	}
	if len(sender.ensureTopicThreads) == 0 || sender.ensureTopicThreads[0].Title != "Real title from first prompt" {
		t.Fatalf("EnsureThreadTopic threads = %#v, want refreshed Codex title", sender.ensureTopicThreads)
	}
}

func TestProjectNewThreadPromptFallsBackToChatLevelPendingState(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	state := pendingNewThreadState{
		ProjectName:   "project",
		DirectoryName: "project",
		CWD:           "/Users/example/project",
		ExpiresAt:     time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}
	payloadBytes, _ := json.Marshal(state)
	if err := service.store.SetState(ctx, newThreadStateKey(123456789, 9001), string(payloadBytes)); err != nil {
		t.Fatalf("SetState(exact) failed: %v", err)
	}
	if err := service.store.SetState(ctx, newThreadFallbackStateKey(123456789), string(payloadBytes)); err != nil {
		t.Fatalf("SetState(fallback) failed: %v", err)
	}
	stub := &stubSession{
		threadStartResult: map[string]any{"thread": map[string]any{"id": "fallback-thread-id", "cwd": "/Users/example/project", "title": "Fallback thread"}},
		threadReads: map[string]map[string]any{
			"fallback-thread-id": {
				"thread": map[string]any{
					"id":      "fallback-thread-id",
					"cwd":     "/Users/example/project",
					"title":   "Fallback title",
					"preview": "first prompt",
					"status":  "inProgress",
					"turns": []any{map[string]any{
						"id":     "started-turn",
						"status": "inProgress",
						"items":  []any{map[string]any{"id": "user-item", "type": "userMessage", "content": []any{map[string]any{"text": "first prompt"}}}},
					}},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true

	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "first prompt", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText(first prompt) failed: %v", err)
	}
	if response == nil || response.ThreadID != "fallback-thread-id" {
		t.Fatalf("response = %#v, want fallback-created thread", response)
	}
	if raw, _ := service.store.GetState(ctx, newThreadStateKey(123456789, 0)); strings.TrimSpace(raw) != "" {
		t.Fatalf("topic pending state still exists: %q", raw)
	}
	if raw, _ := service.store.GetState(ctx, newThreadStateKey(123456789, 9001)); strings.TrimSpace(raw) != "" {
		t.Fatalf("original topic pending state still exists: %q", raw)
	}
	if raw, _ := service.store.GetState(ctx, newThreadFallbackStateKey(123456789)); strings.TrimSpace(raw) != "" {
		t.Fatalf("fallback pending state still exists: %q", raw)
	}
}

func TestProjectNewThreadUsesDesktopCreateWhenEnabled(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "project-source-thread",
		Title:         "Source",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     10,
		Raw:           json.RawMessage(`{"id":"project-source-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread(project) failed: %v", err)
	}
	stub := &stubSession{
		threadListResult: map[string]any{"data": []any{map[string]any{
			"id":        "desktop-thread-id",
			"cwd":       "/Users/example/project",
			"name":      "Desktop title",
			"preview":   "first prompt",
			"updatedAt": time.Now().UTC().Format(time.RFC3339),
		}}},
		threadReads: map[string]map[string]any{
			"desktop-thread-id": {
				"thread": map[string]any{
					"id":      "desktop-thread-id",
					"cwd":     "/Users/example/project",
					"title":   "Real title from first prompt",
					"preview": "first prompt",
					"status":  "inProgress",
					"turns": []any{map[string]any{
						"id":     "started-turn",
						"status": "inProgress",
						"items":  []any{map[string]any{"id": "user-item", "type": "userMessage", "content": []any{map[string]any{"text": "first prompt"}}}},
					}},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true
	var createdCWD, createdPrompt string
	var createdProjectless bool
	service.desktopThreadCreator = func(ctx context.Context, cwd, prompt string, projectless bool) error {
		createdCWD = cwd
		createdPrompt = prompt
		createdProjectless = projectless
		return nil
	}

	menu := openOnlyProjectMenu(t, service, ctx)
	_, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, callbackTokenForButton(menu.Buttons, "New thread"), model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(New thread) failed: %v", err)
	}
	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "first prompt", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText(first prompt) failed: %v", err)
	}
	if response == nil || response.ThreadID != "desktop-thread-id" {
		t.Fatalf("response = %#v, want desktop-created thread", response)
	}
	if createdCWD != "/Users/example/project" || createdPrompt != "first prompt" || createdProjectless {
		t.Fatalf("desktop create cwd/prompt/projectless = %q/%q/%v, want project cwd, first prompt, and projectless=false", createdCWD, createdPrompt, createdProjectless)
	}
	if len(stub.threadStartCalls) != 0 {
		t.Fatalf("threadStartCalls = %#v, want desktop path without app-server thread/start", stub.threadStartCalls)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want desktop path without app-server turn/start", stub.turnStartCalls)
	}
	stored, err := service.store.GetThread(ctx, "desktop-thread-id")
	if err != nil {
		t.Fatalf("GetThread(desktop-thread-id) failed: %v", err)
	}
	if stored == nil || stored.Title != "Real title from first prompt" || stored.LastPreview != "first prompt" {
		t.Fatalf("stored thread = %#v, want refreshed app-server metadata", stored)
	}
	if len(sender.ensureTopicThreads) == 0 || sender.ensureTopicThreads[0].ID != "desktop-thread-id" {
		t.Fatalf("EnsureThreadTopic threads = %#v, want desktop-created thread", sender.ensureTopicThreads)
	}
	if len(sender.messages) == 0 || sender.messages[0].style != model.MessageStyleCodexPanel {
		t.Fatalf("sender messages = %#v, want Codex panel", sender.messages)
	}
}

func TestProjectNewThreadFallsBackToAppServerWhenDesktopCreateFails(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "project-source-thread",
		Title:         "Source",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     10,
		Raw:           json.RawMessage(`{"id":"project-source-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread(project) failed: %v", err)
	}
	stub := &stubSession{
		threadStartResult: map[string]any{"thread": map[string]any{"id": "new-thread-id", "cwd": "/Users/example/project"}},
		threadReads: map[string]map[string]any{
			"new-thread-id": {
				"thread": map[string]any{
					"id":      "new-thread-id",
					"cwd":     "/Users/example/project",
					"title":   "Real title from first prompt",
					"preview": "first prompt",
					"turns": []any{map[string]any{
						"id":     "started-turn",
						"status": "inProgress",
						"items":  []any{map[string]any{"id": "user-item", "type": "userMessage", "content": []any{map[string]any{"text": "first prompt"}}}},
					}},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true
	service.desktopThreadCreator = func(ctx context.Context, cwd, prompt string, projectless bool) error {
		return fmt.Errorf("desktop unavailable")
	}

	menu := openOnlyProjectMenu(t, service, ctx)
	_, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, callbackTokenForButton(menu.Buttons, "New thread"), model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(New thread) failed: %v", err)
	}
	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "first prompt", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText(first prompt) failed: %v", err)
	}
	if response == nil || response.ThreadID != "new-thread-id" {
		t.Fatalf("response = %#v, want fallback app-server thread", response)
	}
	if len(stub.threadStartCalls) != 1 || stub.threadStartCalls[0] != "/Users/example/project" {
		t.Fatalf("threadStartCalls = %#v, want app-server fallback", stub.threadStartCalls)
	}
	if len(stub.turnStartCalls) != 1 || stub.turnStartCalls[0].threadID != "new-thread-id" || stub.turnStartCalls[0].message != "first prompt" {
		t.Fatalf("turnStartCalls = %#v, want first prompt on fallback thread", stub.turnStartCalls)
	}
}

func TestProjectNewThreadWithoutCodexTitleKeepsFirstPromptPreview(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "project-source-thread",
		Title:         "Source",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     10,
		Raw:           json.RawMessage(`{"id":"project-source-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread(project) failed: %v", err)
	}
	stub := &stubSession{
		threadStartResult: map[string]any{"thread": map[string]any{"id": "new-thread-id", "cwd": "/Users/example/project"}},
		threadReads: map[string]map[string]any{
			"new-thread-id": {
				"thread": map[string]any{
					"id":      "new-thread-id",
					"cwd":     "/Users/example/project",
					"title":   "",
					"preview": "first prompt",
					"status":  "inProgress",
					"turns": []any{map[string]any{
						"id":     "started-turn",
						"status": "inProgress",
						"items":  []any{map[string]any{"id": "user-item", "type": "userMessage", "content": []any{map[string]any{"text": "first prompt"}}}},
					}},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true

	menu := openOnlyProjectMenu(t, service, ctx)
	_, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, callbackTokenForButton(menu.Buttons, "New thread"), model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(New thread) failed: %v", err)
	}
	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "first prompt", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText(first prompt) failed: %v", err)
	}
	if response == nil || response.ThreadID != "new-thread-id" {
		t.Fatalf("response = %#v, want new thread", response)
	}
	stored, err := service.store.GetThread(ctx, "new-thread-id")
	if err != nil {
		t.Fatalf("GetThread(new-thread-id) failed: %v", err)
	}
	if stored == nil || stored.Title != "" || stored.LastPreview != "first prompt" {
		t.Fatalf("stored thread = %#v, want empty title and first prompt preview", stored)
	}
}

func TestChatsProjectNewThreadCreatesDesktopChatWithoutProjectPath(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	service.cfg.CodexChatsRoot = t.TempDir()
	ctx := context.Background()
	service.now = func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "existing-chat-thread",
		Title:         "Existing Chat",
		CWD:           filepath.Join(service.cfg.CodexChatsRoot, "2026-06-30", "existing-chat"),
		ProjectName:   "existing-chat",
		DirectoryName: "existing-chat",
		UpdatedAt:     10,
		Raw:           json.RawMessage(`{"id":"existing-chat-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread(existing chat) failed: %v", err)
	}
	stub := &stubSession{
		threadListResult: map[string]any{"threads": []any{map[string]any{
			"id":         "new-chat-thread",
			"title":      "",
			"preview":    "临时对话第一条",
			"status":     "inProgress",
			"updated_at": time.Now().Unix(),
		}}},
		threadReads: map[string]map[string]any{
			"new-chat-thread": {
				"thread": map[string]any{
					"id":      "new-chat-thread",
					"title":   "",
					"preview": "临时对话第一条",
					"status":  "inProgress",
					"turns": []any{map[string]any{
						"id":     "started-turn",
						"status": "inProgress",
						"items":  []any{map[string]any{"id": "user-item", "type": "userMessage", "content": []any{map[string]any{"text": "临时对话第一条"}}}},
					}},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true
	var createdCWD, createdPrompt string
	var createdProjectless bool
	service.desktopThreadCreator = func(ctx context.Context, cwd, prompt string, projectless bool) error {
		createdCWD = cwd
		createdPrompt = prompt
		createdProjectless = projectless
		return nil
	}

	projects, err := service.handleCommandFromSource(ctx, 123456789, 0, "/projects", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/projects) failed: %v", err)
	}
	chatsToken := callbackTokenForButton(projects.Buttons, "1. Chats")
	if chatsToken == "" {
		t.Fatalf("projects buttons = %#v, want Chats project", projects.Buttons)
	}
	chats, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, chatsToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(Chats) failed: %v", err)
	}
	newToken := callbackTokenForButton(chats.Buttons, "New chat")
	if newToken == "" {
		t.Fatalf("Chats buttons = %#v, want New chat", chats.Buttons)
	}
	if _, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 43, 123456789, newToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(New thread) failed: %v", err)
	}
	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "临时对话第一条", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText(first prompt) failed: %v", err)
	}
	if response == nil || response.ThreadID != "new-chat-thread" {
		t.Fatalf("response = %#v, want new chat thread", response)
	}
	if createdCWD != "" || createdPrompt != "临时对话第一条" || !createdProjectless {
		t.Fatalf("desktop create cwd/prompt/projectless = %q/%q/%v, want projectless Chat prompt", createdCWD, createdPrompt, createdProjectless)
	}
	if len(stub.threadStartCalls) != 0 {
		t.Fatalf("threadStartCalls = %#v, want no app-server fallback for desktop Chat", stub.threadStartCalls)
	}
	stored, err := service.store.GetThread(ctx, "new-chat-thread")
	if err != nil {
		t.Fatalf("GetThread(new-chat-thread) failed: %v", err)
	}
	if stored == nil || stored.ProjectName != chatsProjectName || stored.CWD != "" {
		t.Fatalf("stored thread = %#v, want Chats project without cwd", stored)
	}
}

func TestChatsProjectNewThreadDoesNotFallbackToAppServerWhenDesktopCreateFails(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	stub := &stubSession{
		threadStartResult: map[string]any{"thread": map[string]any{"id": "appserver-chat-thread"}},
	}
	service.live = stub
	service.liveConnected = true
	service.desktopThreadCreator = func(ctx context.Context, cwd, prompt string, projectless bool) error {
		return fmt.Errorf("desktop unavailable")
	}
	response, err := service.createThreadFromProjectPrompt(ctx, 123456789, 0, pendingNewThreadState{
		Kind:        chatsProjectWorkspaceKey,
		ProjectName: chatsProjectName,
	}, "first prompt", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("createThreadFromProjectPrompt(first prompt) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "Could not create a Codex desktop Chat automatically") {
		t.Fatalf("response = %#v, want desktop Chat failure", response)
	}
	if len(stub.threadStartCalls) != 0 || len(stub.turnStartCalls) != 0 {
		t.Fatalf("app-server calls = %#v/%#v, want no fallback", stub.threadStartCalls, stub.turnStartCalls)
	}
}

func TestProjectNewThreadRejectsThreadStartWithoutID(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:            "project-source-thread",
		Title:         "Source",
		CWD:           "/Users/example/project",
		ProjectName:   "project",
		DirectoryName: "project",
		UpdatedAt:     10,
		Raw:           json.RawMessage(`{"id":"project-source-thread"}`),
	}); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{threadStartResult: map[string]any{"thread": map[string]any{"cwd": "/Users/example/project"}}}
	service.live = stub
	service.liveConnected = true

	menu := openOnlyProjectMenu(t, service, ctx)
	armed, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, callbackTokenForButton(menu.Buttons, "New thread"), model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(New thread) failed: %v", err)
	}
	if armed == nil || !strings.Contains(armed.Text, "Send the first prompt") {
		t.Fatalf("armed response = %#v, want prompt instruction", armed)
	}
	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "first prompt", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "could not create thread") {
		t.Fatalf("response = %#v, want missing thread id error", response)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no turn start without thread id", stub.turnStartCalls)
	}
}

func TestNewChatCommandCreatesDesktopChatWithoutProjectPathAndBinds(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	stub := &stubSession{
		threadListResult: map[string]any{"threads": []any{map[string]any{
			"id":         "new-chat-thread",
			"title":      "New chat",
			"preview":    "Проверь tool call по погоде",
			"updated_at": time.Now().Unix(),
		}}},
		threadReads: map[string]map[string]any{
			"new-chat-thread": {
				"thread": map[string]any{
					"id":     "new-chat-thread",
					"title":  "New chat",
					"status": "active",
					"turns": []any{map[string]any{
						"id":     "started-turn",
						"status": "inProgress",
						"items":  []any{map[string]any{"id": "user-item", "type": "userMessage", "content": []any{map[string]any{"text": "Проверь tool call по погоде"}}}},
					}},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true
	var createdCWD, createdPrompt string
	var createdProjectless bool
	service.desktopThreadCreator = func(ctx context.Context, cwd, prompt string, projectless bool) error {
		createdCWD = cwd
		createdPrompt = prompt
		createdProjectless = projectless
		return nil
	}

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/new Проверь tool call по погоде", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/new) failed: %v", err)
	}
	if response == nil || response.ThreadID != "new-chat-thread" || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want new chat thread/turn", response)
	}
	if createdCWD != "" || createdPrompt != "Проверь tool call по погоде" || !createdProjectless {
		t.Fatalf("desktop create cwd/prompt/projectless = %q/%q/%v, want projectless Chat prompt", createdCWD, createdPrompt, createdProjectless)
	}
	if len(stub.threadStartCalls) != 0 || len(stub.turnStartCalls) != 0 {
		t.Fatalf("app-server calls = %#v/%#v, want no fallback", stub.threadStartCalls, stub.turnStartCalls)
	}
	thread, err := service.store.GetThread(ctx, "new-chat-thread")
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if thread == nil || thread.ProjectName != "Chats" || thread.CWD != "" {
		t.Fatalf("thread = %#v, want stored Chats thread without cwd", thread)
	}
	catalog, err := service.projectCatalog(ctx)
	if err != nil {
		t.Fatalf("projectCatalog failed: %v", err)
	}
	if len(catalog.Chats) != 1 || catalog.Chats[0].ID != "new-chat-thread" {
		t.Fatalf("catalog.Chats = %#v, want new chat thread", catalog.Chats)
	}
}

func TestNewChatCWDUsesFallbackSlugAndCollisionSuffix(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fixedNow := time.Date(2026, 5, 5, 14, 32, 5, 0, time.Local)
	existing := filepath.Join(root, "2026-05-05", "chat-143205")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("MkdirAll(existing) failed: %v", err)
	}

	cwd, directoryName, err := createCodexChatCWD(root, "Привет мир", fixedNow)
	if err != nil {
		t.Fatalf("createCodexChatCWD failed: %v", err)
	}
	want := filepath.Join(root, "2026-05-05", "chat-143205-2")
	if cwd != want || directoryName != "chat-143205-2" {
		t.Fatalf("cwd=%q directoryName=%q, want %q and chat-143205-2", cwd, directoryName, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("expected fallback cwd %q to exist as directory, info=%#v err=%v", want, info, err)
	}
}

func TestSummaryPanelDoesNotShowStalePendingUserInputButtons(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "thread-stale-pending",
		Title:       "Stale pending",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "active",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:             thread,
		LatestTurnID:       "new-turn",
		LatestTurnStatus:   "inProgress",
		LatestProgressText: "Working on new turn.",
		LatestProgressFP:   "new-progress",
	}
	pending := &model.PendingApproval{
		RequestID:   "old-request",
		ThreadID:    thread.ID,
		TurnID:      "old-turn",
		PromptKind:  "user_input",
		Question:    "Old choice?",
		Status:      "pending",
		PayloadJSON: `{"questions":[{"id":"choice","question":"Old choice?","options":[{"label":"Old option","description":"Old."}]}]}`,
	}

	message, buttons, _ := service.renderSummaryPanel(ctx, thread, snapshot, pending)
	if strings.Contains(message.Text, "Old choice?") {
		t.Fatalf("summary text = %q, want no stale pending question", message.Text)
	}
	if callbackTokenForButton(buttons, "Old option") != "" {
		t.Fatalf("summary buttons = %#v, want no stale pending choice button", buttons)
	}
}

func TestTrackedThreadsSkipsRecentlyChangedTerminalThreadWithoutObserver(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "recent-terminal",
		Title:       "Recent terminal",
		ProjectName: "Codex",
		UpdatedAt:   time.Now().UTC().Unix(),
		Status:      "completed",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	oldSnapshot := model.ThreadSnapshotState{
		ThreadUpdatedAt:      thread.UpdatedAt - 120,
		LastSeenThreadStatus: "completed",
		LastSeenTurnID:       "turn-old",
		LastSeenTurnStatus:   "completed",
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, oldSnapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	tracked := service.trackedThreads(ctx, 10)
	for _, candidate := range tracked {
		if candidate.ID == thread.ID {
			t.Fatalf("tracked threads include passive terminal change after observer removal: %#v", tracked)
		}
	}
}

func TestTrackedThreadsSkipsArchivedCurrentPanelThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "archived-current-panel",
		Title:       "Archived current panel",
		ProjectName: "Codex",
		UpdatedAt:   time.Now().UTC().Unix(),
		Status:      "active",
		Archived:    true,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:      123456789,
		TopicID:     0,
		ProjectName: "Codex",
		ThreadID:    thread.ID,
		SourceMode:  model.PanelSourceExplicit,
		Status:      "active",
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	tracked := service.trackedThreads(ctx, 10)
	for _, candidate := range tracked {
		if candidate.ID == thread.ID {
			t.Fatalf("tracked threads include archived current panel thread: %#v", tracked)
		}
	}
}

func TestTrackedThreadsSkipsRecentTerminalChangeThatPredatesObserveEnable(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC().Unix()
	if err := service.store.UpsertThread(ctx, model.Thread{
		ID:          "recent-before-enable",
		Title:       "Recent but old for observer",
		ProjectName: "Codex",
		UpdatedAt:   now - 30,
		Status:      "completed",
	}); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	tracked := service.trackedThreads(ctx, 10)
	for _, thread := range tracked {
		if thread.ID == "recent-before-enable" {
			t.Fatalf("tracked threads unexpectedly include passive terminal change: %#v", tracked)
		}
	}
}

func TestTrackedThreadsSkipsNotLoadedThreadWithStaleActiveTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "not-loaded-stale-active",
		Title:        "Not loaded stale active",
		ProjectName:  "Codex",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "notLoaded",
		ActiveTurnID: "stale-turn",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}

	tracked := service.trackedThreads(ctx, 10)
	for _, candidate := range tracked {
		if candidate.ID == thread.ID {
			t.Fatalf("tracked threads include notLoaded thread with stale active turn: %#v", tracked)
		}
	}
}

func TestCurrentPanelThreadIDsSkipTerminalFeishuInputPanels(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	threadA := model.Thread{ID: "thread-a", Title: "A", ProjectName: "Codex", UpdatedAt: time.Now().UTC().Unix(), Status: "idle"}
	threadB := model.Thread{ID: "thread-b", Title: "B", ProjectName: "Codex", UpdatedAt: time.Now().UTC().Unix(), Status: "idle"}
	if err := service.store.UpsertThread(ctx, threadA); err != nil {
		t.Fatalf("UpsertThread(threadA) failed: %v", err)
	}
	if err := service.store.UpsertThread(ctx, threadB); err != nil {
		t.Fatalf("UpsertThread(threadB) failed: %v", err)
	}

	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         threadA.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 1,
		ToolMessageID:    2,
		OutputMessageID:  3,
		CurrentTurnID:    "turn-a",
		Status:           "completed",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel(feishu_input terminal) failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         threadB.ID,
		SourceMode:       "explicit",
		SummaryMessageID: 11,
		ToolMessageID:    12,
		OutputMessageID:  13,
		CurrentTurnID:    "turn-b",
		Status:           "completed",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel(explicit terminal) failed: %v", err)
	}

	ids := service.currentPanelThreadIDs(ctx)
	foundA := false
	foundB := false
	for _, id := range ids {
		if id == threadA.ID {
			foundA = true
		}
		if id == threadB.ID {
			foundB = true
		}
	}

	if foundA {
		t.Fatalf("currentPanelThreadIDs unexpectedly include terminal feishu_input panel: %#v", ids)
	}
	if foundB {
		t.Fatalf("currentPanelThreadIDs unexpectedly include terminal explicit panel: %#v", ids)
	}
}

func TestCurrentPanelThreadIDsSkipTerminalExplicitPanels(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	thread := model.Thread{ID: "thread-explicit-terminal", Title: "Explicit", ProjectName: "Codex", UpdatedAt: time.Now().UTC().Unix(), Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}

	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         thread.ID,
		SourceMode:       "explicit",
		SummaryMessageID: 1,
		ToolMessageID:    2,
		OutputMessageID:  3,
		CurrentTurnID:    "turn-explicit",
		Status:           "completed",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel(explicit terminal) failed: %v", err)
	}

	ids := service.currentPanelThreadIDs(ctx)
	for _, id := range ids {
		if id == thread.ID {
			t.Fatalf("currentPanelThreadIDs unexpectedly include terminal explicit panel: %#v", ids)
		}
	}
}

func TestFeishuTopicMappedThreadsRemainTrackedAfterTerminalPanel(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	thread := model.Thread{ID: "thread-feishu-topic-subscription", Title: "Feishu topic subscription", ProjectName: "Codex", UpdatedAt: time.Now().UTC().Unix(), Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 1,
		CurrentTurnID:    "turn-completed",
		Status:           "completed",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	if err := service.store.SupersedeCurrentThreadPanelsExcept(ctx, thread.ID, 0, 0); err != nil {
		t.Fatalf("SupersedeCurrentThreadPanelsExcept failed: %v", err)
	}
	if err := service.store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            123456789,
		OpenChatID:        "oc_test",
		ThreadID:          thread.ID,
		RootMessageID:     9001,
		RootOpenMessageID: "om_test",
		FeishuThreadID:    "omt_test",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}

	tracked := service.trackedThreads(ctx, 10)
	found := false
	for _, candidate := range tracked {
		if candidate.ID == thread.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("trackedThreads = %#v, want Feishu topic-mapped thread", tracked)
	}
	if !service.threadNeedsLiveSync(ctx, thread.ID) {
		t.Fatal("threadNeedsLiveSync returned false for Feishu topic-mapped thread")
	}
}

func TestAttachTrackedMarksArchivedFeishuTopicThreadFromResumeError(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{ID: "thread-feishu-topic-archived", Title: "Archived topic", ProjectName: "Codex", CWD: "/tmp/project", UpdatedAt: time.Now().UTC().Unix(), Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            123456789,
		OpenChatID:        "oc_archived",
		ThreadID:          thread.ID,
		RootMessageID:     9001,
		RootOpenMessageID: "om_archived",
		FeishuThreadID:    "omt_archived",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}
	stub := &stubSession{threadResumeErr: fmt.Errorf("session %s is archived. Run `codex unarchive %s` to unarchive it first.", thread.ID, thread.ID)}
	service.live = stub
	service.liveConnected = true

	service.attachTracked(ctx)

	stored, err := service.store.GetThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if stored == nil || !stored.Archived {
		t.Fatalf("stored thread = %#v, want archived", stored)
	}
	if strings.TrimSpace(service.lastError) != "" {
		t.Fatalf("lastError = %q, want no daemon error for archived resume", service.lastError)
	}
}

func TestThreadNeedsLiveSyncSkipsTerminalFeishuInputPanels(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	thread := model.Thread{ID: "thread-live", Title: "Live", ProjectName: "Codex", UpdatedAt: time.Now().UTC().Unix(), Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 1,
		ToolMessageID:    2,
		OutputMessageID:  3,
		CurrentTurnID:    "turn-1",
		Status:           "completed",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	if service.threadNeedsLiveSync(ctx, thread.ID) {
		t.Fatal("threadNeedsLiveSync returned true for terminal feishu_input panel")
	}

	if service.threadNeedsLiveSync(ctx, thread.ID) {
		t.Fatal("threadNeedsLiveSync returned true for idle explicit panel thread")
	}
	thread.Status = "inProgress"
	thread.ActiveTurnID = "turn-active"
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread(active panel) failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:        123456789,
		TopicID:       0,
		ProjectName:   "Codex",
		ThreadID:      thread.ID,
		SourceMode:    model.PanelSourceExplicit,
		CurrentTurnID: "turn-active",
		Status:        "inProgress",
	}); err != nil {
		t.Fatalf("CreateThreadPanel(active explicit) failed: %v", err)
	}
	if !service.threadNeedsLiveSync(ctx, thread.ID) {
		t.Fatal("threadNeedsLiveSync returned false for active explicit panel thread")
	}
}

func TestLiveToolNotificationStoresRunningCommandWithoutRenderingItAsCurrent(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	turnID := "turn-live-tool"
	thread := model.Thread{
		ID:           "thread-live-tool",
		Title:        "Live tool",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	staleCurrent := appserver.ThreadReadSnapshot{
		Thread:             thread,
		LatestTurnID:       turnID,
		LatestTurnStatus:   "inProgress",
		LatestProgressText: "printf 'alpha\\nbeta\\n'",
		LatestProgressFP:   "progress-alpha-fp",
		LatestToolID:       "cmd-alpha",
		LatestToolKind:     "commandExecution",
		LatestToolLabel:    "printf 'alpha\\nbeta\\n'",
		LatestToolStatus:   "completed",
		LatestToolOutput:   "alpha\nbeta\n",
		LatestToolFP:       "tool-alpha-fp",
		DetailItems: []model.DetailItem{
			{ID: "cmd-alpha", Kind: model.DetailItemTool, Label: "printf 'alpha\\nbeta\\n'", Status: "completed"},
			{ID: "cmd-alpha:output", Kind: model.DetailItemOutput, Output: "alpha\nbeta\n"},
		},
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, appserver.CompactSnapshot(nil, staleCurrent, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	summaryMessage, _, summaryHash := service.renderSummaryPanel(ctx, thread, &staleCurrent, nil)
	_ = summaryMessage
	_, staleToolHash := service.renderToolPanel(ctx, thread, &staleCurrent)
	_, staleOutputHash := service.renderOutputPanel(ctx, thread, &staleCurrent)
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 201,
		ToolMessageID:    202,
		OutputMessageID:  203,
		CurrentTurnID:    turnID,
		Status:           "inProgress",
		ArchiveEnabled:   true,
		LastSummaryHash:  summaryHash,
		LastToolHash:     staleToolHash,
		LastOutputHash:   staleOutputHash,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	stub := &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: {
				"thread": map[string]any{
					"id":           thread.ID,
					"title":        thread.Title,
					"cwd":          thread.CWD,
					"status":       "active",
					"activeTurnId": turnID,
					"turns": []any{
						map[string]any{
							"id":     turnID,
							"status": "inProgress",
							"items": []any{
								map[string]any{
									"id":               "cmd-alpha",
									"type":             "commandExecution",
									"command":          "printf 'alpha\\nbeta\\n'",
									"status":           "completed",
									"aggregatedOutput": "alpha\nbeta\n",
								},
							},
						},
					},
				},
			},
		},
	}

	service.handleLiveEvent(ctx, stub, appserver.Event{
		Channel: "notification",
		Method:  "item/started",
		Params: map[string]any{
			"threadId": thread.ID,
			"turnId":   turnID,
			"item": map[string]any{
				"id":      "cmd-slow",
				"type":    "commandExecution",
				"command": "sleep 20; printf 'slow-command-done\\n'",
				"status":  "running",
			},
		},
	})

	renderedRunningTool := false
	resetCompletedOutput := false
	for _, edit := range sender.edits {
		switch edit.messageID {
		case 202:
			if strings.Contains(edit.text, "sleep 20") &&
				strings.Contains(edit.text, "slow-command-done") &&
				strings.Contains(edit.text, "Status: running") {
				renderedRunningTool = true
			}
		case 203:
			if strings.Contains(edit.text, "No completed tool output yet.") ||
				(strings.Contains(edit.text, "slow-command-done") && !strings.Contains(edit.text, "alpha")) {
				resetCompletedOutput = true
			}
		}
	}
	if renderedRunningTool {
		t.Fatalf("running command was rendered as current tool; edits=%#v", sender.edits)
	}
	if resetCompletedOutput {
		t.Fatalf("completed output was reset by running live tool; edits=%#v", sender.edits)
	}
	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil {
		t.Fatal("snapshot = nil")
	}
	var current appserver.ThreadReadSnapshot
	if err := json.Unmarshal(stored.CompactJSON, &current); err != nil {
		t.Fatalf("unmarshal CompactJSON failed: %v", err)
	}
	if got := current.LatestToolLabel; !strings.Contains(got, "sleep 20") {
		t.Fatalf("LatestToolLabel = %q, want running sleep command", got)
	}
	if got, want := current.LatestToolStatus, "running"; got != want {
		t.Fatalf("LatestToolStatus = %q, want %q", got, want)
	}
	toolText, _ := service.renderToolPanel(ctx, thread, &current)
	if strings.Contains(toolText, "sleep 20") || strings.Contains(toolText, "Status: running") {
		t.Fatalf("rendered tool = %q, want running command hidden", toolText)
	}
	if !strings.Contains(toolText, "printf") || !strings.Contains(toolText, "Status: completed") {
		t.Fatalf("rendered tool = %q, want last completed command", toolText)
	}
	outputText, _ := service.renderOutputPanel(ctx, thread, &current)
	if !strings.Contains(outputText, "alpha") || strings.Contains(outputText, "slow-command-done") {
		t.Fatalf("rendered output = %q, want last completed output", outputText)
	}

	current.LatestToolStatus = "completed"
	current.LatestToolOutput = "slow-command-done\n"
	current.LatestToolFP = "tool-slow-completed-fp"
	if err := service.store.UpsertSnapshot(ctx, thread.ID, appserver.CompactSnapshot(stored, current, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot(completed tool) failed: %v", err)
	}
	if service.applyLiveToolSnapshot(ctx, thread.ID, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
		LatestToolID:     "cmd-slow",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "sleep 20; printf 'slow-command-done\\n'",
		LatestToolStatus: "running",
		LatestToolFP:     "late-running-fp",
	}) {
		t.Fatal("late running live tool update downgraded completed tool")
	}
	stored, err = service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot(after late live update) failed: %v", err)
	}
	if err := json.Unmarshal(stored.CompactJSON, &current); err != nil {
		t.Fatalf("unmarshal CompactJSON(after late live update) failed: %v", err)
	}
	if got, want := current.LatestToolStatus, "completed"; got != want {
		t.Fatalf("LatestToolStatus(after late live update) = %q, want %q", got, want)
	}
}

func TestLiveToolNotificationIgnoresOlderTurnAfterNewerCompletion(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "thread-old-live-tool",
		Title:       "Old live tool",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		UpdatedAt:   time.Now().UTC().Unix(),
		Status:      "idle",
	}
	current := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "01900000-0000-7000-8000-000000000020",
		LatestTurnStatus: "completed",
		LatestToolID:     "cmd-new",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "printf 'alpha\\nbeta\\n'",
		LatestToolStatus: "completed",
		LatestToolOutput: "alpha\nbeta\n",
		LatestToolFP:     "tool-new-completed",
		LatestFinalText:  "OK_LIVE_COMMANDS_printf",
		LatestFinalFP:    "final-new",
	}
	state := appserver.CompactSnapshot(nil, current, time.Now().UTC())
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, state); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	applied := service.applyLiveToolSnapshot(ctx, thread.ID, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "01900000-0000-7000-8000-000000000010",
		LatestTurnStatus: "inProgress",
		LatestToolID:     "cmd-old",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "date",
		LatestToolStatus: "completed",
		LatestToolOutput: "Sat May  2\n",
		LatestToolFP:     "tool-old-completed",
	})
	if applied {
		t.Fatal("older live tool update overwrote newer completed turn")
	}
	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	var compact appserver.ThreadReadSnapshot
	if err := json.Unmarshal(stored.CompactJSON, &compact); err != nil {
		t.Fatalf("unmarshal compact snapshot: %v", err)
	}
	if compact.LatestTurnID != current.LatestTurnID {
		t.Fatalf("LatestTurnID = %q, want %q", compact.LatestTurnID, current.LatestTurnID)
	}
	if strings.Contains(compact.LatestToolLabel, "date") {
		t.Fatalf("LatestToolLabel = %q, want newer command preserved", compact.LatestToolLabel)
	}
}

func TestLiveToolNotificationIgnoresOlderSameTurnToolAfterNewerTool(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "thread-old-same-turn-tool",
		Title:        "Old same turn tool",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: "turn-same",
	}
	current := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-same",
		LatestTurnStatus: "inProgress",
		LatestToolID:     "cmd-range-3",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "/tmp/math.py 60001 90000",
		LatestToolStatus: "completed",
		LatestToolOutput: "RANGE 60001 90000\n",
		LatestToolFP:     "tool-range-3-completed",
		DetailItems: []model.DetailItem{
			{ID: "cmd-range-1", Kind: model.DetailItemTool, Label: "/tmp/math.py 1 30000", Status: "completed"},
			{ID: "cmd-range-1:output", Kind: model.DetailItemOutput, Output: "RANGE 1 30000\n"},
			{ID: "cmd-range-2", Kind: model.DetailItemTool, Label: "/tmp/math.py 30001 60000", Status: "completed"},
			{ID: "cmd-range-2:output", Kind: model.DetailItemOutput, Output: "RANGE 30001 60000\n"},
			{ID: "cmd-range-3", Kind: model.DetailItemTool, Label: "/tmp/math.py 60001 90000", Status: "completed"},
			{ID: "cmd-range-3:output", Kind: model.DetailItemOutput, Output: "RANGE 60001 90000\n"},
		},
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, appserver.CompactSnapshot(nil, current, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	applied := service.applyLiveToolSnapshot(ctx, thread.ID, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-same",
		LatestTurnStatus: "inProgress",
		LatestToolID:     "cmd-range-1",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "/tmp/math.py 1 30000",
		LatestToolStatus: "running",
		LatestToolFP:     "tool-range-1-late-running",
	})
	if applied {
		t.Fatal("older same-turn live tool update overwrote newer tool")
	}
	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	var compact appserver.ThreadReadSnapshot
	if err := json.Unmarshal(stored.CompactJSON, &compact); err != nil {
		t.Fatalf("unmarshal compact snapshot: %v", err)
	}
	if compact.LatestToolID != current.LatestToolID {
		t.Fatalf("LatestToolID = %q, want %q", compact.LatestToolID, current.LatestToolID)
	}

	applied = service.applyLiveToolSnapshot(ctx, thread.ID, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-same",
		LatestTurnStatus: "inProgress",
		LatestToolID:     "cmd-range-4",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "/tmp/math.py 90001 120000",
		LatestToolStatus: "running",
		LatestToolFP:     "tool-range-4-running",
	})
	if !applied {
		t.Fatal("new same-turn live tool update was rejected")
	}
	stored, err = service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot(after newer tool) failed: %v", err)
	}
	if err := json.Unmarshal(stored.CompactJSON, &compact); err != nil {
		t.Fatalf("unmarshal compact snapshot(after newer tool): %v", err)
	}
	if compact.LatestToolID != "cmd-range-4" {
		t.Fatalf("LatestToolID(after newer tool) = %q, want cmd-range-4", compact.LatestToolID)
	}
}

func TestPollSnapshotWithoutToolDoesNotPreserveSameTurnRunningToolAsCurrent(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-live-tool-preserve"
	thread := model.Thread{
		ID:           "thread-live-tool-preserve",
		Title:        "Live tool preserve",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	firstSeen := time.Date(2026, time.May, 1, 23, 46, 1, 0, time.UTC)
	previousCurrent := appserver.ThreadReadSnapshot{
		Thread:              thread,
		LatestTurnID:        turnID,
		LatestTurnStatus:    "inProgress",
		LatestToolID:        "cmd-slow",
		LatestToolKind:      "commandExecution",
		LatestToolLabel:     "sleep 20; printf 'slow-command-done\\n'",
		LatestToolStatus:    "running",
		LatestToolFP:        "cmd-slow-running-fp",
		LatestToolStartedAt: firstSeen.Format(time.RFC3339Nano),
		LatestToolUpdatedAt: firstSeen.Format(time.RFC3339Nano),
	}
	previous := appserver.CompactSnapshot(nil, previousCurrent, firstSeen)
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot(previous) failed: %v", err)
	}
	pollWithoutTool := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
	}
	next := appserver.CompactSnapshot(&previous, pollWithoutTool, firstSeen.Add(8*time.Second))
	if err := service.store.UpsertSnapshot(ctx, thread.ID, next); err != nil {
		t.Fatalf("UpsertSnapshot(next) failed: %v", err)
	}
	_, current, err := service.loadThreadPanelSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("loadThreadPanelSnapshot failed: %v", err)
	}

	text, _ := service.renderToolPanelAt(ctx, thread, current, firstSeen.Add(8*time.Second))

	if strings.Contains(text, "sleep 20") || strings.Contains(text, "Status: running") {
		t.Fatalf("rendered tool = %q, want omitted running tool hidden", text)
	}
	if !strings.Contains(text, "No completed tool yet.") {
		t.Fatalf("rendered tool = %q, want neutral completed-tool absence", text)
	}
	summaryMessages := service.renderSummaryPanelMarkdownAt(ctx, thread, current, nil, nil, firstSeen.Add(8*time.Second))
	if len(summaryMessages) != 1 {
		t.Fatalf("len(summaryMessages) = %d, want 1", len(summaryMessages))
	}
	if !strings.Contains(summaryMessages[0].CodexStatus, "Processing 8s") {
		t.Fatalf("codex status = %q, want elapsed run time", summaryMessages[0].CodexStatus)
	}
}

func TestPollSnapshotWithoutToolPreservesChatOriginLiveCurrentTool(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-chat-live-current-preserve"
	thread := model.Thread{
		ID:           "thread-chat-live-current-preserve",
		Title:        "chat live preserve",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	firstSeen := time.Date(2026, time.May, 1, 23, 46, 1, 0, time.UTC)
	previousCurrent := appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          turnID,
		LatestTurnStatus:      "inProgress",
		LatestToolID:          "cmd-slow",
		LatestToolKind:        "commandExecution",
		LatestToolLabel:       "sleep 20; printf 'slow-command-done\\n'",
		LatestToolStatus:      "running",
		LatestToolFP:          "cmd-slow-running-fp",
		LatestToolLiveCurrent: true,
		LatestToolStartedAt:   firstSeen.Format(time.RFC3339Nano),
		LatestToolUpdatedAt:   firstSeen.Format(time.RFC3339Nano),
		DetailItems: []model.DetailItem{
			{ID: "item-user", Kind: model.DetailItemUser, Text: "run slow command"},
			{ID: "cmd-slow", Kind: model.DetailItemTool, Label: "sleep 20; printf 'slow-command-done\\n'", Status: "running", FP: "cmd-slow-running-fp"},
		},
	}
	previous := appserver.CompactSnapshot(nil, previousCurrent, firstSeen)
	pollWithoutTool := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
		DetailItems: []model.DetailItem{
			{ID: "item-user", Kind: model.DetailItemUser, Text: "run slow command"},
		},
	}

	service.preserveChatOriginLiveCurrentTool(ctx, &pollWithoutTool, &previous)

	if !pollWithoutTool.LatestToolLiveCurrent {
		t.Fatal("LatestToolLiveCurrent = false, want preserved live current tool")
	}
	if got := pollWithoutTool.LatestToolLabel; !strings.Contains(got, "slow-command-done") {
		t.Fatalf("LatestToolLabel = %q, want preserved live current command", got)
	}
	text, _ := service.renderToolPanelAt(ctx, thread, &pollWithoutTool, firstSeen.Add(8*time.Second))
	if !strings.Contains(text, "Current tool:") || !strings.Contains(text, "slow-command-done") || !strings.Contains(text, "Status: running") {
		t.Fatalf("rendered tool = %q, want preserved current command", text)
	}
	if strings.Contains(text, "item-user") {
		t.Fatalf("rendered tool = %q, want no duplicated user detail", text)
	}
}

func TestPollSnapshotWithOlderCompletedToolPreservesChatOriginLiveCurrentTool(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-chat-live-current-over-completed"
	thread := model.Thread{
		ID:           "thread-chat-live-current-over-completed",
		Title:        "chat live preserve over completed",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	firstSeen := time.Date(2026, time.May, 1, 23, 48, 1, 0, time.UTC)
	previousCurrent := appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          turnID,
		LatestTurnStatus:      "inProgress",
		LatestToolID:          "cmd-sleep20",
		LatestToolKind:        "commandExecution",
		LatestToolLabel:       "sleep 20",
		LatestToolStatus:      "running",
		LatestToolFP:          "cmd-sleep20-running-fp",
		LatestToolLiveCurrent: true,
		LatestToolStartedAt:   firstSeen.Add(10 * time.Second).Format(time.RFC3339Nano),
		LatestToolUpdatedAt:   firstSeen.Add(10 * time.Second).Format(time.RFC3339Nano),
		DetailItems: []model.DetailItem{
			{ID: "item-user", Kind: model.DetailItemUser, Text: "run two sleeps"},
			{ID: "cmd-sleep10", Kind: model.DetailItemTool, Label: "sleep 10", Status: "completed", FP: "cmd-sleep10-completed-fp"},
			{ID: "cmd-sleep10:output", Kind: model.DetailItemOutput, Output: "sleep10 done\n"},
			{ID: "cmd-sleep20", Kind: model.DetailItemTool, Label: "sleep 20", Status: "running", FP: "cmd-sleep20-running-fp"},
		},
	}
	previous := appserver.CompactSnapshot(nil, previousCurrent, firstSeen.Add(12*time.Second))
	pollWithOlderCompleted := appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          turnID,
		LatestTurnStatus:      "inProgress",
		LatestToolID:          "cmd-sleep10",
		LatestToolKind:        "commandExecution",
		LatestToolLabel:       "sleep 10",
		LatestToolStatus:      "completed",
		LatestToolOutput:      "sleep10 done\n",
		LatestToolFP:          "cmd-sleep10-completed-fp",
		LatestToolStartedAt:   firstSeen.Format(time.RFC3339Nano),
		LatestToolUpdatedAt:   firstSeen.Add(10 * time.Second).Format(time.RFC3339Nano),
		LatestToolLiveCurrent: false,
		DetailItems: []model.DetailItem{
			{ID: "item-user", Kind: model.DetailItemUser, Text: "run two sleeps"},
			{ID: "cmd-sleep10", Kind: model.DetailItemTool, Label: "sleep 10", Status: "completed", FP: "cmd-sleep10-completed-fp"},
			{ID: "cmd-sleep10:output", Kind: model.DetailItemOutput, Output: "sleep10 done\n"},
		},
	}

	service.preserveChatOriginLiveCurrentTool(ctx, &pollWithOlderCompleted, &previous)

	if !pollWithOlderCompleted.LatestToolLiveCurrent {
		t.Fatal("LatestToolLiveCurrent = false, want preserved live current tool")
	}
	if got := pollWithOlderCompleted.LatestToolLabel; got != "sleep 20" {
		t.Fatalf("LatestToolLabel = %q, want preserved live current command", got)
	}
	text, _ := service.renderToolPanelAt(ctx, thread, &pollWithOlderCompleted, firstSeen.Add(15*time.Second))
	if !strings.Contains(text, "Current tool:") || !strings.Contains(text, "sleep 20") || !strings.Contains(text, "Status: running") {
		t.Fatalf("rendered tool = %q, want preserved current command", text)
	}
	if strings.Contains(text, "Last completed tool:") || strings.Contains(text, "sleep 10") {
		t.Fatalf("rendered tool = %q, want older completed command hidden while current command is live", text)
	}
	outputText, _ := service.renderOutputPanel(ctx, thread, &pollWithOlderCompleted)
	if !strings.Contains(outputText, "sleep10 done") {
		t.Fatalf("rendered output = %q, want last completed output preserved", outputText)
	}
}

func TestSnapshotHasPassiveChangeIgnoresIdenticalTerminalReplay(t *testing.T) {
	t.Parallel()

	thread := model.Thread{
		ID:          "thread-passive",
		Title:       "Passive",
		ProjectName: "Codex",
		Status:      "idle",
	}
	current := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-1",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-fp-1",
		LatestFinalText:  "Done.",
	}
	previous := appserver.CompactSnapshot(nil, current, time.Now().UTC())

	if snapshotHasPassiveChange(&previous, &current) {
		t.Fatal("snapshotHasPassiveChange returned true for identical terminal replay")
	}

	current.LatestFinalFP = "upgraded-fingerprint"
	current.LatestFinalText = "Done."
	if !snapshotHasPassiveChange(&previous, &current) {
		t.Fatal("snapshotHasPassiveChange returned false for same terminal turn with changed final fingerprint")
	}

	current.LatestTurnID = "turn-2"
	current.LatestFinalFP = "final-fp-2"
	current.LatestFinalText = "Done again."
	if !snapshotHasPassiveChange(&previous, &current) {
		t.Fatal("snapshotHasPassiveChange returned false for new terminal turn")
	}
}

func TestPollTrackedSkipsFirstSeenRecentTerminalAfterObserverRemoval(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	now := time.Now().UTC().Unix()
	thread := model.Thread{
		ID:          "thread-catchup-terminal",
		Title:       "Catchup terminal",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   now,
		Status:      "completed",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	service.poll = &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: {
				"id":     thread.ID,
				"name":   thread.Title,
				"cwd":    thread.CWD,
				"status": "completed",
				"turns": []any{
					map[string]any{
						"id":     "turn-catchup",
						"status": "completed",
						"items": []any{
							map[string]any{
								"id":    "agent-final",
								"type":  "agentMessage",
								"phase": "final_answer",
								"text":  "CATCHUP_OK",
							},
						},
					},
				},
			},
		},
	}
	service.pollConnected = true

	service.pollTracked(ctx)

	if len(sender.messages) != 0 {
		t.Fatalf("messages = %#v, want no passive catchup after observer removal", sender.messages)
	}
}

func TestPollTrackedDefersChatOriginEmptyInterruptedAndKeepsActiveState(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	logs := captureServiceLogs(service)
	ctx := context.Background()
	turnID := "turn-empty-interrupted"
	thread := model.Thread{
		ID:           "thread-empty-interrupted",
		Title:        "Empty interrupted",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	previousCurrent := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
	}
	previous := appserver.CompactSnapshot(nil, previousCurrent, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	service.poll = &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: diagnosticThreadReadPayload(thread, turnID, "interrupted"),
		},
	}
	service.pollConnected = true

	service.pollTracked(ctx)

	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil {
		t.Fatal("snapshot = nil")
	}
	if stored.LastSeenTurnStatus != "inProgress" {
		t.Fatalf("LastSeenTurnStatus = %q, want inProgress", stored.LastSeenTurnStatus)
	}
	if stored.LastCompletionFP != "" {
		t.Fatalf("LastCompletionFP = %q, want empty while deferred", stored.LastCompletionFP)
	}
	if stored.NextPollAfter == "" {
		t.Fatal("NextPollAfter is empty, want hot polling while deferred")
	}
	if !service.threadNeedsCatchupPolling(ctx, thread, stored) {
		t.Fatal("threadNeedsCatchupPolling = false, want deferred empty interrupted to keep polling")
	}
	state := loadTerminalGateState(t, service, ctx, terminalGateDeferKey(thread.ID, turnID))
	if state.EmptyInterruptedSeenCount != 1 || state.LastDecision != string(terminalGateDefer) {
		t.Fatalf("defer state = %#v, want one deferred empty interrupted", state)
	}
	got := logs.String()
	requireLogContains(t, got, `"event":"chat_origin_terminal_deferred"`)
	if strings.Contains(got, `"event":"chat_origin_turn_terminal"`) {
		t.Fatalf("terminal log should be deferred, got:\n%s", got)
	}
}

func TestPollTrackedDeferredInterruptedDoesNotOverwriteFreshLiveToolSnapshot(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-empty-interrupted-live-tool"
	thread := model.Thread{
		ID:           "thread-empty-interrupted-live-tool",
		Title:        "Empty interrupted live tool",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	stalePrevious := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
		DetailItems: []model.DetailItem{
			{ID: "user-1", Kind: model.DetailItemUser, Text: "run sleep"},
		},
	}, time.Now().UTC())
	freshLiveCurrent := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          turnID,
		LatestTurnStatus:      "inProgress",
		LatestToolID:          "cmd-sleep",
		LatestToolKind:        "commandExecution",
		LatestToolLabel:       "sleep 10",
		LatestToolStatus:      "running",
		LatestToolFP:          "cmd-sleep-running",
		LatestToolLiveCurrent: true,
		DetailItems: []model.DetailItem{
			{ID: "user-1", Kind: model.DetailItemUser, Text: "run sleep"},
			{ID: "cmd-sleep", Kind: model.DetailItemTool, Label: "sleep 10", Status: "running", FP: "cmd-sleep-running"},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, freshLiveCurrent); err != nil {
		t.Fatalf("UpsertSnapshot(freshLiveCurrent) failed: %v", err)
	}
	emptyInterrupted := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{ID: "user-1", Kind: model.DetailItemUser, Text: "run sleep"},
		},
	}

	handled, _ := service.applyChatOriginTerminalGate(ctx, "poll_tracked", &emptyInterrupted, &stalePrevious)
	if !handled {
		t.Fatal("applyChatOriginTerminalGate = false, want deferred interrupted")
	}

	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil {
		t.Fatal("snapshot = nil")
	}
	_, current, err := service.loadThreadPanelSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("loadThreadPanelSnapshot failed: %v", err)
	}
	if !current.LatestToolLiveCurrent || current.LatestToolLabel != "sleep 10" {
		t.Fatalf("stored live tool = %q live=%v, want preserved sleep 10 current tool", current.LatestToolLabel, current.LatestToolLiveCurrent)
	}
	if stored.NextPollAfter == "" {
		t.Fatal("NextPollAfter is empty, want hot polling metadata on preserved live snapshot")
	}
}

func TestChatOriginHotPollContinuesForDeferredInterrupted(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-hot-poll-deferred-interrupted"
	thread := model.Thread{
		ID:           "thread-hot-poll-deferred-interrupted",
		Title:        "Hot poll deferred interrupted",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	previous := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	service.poll = &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: diagnosticThreadReadPayload(thread, turnID, "interrupted"),
		},
	}
	service.pollConnected = true

	keepGoing := service.chatOriginHotPollOnce(ctx, thread.ID, turnID)

	if !keepGoing {
		t.Fatal("chatOriginHotPollOnce returned false, want continued polling for deferred interrupted")
	}
	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil || stored.LastSeenTurnStatus != "inProgress" {
		t.Fatalf("snapshot = %#v, want deferred snapshot to keep previous inProgress status", stored)
	}
	if stored.NextPollAfter == "" {
		t.Fatal("NextPollAfter is empty, want continued hot polling")
	}
}

func TestPollTrackedDefersChatOriginPartialInterruptedAndKeepsActiveState(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	logs := captureServiceLogs(service)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	turnID := "turn-partial-interrupted"
	thread := model.Thread{
		ID:           "thread-partial-interrupted",
		Title:        "Partial interrupted",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	previousCurrent := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
		LatestToolID:     "cmd-slow",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "sleep 20; printf 'slow-command-done\\n'",
		LatestToolStatus: "running",
		LatestToolFP:     "cmd-slow-running",
	}
	previous := appserver.CompactSnapshot(nil, previousCurrent, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	summaryMessage, _, summaryHash := service.renderSummaryPanel(ctx, thread, &previousCurrent, nil)
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 1,
		CurrentTurnID:    turnID,
		Status:           previousCurrent.LatestTurnStatus,
		LastSummaryHash:  summaryHash,
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	_ = summaryMessage
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	service.poll = &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: diagnosticThreadReadPayloadWithCommentary(thread, turnID, "interrupted", "First process message."),
		},
	}
	service.pollConnected = true

	service.pollTracked(ctx)

	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil {
		t.Fatal("snapshot = nil")
	}
	if stored.LastSeenTurnStatus != "inProgress" {
		t.Fatalf("LastSeenTurnStatus = %q, want inProgress while partial interrupted is deferred", stored.LastSeenTurnStatus)
	}
	if stored.LastCompletionFP != "" {
		t.Fatalf("LastCompletionFP = %q, want empty while partial interrupted is deferred", stored.LastCompletionFP)
	}
	var compact appserver.ThreadReadSnapshot
	if err := json.Unmarshal(stored.CompactJSON, &compact); err != nil {
		t.Fatalf("unmarshal CompactJSON failed: %v", err)
	}
	if compact.LatestTurnStatus != "interrupted" {
		t.Fatalf("compact LatestTurnStatus = %q, want interrupted so display mapping can distinguish deferred progress", compact.LatestTurnStatus)
	}
	if len(compact.DetailItems) == 0 {
		t.Fatal("compact DetailItems is empty, want partial interrupted details preserved")
	}
	if stored.NextPollAfter == "" {
		t.Fatal("NextPollAfter is empty, want hot polling while partial interrupted is deferred")
	}
	if len(sender.edits) == 0 {
		t.Fatalf("sender edits = %#v, want progress panel update for partial interrupted", sender.edits)
	}
	if !strings.Contains(sender.edits[len(sender.edits)-1].text, "First process message.") {
		t.Fatalf("last edit = %q, want partial progress content", sender.edits[len(sender.edits)-1].text)
	}
	state := loadTerminalGateState(t, service, ctx, terminalGateDeferKey(thread.ID, turnID))
	if state.LastReason != "partial_interrupted" || state.LastDecision != string(terminalGateDefer) {
		t.Fatalf("defer state = %#v, want partial_interrupted defer", state)
	}
	got := logs.String()
	requireLogContains(t, got, `"event":"chat_origin_terminal_deferred"`)
	requireLogContains(t, got, `"reason":"partial_interrupted"`)
	if strings.Contains(got, `"event":"chat_origin_turn_terminal"`) {
		t.Fatalf("terminal log should be deferred, got:\n%s", got)
	}
}

func TestSnapshotHasPassiveChangeDetectsInterruptedVisibleStateChange(t *testing.T) {
	t.Parallel()

	thread := model.Thread{ID: "thread-interrupted-visible", Title: "Interrupted visible", ProjectName: "Codex", Status: "active"}
	previousSnapshot := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-interrupted-visible",
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Thinking about the answer.", FP: "agent-1", CommentaryIndex: 1},
		},
	}
	previous := compactDeferredProgressForTest(previousSnapshot)
	current := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-interrupted-visible",
		LatestTurnStatus: "interrupted",
		LatestToolID:     "tool-1",
		LatestToolLabel:  "go test ./...",
		LatestToolStatus: "running",
		LatestToolFP:     "tool-1-running",
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Thinking about the answer.", FP: "agent-1", CommentaryIndex: 1},
			{ID: "tool-1", Kind: model.DetailItemTool, Label: "go test ./...", Status: "running", FP: "tool-1-running"},
		},
	}

	if !snapshotHasPassiveChange(&previous, current) {
		t.Fatal("snapshotHasPassiveChange = false, want visible interrupted state change to trigger panel update")
	}
}

func compactDeferredProgressForTest(snapshot appserver.ThreadReadSnapshot) model.ThreadSnapshotState {
	state := appserver.CompactSnapshot(nil, snapshot, time.Now().UTC())
	state.LastCompletionFP = ""
	state.LastSeenTurnStatus = "inProgress"
	return state
}

func TestPollTrackedAcceptsChatOriginFinalInterruptedAndStopsHotPolling(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	logs := captureServiceLogs(service)
	ctx := context.Background()
	turnID := "turn-final-interrupted"
	thread := model.Thread{
		ID:           "thread-final-interrupted",
		Title:        "Final interrupted",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	previousCurrent := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
		LatestToolID:     "cmd-pwd",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "pwd",
		LatestToolStatus: "completed",
		LatestToolFP:     "cmd-pwd-completed",
	}
	previous := appserver.CompactSnapshot(nil, previousCurrent, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	service.poll = &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: diagnosticThreadReadPayloadWithFinal(thread, turnID, "interrupted", "OK_FINAL"),
		},
	}
	service.pollConnected = true

	service.pollTracked(ctx)

	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil {
		t.Fatal("snapshot = nil")
	}
	if stored.LastSeenTurnStatus != "interrupted" {
		t.Fatalf("LastSeenTurnStatus = %q, want interrupted final snapshot", stored.LastSeenTurnStatus)
	}
	if stored.LastCompletionFP == "" {
		t.Fatal("LastCompletionFP is empty, want final interrupted snapshot accepted")
	}
	if stored.NextPollAfter == "" {
		t.Fatal("NextPollAfter is empty, want normal follow-up poll timestamp")
	}
	if time.Until(parseTime(stored.NextPollAfter)) < 20*time.Second {
		t.Fatalf("NextPollAfter = %q, want non-hot polling after final", stored.NextPollAfter)
	}
	got := logs.String()
	requireLogContains(t, got, `"event":"observer_sync_result"`)
	if strings.Contains(got, `"event":"chat_origin_terminal_deferred"`) {
		t.Fatalf("final interrupted should not be deferred, got:\n%s", got)
	}
}

func TestChatOriginHotPollCapturesRunningTool(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-hot-poll-tool"
	thread := model.Thread{
		ID:           "thread-hot-poll-tool",
		Title:        "Hot poll tool",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	previous := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	payload := diagnosticThreadReadPayloadWithTool(thread, turnID, "inProgress")
	threadPayload := payload["thread"].(map[string]any)
	threadPayload["status"] = "active"
	threadPayload["activeTurnId"] = turnID
	turn := threadPayload["turns"].([]any)[0].(map[string]any)
	items := turn["items"].([]any)
	tool := items[1].(map[string]any)
	tool["status"] = "inProgress"
	tool["aggregatedOutput"] = nil
	service.poll = &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: payload,
		},
	}
	service.pollConnected = true

	keepGoing := service.chatOriginHotPollOnce(ctx, thread.ID, turnID)

	if !keepGoing {
		t.Fatal("chatOriginHotPollOnce returned false for in-progress turn")
	}
	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil {
		t.Fatal("snapshot = nil")
	}
	if stored.LastSeenTurnStatus != "inProgress" {
		t.Fatalf("LastSeenTurnStatus = %q, want inProgress", stored.LastSeenTurnStatus)
	}
	_, current, err := service.loadThreadPanelSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("loadThreadPanelSnapshot failed: %v", err)
	}
	if !strings.Contains(current.LatestToolLabel, "sleep 20") || current.LatestToolStatus != "inProgress" {
		t.Fatalf("stored tool = %q/%q, want running sleep command", current.LatestToolLabel, current.LatestToolStatus)
	}
	if stored.NextPollAfter == "" {
		t.Fatal("NextPollAfter is empty, want hot poll continuation")
	}
}

func TestRefreshThreadForOperationDefersEmptyInterrupted(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-refresh-empty-interrupted"
	thread := model.Thread{
		ID:           "thread-refresh-empty-interrupted",
		Title:        "Refresh empty interrupted",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	previousCurrent := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
	}
	previous := appserver.CompactSnapshot(nil, previousCurrent, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	stub := &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: diagnosticThreadReadPayload(thread, turnID, "interrupted"),
		},
	}

	refreshed, err := service.refreshThreadForOperation(ctx, stub, thread.ID, "thread_read")
	if err != nil {
		t.Fatalf("refreshThreadForOperation failed: %v", err)
	}
	if refreshed == nil || refreshed.Status != "active" || refreshed.ActiveTurnID != turnID {
		t.Fatalf("refreshed thread = %#v, want existing active thread", refreshed)
	}
	stored, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if stored == nil {
		t.Fatal("snapshot = nil")
	}
	if stored.LastSeenTurnStatus != "inProgress" {
		t.Fatalf("LastSeenTurnStatus = %q, want inProgress", stored.LastSeenTurnStatus)
	}
	if stored.LastCompletionFP != "" {
		t.Fatalf("LastCompletionFP = %q, want empty while deferred", stored.LastCompletionFP)
	}
	if stored.NextPollAfter == "" {
		t.Fatal("NextPollAfter is empty, want hot polling while deferred")
	}
}

func TestRefreshThreadForOperationTerminalCompletedToolReplacesLiveCurrent(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	turnID := "turn-refresh-terminal-tool"
	thread := model.Thread{
		ID:           "thread-refresh-terminal-tool",
		Title:        "Refresh terminal tool",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: turnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	previous := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          turnID,
		LatestTurnStatus:      "inProgress",
		LatestToolID:          "cmd-running",
		LatestToolKind:        "commandExecution",
		LatestToolLabel:       "sleep 10",
		LatestToolStatus:      "running",
		LatestToolFP:          "cmd-running-fp",
		LatestToolLiveCurrent: true,
		DetailItems: []model.DetailItem{
			{ID: "user-item", Kind: model.DetailItemUser, Text: "hello"},
			{ID: "cmd-running", Kind: model.DetailItemTool, Label: "sleep 10", Status: "running", FP: "cmd-running-fp"},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, previous); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	stub := &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: diagnosticThreadReadPayloadWithFinal(thread, turnID, "completed", "OK_FINAL"),
		},
	}

	if _, err := service.refreshThreadForOperation(ctx, stub, thread.ID, "thread_read"); err != nil {
		t.Fatalf("refreshThreadForOperation failed: %v", err)
	}

	_, current, err := service.loadThreadPanelSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("loadThreadPanelSnapshot failed: %v", err)
	}
	if current.LatestToolLiveCurrent {
		t.Fatal("LatestToolLiveCurrent = true, want completed thread/read tool to replace live current")
	}
	if got := current.LatestToolLabel; !strings.Contains(got, "slow-command-done") {
		t.Fatalf("LatestToolLabel = %q, want completed thread/read tool", got)
	}
	if got := current.LatestToolStatus; got != "completed" {
		t.Fatalf("LatestToolStatus = %q, want completed", got)
	}
	if got := current.LatestTurnStatus; got != "completed" {
		t.Fatalf("LatestTurnStatus = %q, want completed", got)
	}
}

func TestPollTrackedSkipsThreadNotLoadedWithoutRepair(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	thread := model.Thread{
		ID:          "thread-not-loaded",
		Title:       "Not loaded",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		UpdatedAt:   time.Now().UTC().Unix(),
		Status:      "active",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	service.poll = &stubSession{threadReadErr: errors.New("map[code:-32600 message:thread not loaded: thread-not-loaded]")}
	service.pollConnected = true

	service.pollTracked(ctx)

	repair, err := service.store.GetState(ctx, "control.repair_request")
	if err != nil {
		t.Fatalf("GetState(control.repair_request) failed: %v", err)
	}
	if strings.TrimSpace(repair) != "" {
		t.Fatalf("repair request = %q, want empty for thread not loaded", repair)
	}
	got := logs.String()
	requireLogContains(t, got, `"event":"thread_read_skipped"`)
	requireLogContains(t, got, `"reason":"thread_not_loaded"`)
	if strings.Contains(got, `"event":"repair_requested"`) {
		t.Fatalf("unexpected repair_requested log for thread not loaded: %s", got)
	}
}

func TestRefreshObserverIndexSkipsSyncAfterObserverRemoval(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "thread-from-list",
		Title:        "From list",
		ProjectName:  "Codex",
		CWD:          `C:\Users\you\Projects\Codex`,
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "inProgress",
		ActiveTurnID: "turn-1",
	}
	service.poll = &stubSession{
		threadListResult: map[string]any{
			"threads": []any{
				map[string]any{
					"id":           thread.ID,
					"name":         thread.Title,
					"cwd":          thread.CWD,
					"updatedAt":    float64(thread.UpdatedAt),
					"status":       thread.Status,
					"activeTurnId": thread.ActiveTurnID,
				},
			},
		},
	}
	service.pollConnected = true

	service.refreshObserverIndex(ctx)

	if service.poll.(*stubSession).threadListCalls != 0 {
		t.Fatalf("thread list calls = %d, want 0 without passive observer", service.poll.(*stubSession).threadListCalls)
	}
	stored, err := service.store.GetThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if stored != nil {
		t.Fatalf("stored thread = %#v, want no passive observer index sync", stored)
	}
}

func TestRefreshObserverIndexSkipsSyncWithoutBackgroundObserver(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	stub := &stubSession{}
	service.poll = stub
	service.pollConnected = true

	service.refreshObserverIndex(ctx)

	if stub.threadListCalls != 0 {
		t.Fatalf("thread list calls = %d, want 0 without background observer", stub.threadListCalls)
	}
}

func TestPlainReplyToSyntheticPlanPromptUsesTurnSteer(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{ID: "synthetic-plan-thread", Title: "Synthetic", ProjectName: "Codex", CWD: `C:\Users\you\Projects\Codex`}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 777,
		ThreadID:  thread.ID,
		TurnID:    "turn-synthetic",
		EventID:   "synthetic-fp",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "Use option A", 777, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "turn-synthetic" {
		t.Fatalf("response = %#v, want thread/turn synthetic", response)
	}
	if len(stub.turnSteerCalls) != 1 {
		t.Fatalf("turnSteerCalls = %#v, want one steer", stub.turnSteerCalls)
	}
	if got := stub.turnSteerCalls[0]; got.threadID != thread.ID || got.turnID != "turn-synthetic" || got.message != "Use option A" {
		t.Fatalf("turn steer call = %#v, want synthetic plan answer", got)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no fallback start", stub.turnStartCalls)
	}
}

func TestPlainReplyToSyntheticPlanPromptFallsBackToTurnStart(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{ID: "synthetic-stale-thread", Title: "Synthetic stale", ProjectName: "Codex", CWD: `C:\Users\you\Projects\Codex`}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 778,
		ThreadID:  thread.ID,
		TurnID:    "turn-stale",
		EventID:   "synthetic-fp-stale",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	stub := &stubSession{turnSteerErr: errors.New("turn already completed")}
	service.live = stub
	service.liveConnected = true

	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "Start new turn instead", 778, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want fallback started turn", response)
	}
	if len(stub.turnSteerCalls) != 1 {
		t.Fatalf("turnSteerCalls = %#v, want one failed steer", stub.turnSteerCalls)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one fallback start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.threadID != thread.ID || got.message != "Start new turn instead" {
		t.Fatalf("turn start call = %#v, want fallback answer", got)
	}
}

func TestReplyToActiveThreadSteersActiveTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "active-reply-thread",
		Title:        "Active reply",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "active",
		ActiveTurnID: "turn-active",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThread(ctx, 123456789, 0, thread.ID, "Add this while running")
	if err != nil {
		t.Fatalf("sendInputToThread failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "turn-active" {
		t.Fatalf("response = %#v, want active turn steer", response)
	}
	if len(stub.turnSteerCalls) != 1 {
		t.Fatalf("turnSteerCalls = %#v, want one steer", stub.turnSteerCalls)
	}
	if got := stub.turnSteerCalls[0]; got.threadID != thread.ID || got.turnID != "turn-active" || got.message != "Add this while running" {
		t.Fatalf("turn steer call = %#v, want active turn input", got)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no parallel start", stub.turnStartCalls)
	}
}

func TestStaleActiveThreadWithFinalAnswerStartsNewTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "stale-active-final-thread",
		Title:        "Stale active final",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "active",
		ActiveTurnID: "turn-stale",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{
		threadReads: map[string]map[string]any{
			thread.ID: {
				"id":           thread.ID,
				"name":         thread.Title,
				"cwd":          thread.CWD,
				"status":       "inProgress",
				"activeTurnId": "turn-stale",
				"turns": []any{
					map[string]any{
						"id":     "turn-stale",
						"status": "inProgress",
						"items": []any{
							map[string]any{
								"id":   "user-1",
								"type": "userMessage",
								"content": []any{
									map[string]any{"type": "text", "text": "Original request"},
								},
							},
							map[string]any{
								"id":    "final-1",
								"type":  "agentMessage",
								"phase": "final_answer",
								"text":  "Done.",
							},
						},
					},
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThread(ctx, 123456789, 0, thread.ID, "Start after stale final")
	if err != nil {
		t.Fatalf("sendInputToThread failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want new started turn", response)
	}
	if len(stub.turnSteerCalls) != 0 {
		t.Fatalf("turnSteerCalls = %#v, want no stale steer", stub.turnSteerCalls)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one new turn", stub.turnStartCalls)
	}
	stored, err := service.store.GetThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if stored == nil || stored.ActiveTurnID != "started-turn" || stored.Status != "inProgress" {
		t.Fatalf("stored thread = %#v, want seeded started turn", stored)
	}
	snapshot, err := service.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if snapshot == nil || snapshot.LastSeenTurnID != "started-turn" || snapshot.LastSeenTurnStatus != "inProgress" {
		t.Fatalf("snapshot = %#v, want seeded started turn", snapshot)
	}
}

func TestReplyToActiveThreadDoesNotFallbackToTurnStartWhenSteerFails(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "active-not-steerable-thread",
		Title:        "Active not steerable",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "active",
		ActiveTurnID: "turn-active",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{turnSteerErr: errors.New("active turn is not steerable")}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "turn-active", "Do not fork this", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurn failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "I did not start a parallel turn.") {
		t.Fatalf("response = %#v, want no parallel-turn warning", response)
	}
	if len(stub.turnSteerCalls) != 1 {
		t.Fatalf("turnSteerCalls = %#v, want one failed steer", stub.turnSteerCalls)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no fallback start", stub.turnStartCalls)
	}
}

func TestFeishuInputToExistingThreadActivatesTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "existing-feishu-input-thread",
		Title:       "Existing Feishu input",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "old-turn",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "old-final",
		LatestFinalText:  "Done.",
	}, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "continue from Feishu", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want started turn on existing thread", response)
	}
	if len(sender.ensureTopicThreads) == 0 || sender.ensureTopicThreads[0].ID != thread.ID {
		t.Fatalf("EnsureThreadTopic calls = %#v, want activation for Feishu input", sender.ensureTopicThreads)
	}
	if len(sender.messages) == 0 {
		t.Fatalf("messages = %#v, want panel messages in Feishu topic", sender.messages)
	}
	for _, message := range sender.messages {
		if message.options.FeishuReplyToMessageID != 9001 || !message.options.FeishuReplyInThread || message.options.FeishuCodexThreadID != thread.ID {
			t.Fatalf("message options = %#v, want Feishu topic reply options", message.options)
		}
	}
}

func TestNoActiveTurnSteerFailureFallsBackToTurnStart(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "stale-no-active-thread",
		Title:        "Stale no active",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "active",
		ActiveTurnID: "turn-stale",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{turnSteerErr: errors.New("map[code:-32600 message:no active turn to steer]")}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThread(ctx, 123456789, 0, thread.ID, "Start because stale active is gone")
	if err != nil {
		t.Fatalf("sendInputToThread failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want fallback started turn", response)
	}
	if len(stub.turnSteerCalls) != 1 {
		t.Fatalf("turnSteerCalls = %#v, want one failed active steer", stub.turnSteerCalls)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one fallback start", stub.turnStartCalls)
	}
}

func TestFeishuInputOpensCodexDesktopThreadWhenEnabled(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000201",
		Title:       "Feishu desktop open",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true
	service.desktopInputDispatcher = &stubDesktopInputDispatcher{loadErr: fmt.Errorf("no-client-found")}
	var opened []string
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		opened = append(opened, threadID)
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "Open in desktop", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want started Feishu input", response)
	}
	if len(opened) != 1 || opened[0] != thread.ID {
		t.Fatalf("opened = %#v, want %s", opened, thread.ID)
	}
}

func TestFeishuInputUsesDesktopDispatcherWhenAvailable(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000203",
		Title:       "Feishu desktop dispatch",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true
	desktop := &stubDesktopInputDispatcher{
		startResult: map[string]any{
			"result": map[string]any{"turn": map[string]any{"id": "desktop-turn"}},
		},
	}
	service.desktopInputDispatcher = desktop
	var opened []string
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		opened = append(opened, threadID)
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "Open in desktop", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "desktop-turn" {
		t.Fatalf("response = %#v, want desktop turn", response)
	}
	if len(desktop.loadCalls) != 1 || desktop.loadCalls[0] != thread.ID {
		t.Fatalf("desktop loadCalls = %#v, want %s", desktop.loadCalls, thread.ID)
	}
	if len(opened) != 1 || opened[0] != thread.ID {
		t.Fatalf("opened = %#v, want %s", opened, thread.ID)
	}
	if len(desktop.startCalls) != 1 {
		t.Fatalf("desktop startCalls = %#v, want one", desktop.startCalls)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("app-server turnStartCalls = %#v, want none", stub.turnStartCalls)
	}
}

func TestFeishuImageInputUsesDesktopLocalImagePart(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000214",
		Title:       "Feishu desktop image",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	service.live = &stubSession{}
	service.liveConnected = true
	desktop := &stubDesktopInputDispatcher{
		startResult: map[string]any{
			"result": map[string]any{"turn": map[string]any{"id": "desktop-image-turn"}},
		},
	}
	service.desktopInputDispatcher = desktop
	service.desktopOpener = func(ctx context.Context, threadID string) error { return nil }

	text := "用户发送了一张图片，已保存到：/Users/example/.codex-feishu/data/feishu-attachments/7893bfdbd95a.jpg\n请读取并分析这张图片。"
	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", text, "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.TurnID != "desktop-image-turn" {
		t.Fatalf("response = %#v, want desktop image turn", response)
	}
	if len(desktop.startCalls) != 1 {
		t.Fatalf("desktop startCalls = %#v, want one", desktop.startCalls)
	}
	input, ok := desktop.startCalls[0]["input"].([]map[string]any)
	if !ok {
		t.Fatalf("desktop start input = %#v, want []map[string]any", desktop.startCalls[0]["input"])
	}
	if len(input) != 2 {
		t.Fatalf("desktop start input = %#v, want metadata text and localImage parts", input)
	}
	if got, want := input[0]["type"], "text"; got != want {
		t.Fatalf("metadata part type = %v, want %q", got, want)
	}
	if got := input[0]["text"].(string); !strings.Contains(got, "Files mentioned by the user") || !strings.Contains(got, "My request for Codex") {
		t.Fatalf("metadata text = %q, want Codex attachment metadata", got)
	}
	if got, want := input[1]["type"], "localImage"; got != want {
		t.Fatalf("image part type = %v, want %q", got, want)
	}
	if got, want := input[1]["path"], "/Users/example/.codex-feishu/data/feishu-attachments/7893bfdbd95a.jpg"; got != want {
		t.Fatalf("image part path = %v, want %q", got, want)
	}
}

func TestFeishuImageReplyUsesDesktopLocalImagePart(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:           "01900000-0000-7000-8000-000000000215",
		Title:        "Feishu desktop image reply",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "inProgress",
		ActiveTurnID: "turn-active",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	service.live = &stubSession{}
	service.liveConnected = true
	desktop := &stubDesktopInputDispatcher{
		steerResult: map[string]any{
			"result": map[string]any{"turn": map[string]any{"id": "desktop-image-steer"}},
		},
	}
	service.desktopInputDispatcher = desktop
	service.desktopOpener = func(ctx context.Context, threadID string) error { return nil }

	text := "The user sent an image saved at: /Users/example/.codex-feishu/data/feishu-attachments/reply.jpg\nPlease read and analyze this image."
	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, thread.ActiveTurnID, text, "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.TurnID != "desktop-image-steer" {
		t.Fatalf("response = %#v, want desktop image steer", response)
	}
	if len(desktop.steerInputs) != 1 {
		t.Fatalf("desktop steerInputs = %#v, want one", desktop.steerInputs)
	}
	input := desktop.steerInputs[0]
	if len(input) != 2 {
		t.Fatalf("desktop steer input = %#v, want metadata text and localImage parts", input)
	}
	if got, want := input[0]["type"], "text"; got != want {
		t.Fatalf("request part type = %v, want %q", got, want)
	}
	if got := input[0]["text"].(string); !strings.Contains(got, "Files mentioned by the user") || !strings.Contains(got, "My request for Codex") {
		t.Fatalf("metadata text = %q, want Codex attachment metadata", got)
	}
	if got, want := input[1]["type"], "localImage"; got != want {
		t.Fatalf("image part type = %v, want %q", got, want)
	}
	if got, want := input[1]["path"], "/Users/example/.codex-feishu/data/feishu-attachments/reply.jpg"; got != want {
		t.Fatalf("image part path = %v, want %q", got, want)
	}
}

func TestDesktopImageInputPreservesCodexAttachmentRequest(t *testing.T) {
	t.Parallel()

	text := "\n# Files mentioned by the user:\n\n" +
		"## prompt.jpg: /tmp/prompt.jpg\n\n" +
		"## My request for Codex:\n" +
		"看看这张图里有什么\n" +
		"<image name=[Image #1] path=\"/tmp/prompt.jpg\">\n" +
		"</image>"

	input := desktopTextInput(text)

	if len(input) != 2 {
		t.Fatalf("input = %#v, want request and localImage parts", input)
	}
	if got, want := input[0]["text"], "看看这张图里有什么"; got != want {
		t.Fatalf("request text = %v, want %q", got, want)
	}
	if got, want := input[1]["type"], "localImage"; got != want {
		t.Fatalf("image part type = %v, want %q", got, want)
	}
	if got, want := input[1]["path"], "/tmp/prompt.jpg"; got != want {
		t.Fatalf("image path = %v, want %q", got, want)
	}
}

func TestFeishuInputUsesDesktopDispatcherWhenLiveSessionUnavailable(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000207",
		Title:       "Feishu desktop without live",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	service.live = nil
	service.liveConnected = false
	desktop := &stubDesktopInputDispatcher{
		startResult: map[string]any{
			"result": map[string]any{"turn": map[string]any{"id": "desktop-no-live-turn"}},
		},
	}
	service.desktopInputDispatcher = desktop
	var opened []string
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		opened = append(opened, threadID)
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "Open in desktop", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "desktop-no-live-turn" {
		t.Fatalf("response = %#v, want desktop turn without live session", response)
	}
	if len(desktop.loadCalls) != 1 || desktop.loadCalls[0] != thread.ID {
		t.Fatalf("desktop loadCalls = %#v, want %s", desktop.loadCalls, thread.ID)
	}
	if len(opened) != 1 || opened[0] != thread.ID {
		t.Fatalf("opened = %#v, want %s", opened, thread.ID)
	}
	if len(desktop.startCalls) != 1 {
		t.Fatalf("desktop startCalls = %#v, want one", desktop.startCalls)
	}
}

func TestFeishuInputFallsBackWhenDesktopDispatcherUnavailable(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000204",
		Title:       "Feishu desktop fallback",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true
	service.desktopInputDispatcher = &stubDesktopInputDispatcher{loadErr: fmt.Errorf("no-client-found")}
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "Fallback", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want app-server fallback turn", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one fallback start", stub.turnStartCalls)
	}
}

func TestFeishuInputFallsBackWhenDesktopHistoryRetryTimesOut(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	service.cfg.RequestTimeout = time.Nanosecond
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000217",
		Title:       "Feishu desktop timeout fallback",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true
	service.desktopInputDispatcher = &stubDesktopInputDispatcher{loadErr: fmt.Errorf("no-client-found")}
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "今天是几月几号？农历，黄道信息给我一下", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want app-server fallback turn", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one fallback start", stub.turnStartCalls)
	}
}

func TestFeishuInputRetriesDesktopDispatcherAfterOpeningThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000206",
		Title:       "Feishu desktop retry",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true
	desktop := &stubDesktopInputDispatcher{
		loadErrs: []error{fmt.Errorf("no-client-found")},
		startResult: map[string]any{
			"result": map[string]any{"turn": map[string]any{"id": "desktop-retry-turn"}},
		},
	}
	service.desktopInputDispatcher = desktop
	var opened []string
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		opened = append(opened, threadID)
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "Retry in desktop", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "desktop-retry-turn" {
		t.Fatalf("response = %#v, want desktop retry turn", response)
	}
	if len(opened) != 1 || opened[0] != thread.ID {
		t.Fatalf("opened = %#v, want %s", opened, thread.ID)
	}
	if len(desktop.loadCalls) != 2 {
		t.Fatalf("desktop loadCalls = %#v, want retry after open", desktop.loadCalls)
	}
	if len(desktop.startCalls) != 1 {
		t.Fatalf("desktop startCalls = %#v, want one desktop start", desktop.startCalls)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("app-server turnStartCalls = %#v, want none", stub.turnStartCalls)
	}
}

func TestFeishuInputStartsDesktopTurnWhenReplySteerFindsNoActiveTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000205",
		Title:       "Feishu desktop reply start",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true
	desktop := &stubDesktopInputDispatcher{
		steerErr: fmt.Errorf("SteerTurnInactiveError: Cannot steer conversation 01900000-0000-7000-8000-000000000205 because its active turn already ended"),
		startResult: map[string]any{
			"result": map[string]any{"turn": map[string]any{"id": "desktop-reply-start"}},
		},
	}
	service.desktopInputDispatcher = desktop
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "old-turn", "Reply after idle", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "desktop-reply-start" {
		t.Fatalf("response = %#v, want desktop reply start", response)
	}
	if len(desktop.steerCalls) != 1 {
		t.Fatalf("steerCalls = %#v, want one attempted steer", desktop.steerCalls)
	}
	if len(desktop.steerRestoreMessages) != 1 {
		t.Fatalf("steerRestoreMessages = %#v, want one restore message", desktop.steerRestoreMessages)
	}
	if got, want := desktop.steerRestoreMessages[0]["cwd"], "/Users/example/project"; got != want {
		t.Fatalf("restoreMessage.cwd = %v, want %q", got, want)
	}
	contextPayload, ok := desktop.steerRestoreMessages[0]["context"].(map[string]any)
	if !ok {
		t.Fatalf("restoreMessage.context = %#v, want object", desktop.steerRestoreMessages[0]["context"])
	}
	roots, ok := contextPayload["workspaceRoots"].([]string)
	if !ok {
		t.Fatalf("workspaceRoots = %#v, want string slice", contextPayload["workspaceRoots"])
	}
	if len(roots) != 1 || roots[0] != "/Users/example/project" {
		t.Fatalf("workspaceRoots = %#v, want project cwd", roots)
	}
	if len(desktop.startCalls) != 1 {
		t.Fatalf("startCalls = %#v, want one start after no active turn", desktop.startCalls)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("app-server turnStartCalls = %#v, want none", stub.turnStartCalls)
	}
}

func TestFeishuInputStartsDesktopTurnWhenReplySteerReturnsEmptyTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000216",
		Title:       "Feishu desktop empty steer",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	service.live = &stubSession{}
	service.liveConnected = true
	desktop := &stubDesktopInputDispatcher{
		steerResult: map[string]any{"ok": true},
		startResult: map[string]any{
			"result": map[string]any{"turn": map[string]any{"id": "desktop-empty-steer-start"}},
		},
	}
	service.desktopInputDispatcher = desktop
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "old-ended-turn", "Reply after empty steer", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "desktop-empty-steer-start" {
		t.Fatalf("response = %#v, want desktop start after empty steer", response)
	}
	if len(desktop.steerCalls) != 1 {
		t.Fatalf("steerCalls = %#v, want one attempted steer", desktop.steerCalls)
	}
	if len(desktop.startCalls) != 1 {
		t.Fatalf("startCalls = %#v, want fallback start", desktop.startCalls)
	}
}

func TestFeishuInputFallsBackToLiveWhenDesktopCannotBuildDefaultModeTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = true
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000208",
		Title:       "Feishu desktop default fallback",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.setThreadCollaborationDefaultOverride(ctx, thread.ID); err != nil {
		t.Fatalf("setThreadCollaborationDefaultOverride failed: %v", err)
	}
	stub := &stubSession{
		models:       []appserver.ModelOption{{ID: "gpt-test", IsDefault: true}},
		turnSteerErr: fmt.Errorf("SteerTurnInactiveError: Cannot steer conversation 01900000-0000-7000-8000-000000000208 because its active turn already ended"),
	}
	service.live = stub
	service.liveConnected = true
	desktop := &stubDesktopInputDispatcher{
		steerErr: fmt.Errorf("SteerTurnInactiveError: Cannot steer conversation 01900000-0000-7000-8000-000000000208 because its active turn already ended"),
	}
	service.desktopInputDispatcher = desktop
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		return nil
	}

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "old-turn", "Reply after idle", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want live fallback turn", response)
	}
	if len(desktop.steerCalls) != 1 {
		t.Fatalf("desktop steerCalls = %#v, want one attempted steer", desktop.steerCalls)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("live turnStartCalls = %#v, want one fallback start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.collaborationMode != collaborationModeDefault {
		t.Fatalf("live turn start call = %#v, want default mode fallback", got)
	}
}

func TestFeishuInputDoesNotOpenCodexDesktopThreadWhenDisabled(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.OpenCodexDesktopOnFeishu = false
	ctx := context.Background()
	thread := model.Thread{
		ID:          "01900000-0000-7000-8000-000000000202",
		Title:       "Feishu desktop open disabled",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true
	var opened []string
	service.desktopOpener = func(ctx context.Context, threadID string) error {
		opened = append(opened, threadID)
		return nil
	}

	if _, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "Keep desktop still", "", model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("sendInputToThreadTurnFromSource failed: %v", err)
	}
	if len(opened) != 0 {
		t.Fatalf("opened = %#v, want none", opened)
	}
}

func TestActiveTurnMismatchRetriesFoundTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	oldTurnID := "01900000-0000-7000-8000-000000000101"
	foundTurnID := "01900000-0000-7000-8000-000000000102"
	thread := model.Thread{
		ID:           "active-mismatch-thread",
		Title:        "Active mismatch",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "active",
		ActiveTurnID: oldTurnID,
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{
		turnSteerErrs: []error{
			fmt.Errorf("map[code:-32600 message:expected active turn id `%s` but found `%s`]", oldTurnID, foundTurnID),
			nil,
		},
	}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThread(ctx, 123456789, 0, thread.ID, "Steer authoritative active turn")
	if err != nil {
		t.Fatalf("sendInputToThread failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != foundTurnID {
		t.Fatalf("response = %#v, want retry steer turn", response)
	}
	if len(stub.turnSteerCalls) != 2 {
		t.Fatalf("turnSteerCalls = %#v, want old then found", stub.turnSteerCalls)
	}
	if got := stub.turnSteerCalls[0].turnID; got != oldTurnID {
		t.Fatalf("first steer turn = %q, want old", got)
	}
	if got := stub.turnSteerCalls[1].turnID; got != foundTurnID {
		t.Fatalf("second steer turn = %q, want found", got)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no new parallel start", stub.turnStartCalls)
	}
}

func TestReplyToActiveThreadWithoutTurnIDDoesNotStartParallelTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "active-without-turn-thread",
		Title:       "Active missing turn",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "active",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThread(ctx, 123456789, 0, thread.ID, "Do not start")
	if err != nil {
		t.Fatalf("sendInputToThread failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "active turn id is not available") {
		t.Fatalf("response = %#v, want missing active turn warning", response)
	}
	if len(stub.turnSteerCalls) != 0 {
		t.Fatalf("turnSteerCalls = %#v, want no steer without turn id", stub.turnSteerCalls)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no parallel start", stub.turnStartCalls)
	}
}

func TestPlanCommandStartsPlanCollaborationMode(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	if err := service.store.SetState(ctx, codexReasoningStateKey, "high"); err != nil {
		t.Fatalf("SetState(reasoning) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "plan-command-thread",
		Title:       "Plan command",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/plan "+thread.ID+" propose options", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/plan) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want started plan turn", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	got := stub.turnStartCalls[0]
	if got.collaborationMode != collaborationModePlan || got.model != "gpt-test" || got.reasoningEffort != "high" {
		t.Fatalf("turn start options = %#v, want plan/gpt-test/high", got)
	}
}

func TestGoalCommandSetsCurrentThreadGoal(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "goal-command-thread",
		Title:       "Goal command",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 1,
		CurrentTurnID:    "turn-goal",
		Status:           "completed",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/goal keep working until tests pass", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/goal) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || !strings.Contains(response.Text, "Goal set") {
		t.Fatalf("response = %#v, want goal set response", response)
	}
	if len(stub.goalSetCalls) != 1 || stub.goalSetCalls[0].threadID != thread.ID || stub.goalSetCalls[0].goal != "keep working until tests pass" {
		t.Fatalf("goalSetCalls = %#v, want current thread goal", stub.goalSetCalls)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no turn start for /goal", stub.turnStartCalls)
	}
}

func TestGoalCommandCanUseExplicitThreadAndClear(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{ID: "explicit-goal-thread", Title: "Explicit goal", ProjectName: "Codex", Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	setResponse, err := service.handleCommandFromSource(ctx, 123456789, 0, "/goal "+thread.ID+" ship this feature", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/goal explicit) failed: %v", err)
	}
	if setResponse == nil || setResponse.ThreadID != thread.ID {
		t.Fatalf("set response = %#v, want explicit thread", setResponse)
	}
	if len(stub.goalSetCalls) != 1 || stub.goalSetCalls[0].goal != "ship this feature" {
		t.Fatalf("goalSetCalls = %#v, want explicit goal", stub.goalSetCalls)
	}

	clearResponse, err := service.handleCommandFromSource(ctx, 123456789, 0, "/goal "+thread.ID+" clear", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/goal clear) failed: %v", err)
	}
	if clearResponse == nil || clearResponse.ThreadID != thread.ID || !strings.Contains(clearResponse.Text, "Goal cleared") {
		t.Fatalf("clear response = %#v, want clear response", clearResponse)
	}
	if len(stub.goalClearCalls) != 1 || stub.goalClearCalls[0].threadID != thread.ID {
		t.Fatalf("goalClearCalls = %#v, want explicit clear", stub.goalClearCalls)
	}
}

func TestPlanCommandUnknownHeadWithoutImplicitRouteShowsUsage(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/plan first second third", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/plan no route) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "Usage: /plan <text>") {
		t.Fatalf("response = %#v, want /plan usage", response)
	}
	if len(stub.turnStartCalls) != 0 {
		t.Fatalf("turnStartCalls = %#v, want no explicit first-token start", stub.turnStartCalls)
	}
}

func TestPlanCommandUUIDLikeHeadStaysExplicit(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	explicitID := "01900000-0000-7000-8000-000000000999"

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/plan "+explicitID+" propose options", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/plan uuid text) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.Text, "Unknown thread: "+explicitID) {
		t.Fatalf("response = %#v, want explicit unknown UUID-like thread", response)
	}
}

func TestPlanCommandKnownThreadHeadStaysExplicit(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	explicit := model.Thread{
		ID:          "explicit-plan-thread",
		Title:       "Explicit plan",
		ProjectName: "Codex",
		CWD:         "/Users/example/other-project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, explicit); err != nil {
		t.Fatalf("UpsertThread(explicit) failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/plan "+explicit.ID+" propose options", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/plan known-thread text) failed: %v", err)
	}
	if response == nil || response.ThreadID != explicit.ID {
		t.Fatalf("response = %#v, want explicit thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.threadID != explicit.ID || got.message != "propose options" {
		t.Fatalf("turn start call = %#v, want explicit plan prompt", got)
	}
}

func TestChatTurnLifecycleLogsSuccessfulStart(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "diag-success-thread",
		Title:       "Diagnostics",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{threadReads: map[string]map[string]any{
		thread.ID: diagnosticThreadReadPayload(thread, "existing-turn", "completed"),
	}}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "keep this prompt private", collaborationModePlan, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurn failed: %v", err)
	}
	if response == nil || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want started-turn", response)
	}
	got := logs.String()
	requireLogContains(t, got, `"event":"chat_turn_input_start"`)
	requireLogContains(t, got, `"method":"ThreadResume"`)
	requireLogContains(t, got, `"method":"TurnStart"`)
	requireLogContains(t, got, `"event":"chat_origin_turn_marked"`)
	requireLogContains(t, got, `"collaboration_mode":"plan"`)
	requireLogContains(t, got, `"model":"gpt-test"`)
	requireLogContains(t, got, `"text_len":24`)
	requireLogContains(t, got, `"text_sha256"`)
	if strings.Contains(got, "keep this prompt private") {
		t.Fatalf("diagnostic log leaked prompt body: %s", got)
	}
}

func TestSnapshotHasPassiveChangeAllowsTerminalFinalAfterInterrupted(t *testing.T) {
	t.Parallel()

	previous := &model.ThreadSnapshotState{
		LastSeenTurnID:     "turn-terminal",
		LastSeenTurnStatus: "interrupted",
		LastCompletionFP:   "old-interrupted-fp",
	}
	current := &appserver.ThreadReadSnapshot{
		Thread: model.Thread{
			ID:          "thread-terminal",
			Title:       "Terminal correction",
			ProjectName: "Codex",
			Status:      "idle",
		},
		LatestTurnID:     "turn-terminal",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Done.",
		LatestFinalFP:    "final-fp",
	}

	if !snapshotHasPassiveChange(previous, current) {
		t.Fatal("snapshotHasPassiveChange = false, want final correction after interrupted terminal state")
	}
}

func TestSnapshotHasPassiveChangeIgnoresRepeatedTerminalSnapshot(t *testing.T) {
	t.Parallel()

	current := appserver.ThreadReadSnapshot{
		Thread: model.Thread{
			ID:          "thread-terminal-repeat",
			Title:       "Terminal repeat",
			ProjectName: "Codex",
			Status:      "idle",
		},
		LatestTurnID:     "turn-terminal-repeat",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Done.",
		LatestFinalFP:    "final-fp-repeat",
	}
	previous := appserver.CompactSnapshot(nil, current, time.Now().UTC())

	if snapshotHasPassiveChange(&previous, &current) {
		t.Fatal("snapshotHasPassiveChange = true, want repeated terminal snapshot ignored")
	}
}

func TestChatTurnLifecycleLogsThreadResumeFailure(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	thread := model.Thread{ID: "diag-resume-fail", Title: "Diagnostics", CWD: "/Users/example/project", Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{threadResumeErr: errors.New("resume failed")}
	service.live = stub
	service.liveConnected = true

	_, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "hello", "", model.PanelSourceFeishuInput)
	if err == nil {
		t.Fatal("sendInputToThreadTurn succeeded, want resume failure")
	}
	got := logs.String()
	requireLogContains(t, got, `"method":"ThreadResume"`)
	requireLogContains(t, got, `"outcome":"error"`)
	requireLogContains(t, got, `"error":"resume failed"`)
}

func TestChatTurnLifecycleLogsTurnStartFailure(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	thread := model.Thread{ID: "diag-turn-start-fail", Title: "Diagnostics", CWD: "/Users/example/project", Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{turnStartErr: errors.New("start failed")}
	service.live = stub
	service.liveConnected = true

	_, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "hello", "", model.PanelSourceFeishuInput)
	if err == nil {
		t.Fatal("sendInputToThreadTurn succeeded, want turn start failure")
	}
	got := logs.String()
	requireLogContains(t, got, `"method":"ThreadResume"`)
	requireLogContains(t, got, `"method":"TurnStart"`)
	requireLogContains(t, got, `"outcome":"error"`)
	requireLogContains(t, got, `"error":"start failed"`)
}

func TestChatTurnLifecycleLogsRefreshFailuresAroundStart(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	thread := model.Thread{ID: "diag-refresh-fail", Title: "Diagnostics", CWD: "/Users/example/project", Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{threadReadErr: errors.New("thread read failed")}
	service.live = stub
	service.liveConnected = true

	response, err := service.sendInputToThreadTurnFromSource(ctx, 123456789, 0, thread.ID, "", "hello", "", model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("sendInputToThreadTurn failed: %v", err)
	}
	if response == nil || response.TurnID != "started-turn" {
		t.Fatalf("response = %#v, want started-turn despite refresh failures", response)
	}
	got := logs.String()
	requireLogContains(t, got, `"operation":"refresh_thread_before_start"`)
	requireLogContains(t, got, `"operation":"refresh_thread_after_start"`)
	requireLogContains(t, got, `"event":"thread_refresh_failed"`)
	requireLogContains(t, got, `"method":"TurnStart"`)
}

func TestLiveEventLoopExitRecordsRepairReason(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	ch := make(chan appserver.Event)
	close(ch)
	live := &stubSession{}
	service.live = live
	service.liveEvents = ch
	service.liveConnected = true

	service.liveEventLoop(ctx, live, ch, 0)

	value, err := service.store.GetState(ctx, "repair.last_reason")
	if err != nil {
		t.Fatalf("GetState(repair.last_reason) failed: %v", err)
	}
	if value != "live_event_loop_closed" {
		t.Fatalf("repair.last_reason = %q, want live_event_loop_closed", value)
	}
	got := logs.String()
	requireLogContains(t, got, `"event":"appserver_live_event_loop_closed"`)
	requireLogContains(t, got, `"event":"repair_requested"`)
}

func TestEnsureSessionsSuppressesDuplicateConcurrentStarts(t *testing.T) {
	service := newTestService(t)
	service.cfg.RequestTimeout = 2 * time.Second
	live := newStartCountingSession()
	poll := newStartCountingSession()
	service.live = live
	service.poll = poll

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				service.ensureSessions(ctx)
				return
			}
			service.reconcileSessions(ctx)
		}(i)
	}

	live.waitStarted(t, "live")
	live.release()
	poll.waitStarted(t, "poll")
	poll.release()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent ensure/reconcile did not finish")
	}
	if got := live.StartCalls(); got != 1 {
		t.Fatalf("live Start calls = %d, want 1", got)
	}
	if got := poll.StartCalls(); got != 1 {
		t.Fatalf("poll Start calls = %d, want 1", got)
	}
}

func TestEnsureSessionsFallsBackFromProxyToStdioAfterStartFailure(t *testing.T) {
	service := newTestService(t)
	service.cfg.AppServerListen = "proxy"
	service.appServerListen = "proxy"
	logs := captureServiceLogs(service)
	proxyLive := &startErrorSession{err: errors.New("EOF")}
	proxyPoll := &startErrorSession{err: errors.New("EOF")}
	stdioLive := &stubSession{threadListResult: map[string]any{"threads": []any{}}}
	stdioPoll := &stubSession{}
	liveFactoryCalls := 0
	pollFactoryCalls := 0
	service.liveFactory = func() Session {
		liveFactoryCalls++
		if liveFactoryCalls == 1 {
			return proxyLive
		}
		return stdioLive
	}
	service.pollFactory = func() Session {
		pollFactoryCalls++
		if pollFactoryCalls == 1 {
			return proxyPoll
		}
		return stdioPoll
	}
	service.live = service.liveFactory()
	service.poll = service.pollFactory()

	service.ensureSessions(context.Background())

	if proxyLive.StartCalls() != 1 {
		t.Fatalf("proxy live Start calls = %d, want 1", proxyLive.StartCalls())
	}
	if proxyPoll.StartCalls() != 0 {
		t.Fatalf("proxy poll Start calls = %d, want 0 because live fallback switches both sessions first", proxyPoll.StartCalls())
	}
	if liveFactoryCalls != 2 || pollFactoryCalls != 2 {
		t.Fatalf("factory calls live=%d poll=%d, want 2/2", liveFactoryCalls, pollFactoryCalls)
	}
	if !service.liveConnected || !service.pollConnected {
		t.Fatalf("connected live=%t poll=%t, want both true after stdio fallback", service.liveConnected, service.pollConnected)
	}
	if service.appServerListen != "stdio://" {
		t.Fatalf("appServerListen = %q, want stdio://", service.appServerListen)
	}
	if service.cfg.AppServerListen != "proxy" {
		t.Fatalf("configured AppServerListen = %q, want original proxy config preserved", service.cfg.AppServerListen)
	}
	got := logs.String()
	requireLogContains(t, got, `"event":"appserver_proxy_fallback_to_stdio"`)
	requireLogContains(t, got, `"event":"appserver_session_started"`)
}

func TestStaleLiveEventLoopDoesNotClearNewLiveState(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	oldLive := &stubSession{}
	oldEvents := make(chan appserver.Event)
	newLive := &stubSession{}
	newEvents := make(chan appserver.Event)

	service.mu.Lock()
	service.live = oldLive
	service.liveEvents = oldEvents
	service.liveConnected = true
	service.liveGeneration = 1
	service.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.liveEventLoop(ctx, oldLive, oldEvents, 1)
	}()

	service.mu.Lock()
	service.live = newLive
	service.liveEvents = newEvents
	service.liveConnected = true
	service.liveGeneration = 2
	service.mu.Unlock()
	close(oldEvents)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old live event loop did not exit")
	}
	service.mu.RLock()
	currentLive := service.live
	currentEvents := service.liveEvents
	currentGeneration := service.liveGeneration
	liveConnected := service.liveConnected
	service.mu.RUnlock()
	if !liveConnected || currentLive != newLive || currentEvents != newEvents || currentGeneration != 2 {
		t.Fatalf("new live state was disturbed: connected=%t live=%p events_match=%t generation=%d", liveConnected, currentLive, currentEvents == newEvents, currentGeneration)
	}
	value, err := service.store.GetState(ctx, "control.repair_request")
	if err != nil {
		t.Fatalf("GetState(control.repair_request) failed: %v", err)
	}
	if strings.TrimSpace(value) != "" {
		t.Fatalf("repair request = %q, want empty for stale loop", value)
	}
	requireLogContains(t, logs.String(), `"event":"appserver_live_event_loop_stale"`)
}

func TestTransportErrorDiagnosticSanitizesPrivateFields(t *testing.T) {
	service := newTestService(t)
	ctx := context.Background()
	logs := captureServiceLogs(service)
	stub := &stubSession{stderrTail: []string{
		"token=supersecret12345 in /Users/example/private/session.sock",
	}}

	service.handleLiveEvent(ctx, stub, appserver.Event{
		Channel: "transport_error",
		Params: map[string]any{
			"error": "secret=abc123456789 at /Users/example/private/state.sqlite",
		},
	})

	got := logs.String()
	requireLogContains(t, got, `"event":"appserver_transport_error"`)
	requireLogContains(t, got, `redacted`)
	if strings.Contains(got, "abc123456789") || strings.Contains(got, "supersecret12345") || strings.Contains(got, ".sock") || strings.Contains(got, ".sqlite") {
		t.Fatalf("diagnostic log leaked private data: %s", got)
	}
}

func TestThreadArchivedEventNotifiesFeishuTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{ID: "thread-archived-event", Title: "Archived event", ProjectName: "Codex", UpdatedAt: time.Now().UTC().Unix(), Status: "idle"}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            123456789,
		OpenChatID:        "oc_topic",
		ThreadID:          thread.ID,
		RootMessageID:     9001,
		RootOpenMessageID: "om_root",
		FeishuThreadID:    "omt_topic",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}

	service.handleLiveEvent(ctx, &stubSession{}, appserver.Event{
		Channel: "notification",
		Method:  "thread/archived",
		Params:  map[string]any{"threadId": thread.ID},
	})

	if len(sender.messages) != 1 {
		t.Fatalf("messages = %#v, want one archive notice", sender.messages)
	}
	message := sender.messages[0]
	if message.chatID != 123456789 || message.topicID != 0 || message.options.FeishuReplyToMessageID != 9001 || !message.options.FeishuReplyInThread || message.options.FeishuCodexThreadID != thread.ID {
		t.Fatalf("message = %#v, want Feishu topic reply", message)
	}
	if !strings.Contains(message.text, "archived") && !strings.Contains(message.text, "归档") {
		t.Fatalf("message text = %q, want archive notice", message.text)
	}
	stored, err := service.store.GetThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if stored == nil || !stored.Archived || stored.Listed {
		t.Fatalf("stored thread = %#v, want archived and unlisted", stored)
	}
	service.handleLiveEvent(ctx, &stubSession{}, appserver.Event{
		Channel: "notification",
		Method:  "thread/archived",
		Params:  map[string]any{"threadId": thread.ID},
	})
	if len(sender.messages) != 1 {
		t.Fatalf("messages = %#v, want duplicate archive event suppressed", sender.messages)
	}

	service.handleLiveEvent(ctx, &stubSession{}, appserver.Event{
		Channel: "notification",
		Method:  "thread/unarchived",
		Params:  map[string]any{"threadId": thread.ID},
	})
	if len(sender.messages) != 2 {
		t.Fatalf("messages = %#v, want re-enabled notice after unarchive", sender.messages)
	}
	if !strings.Contains(sender.messages[1].text, "重新启用") && !strings.Contains(sender.messages[1].text, "re-enabled") {
		t.Fatalf("message text = %q, want re-enabled notice", sender.messages[1].text)
	}
	stored, err = service.store.GetThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetThread(after unarchive) failed: %v", err)
	}
	if stored == nil || stored.Archived || !stored.Listed {
		t.Fatalf("stored thread = %#v, want visible after unarchive", stored)
	}
}

func TestThreadDeletedEventNotifiesFeishuTopic(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{ID: "thread-deleted-event", Title: "Deleted event", ProjectName: "Codex", UpdatedAt: time.Now().UTC().Unix(), Status: "idle", Raw: json.RawMessage(`{"id":"thread-deleted-event"}`)}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            123456789,
		OpenChatID:        "oc_topic",
		ThreadID:          thread.ID,
		RootMessageID:     9001,
		RootOpenMessageID: "om_root",
		FeishuThreadID:    "omt_topic",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}

	service.handleLiveEvent(ctx, &stubSession{}, appserver.Event{
		Channel: "notification",
		Method:  "thread/deleted",
		Params:  map[string]any{"threadId": thread.ID},
	})

	if len(sender.messages) != 1 {
		t.Fatalf("messages = %#v, want one delete notice", sender.messages)
	}
	if !strings.Contains(sender.messages[0].text, "deleted") && !strings.Contains(sender.messages[0].text, "删除") {
		t.Fatalf("message text = %q, want delete notice", sender.messages[0].text)
	}
	stored, err := service.store.GetThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetThread failed: %v", err)
	}
	if stored == nil || !stored.Archived || stored.Listed || !strings.EqualFold(stored.Status, "deleted") || !strings.Contains(string(stored.Raw), `"deleted":true`) {
		t.Fatalf("stored thread = %#v raw=%s, want deleted state", stored, stored.Raw)
	}

	service.handleLiveEvent(ctx, &stubSession{}, appserver.Event{
		Channel: "notification",
		Method:  "thread/unarchived",
		Params:  map[string]any{"threadId": thread.ID},
	})
	if len(sender.messages) != 2 {
		t.Fatalf("messages = %#v, want re-enabled notice after deleted state", sender.messages)
	}
	stored, err = service.store.GetThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetThread(after unarchive) failed: %v", err)
	}
	if stored == nil || stored.Archived || !stored.Listed || strings.Contains(string(stored.Raw), `"deleted":true`) {
		t.Fatalf("stored thread = %#v raw=%s, want re-enabled state", stored, stored.Raw)
	}
}

func TestDiagnosticLogsAreRateLimited(t *testing.T) {
	service := newTestService(t)
	logs := captureServiceLogs(service)

	for i := 0; i < 300; i++ {
		service.logLifecycle("looping_event", lifecycleFields{"index": i})
	}

	lineCount := strings.Count(strings.TrimSpace(logs.String()), "\n")
	if strings.TrimSpace(logs.String()) != "" {
		lineCount++
	}
	if lineCount > diagnosticEventLimit("looping_event") {
		t.Fatalf("diagnostic log lines = %d, want <= %d", lineCount, diagnosticEventLimit("looping_event"))
	}
}

func TestDiagnosticLoggerCanBeDisabled(t *testing.T) {
	service := newTestService(t)
	logs := captureServiceLogs(service)

	service.logLifecycle("enabled_event", lifecycleFields{"value": "before"})
	requireLogContains(t, logs.String(), `"event":"enabled_event"`)

	service.SetLogger(nil)
	service.logLifecycle("disabled_event", lifecycleFields{"value": "after"})
	if got := logs.String(); strings.Contains(got, `"event":"disabled_event"`) {
		t.Fatalf("disabled diagnostic log was written: %s", got)
	}
}

func TestObserverSyncResultLogsAreDebounced(t *testing.T) {
	service := newTestService(t)
	logs := captureServiceLogs(service)
	snapshot := appserver.ThreadReadSnapshot{
		Thread: model.Thread{
			ID:          "thread-observer-debounce",
			Title:       "Observer debounce",
			ProjectName: "Codex",
			Status:      "idle",
		},
		LatestTurnID:     "turn-observer-debounce",
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{Kind: model.DetailItemCommentary, Text: "Working."},
		},
	}

	for i := 0; i < 10; i++ {
		snapshot.DetailItems = append(snapshot.DetailItems, model.DetailItem{Kind: model.DetailItemTool, Text: "tool"})
		service.logObserverSyncResult("thread_read", snapshot)
	}

	got := logs.String()
	if count := strings.Count(got, `"event":"observer_sync_result"`); count != 1 {
		t.Fatalf("observer_sync_result logs = %d, want 1; logs:\n%s", count, got)
	}
	requireLogContains(t, got, `"thread_id":"thread-observer-debounce"`)
}

func TestGenericThreadReadDiagnosticsAreDebounced(t *testing.T) {
	service := newTestService(t)
	logs := captureServiceLogs(service)

	for i := 0; i < 10; i++ {
		service.logAppServerCall("ThreadRead", time.Now(), nil, &stubSession{}, lifecycleFields{
			"operation":     "thread_read",
			"thread_id":     "thread-read-debounce",
			"include_turns": true,
		})
	}

	got := logs.String()
	if count := strings.Count(got, `"event":"appserver_call"`); count != 1 {
		t.Fatalf("appserver_call logs = %d, want 1; logs:\n%s", count, got)
	}
	requireLogContains(t, got, `"method":"ThreadRead"`)
	requireLogContains(t, got, `"thread_id":"thread-read-debounce"`)
}

func TestThreadReadSkippedLogsAreDebounced(t *testing.T) {
	service := newTestService(t)
	logs := captureServiceLogs(service)

	for i := 0; i < 10; i++ {
		service.logThreadReadSkipped("thread-1", "thread_not_loaded")
	}
	service.logThreadReadSkipped("thread-2", "thread_not_loaded")

	got := logs.String()
	if count := strings.Count(got, `"event":"thread_read_skipped"`); count != 2 {
		t.Fatalf("thread_read_skipped logs = %d, want 2; logs:\n%s", count, got)
	}
	requireLogContains(t, got, `"thread_id":"thread-1"`)
	requireLogContains(t, got, `"thread_id":"thread-2"`)
	requireLogContains(t, got, `"debounce":"10m0s"`)
}

func TestReplyPlanFlagStartsPlanCollaborationMode(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	if err := service.store.SetState(ctx, codexReasoningStateKey, "medium"); err != nil {
		t.Fatalf("SetState(reasoning) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "reply-plan-thread",
		Title:       "Reply plan",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/reply --plan "+thread.ID+" sketch the plan", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/reply --plan) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want reply plan thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.collaborationMode != collaborationModePlan || got.message != "sketch the plan" {
		t.Fatalf("turn start call = %#v, want plan input", got)
	}
}

func TestReplyDefaultFlagStartsDefaultCollaborationMode(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	if err := service.store.SetState(ctx, codexReasoningStateKey, "medium"); err != nil {
		t.Fatalf("SetState(reasoning) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "reply-default-thread",
		Title:       "Reply default",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/reply --default "+thread.ID+" do the work", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/reply --default) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want reply default thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.collaborationMode != collaborationModeDefault || got.message != "do the work" {
		t.Fatalf("turn start call = %#v, want default input", got)
	}
}

func TestDefaultModeCommandStartsDefaultCollaborationMode(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "default-command-thread",
		Title:       "Default command",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/default "+thread.ID+" do the work", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/default) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want default command thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.threadID != thread.ID || got.collaborationMode != collaborationModeDefault || got.message != "do the work" {
		t.Fatalf("turn start call = %#v, want default-mode command", got)
	}
}

func TestHelpHidesDefaultModeFallback(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	response, err := service.handleCommandFromSource(context.Background(), 123456789, 0, "/help", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/help) failed: %v", err)
	}
	if response == nil {
		t.Fatal("handleCommand(/help) returned nil response")
	}
	helpText := helpResponseText(response)
	for _, hidden := range []string{"/default", "--default", "/start", "/show", "/approve", "/deny"} {
		if strings.Contains(helpText, hidden) {
			t.Fatalf("/help exposes hidden command %q:\n%s", hidden, helpText)
		}
	}
	if !strings.Contains(helpText, "/plan") {
		t.Fatalf("/help text = %q, want public plan command", helpText)
	}
	for _, visible := range []string{"/chats", "/projects", "/new", "/goal", "/setting", "/status", "/repair", "/stop"} {
		if !strings.Contains(helpText, visible) {
			t.Fatalf("/help text = %q, want visible command %s", helpText, visible)
		}
	}
	if strings.Contains(helpText, "/reply") {
		t.Fatalf("/help text = %q, want no /reply command", helpText)
	}
	if len(response.Sections) == 0 {
		t.Fatalf("/help sections are empty, want interactive command collection")
	}
	for _, descriptions := range [][]string{
		{"打开最近会话", "Open recent chats"},
		{"新建 Codex 会话", "Start a new Codex chat"},
		{"调整模型", "Adjust model"},
	} {
		if !containsAny(helpText, descriptions) {
			t.Fatalf("/help text = %q, want one of descriptions %#v", helpText, descriptions)
		}
	}
	if countInteractiveRows(response.Sections) < 7 {
		t.Fatalf("/help sections = %#v, want workspace command rows with callback buttons", response.Sections)
	}
	updateToken := callbackTokenForHelpCommand(response.Sections, "Version")
	if updateToken == "" {
		t.Fatalf("/help sections = %#v, want version check row callback", response.Sections)
	}
	if callbackTokenForHelpCommand(response.Sections, "/plan <文本>") != "" || callbackTokenForHelpCommand(response.Sections, "/goal <目标>") != "" || callbackTokenForHelpCommand(response.Sections, "/stop") != "" {
		t.Fatalf("/help sections = %#v, want topic-only commands to be non-interactive in workspace help", response.Sections)
	}
	statusToken := callbackTokenForHelpCommand(response.Sections, "/status")
	if statusToken == "" {
		t.Fatalf("/help sections = %#v, want /status row callback", response.Sections)
	}
	statusResponse, err := service.HandleCallbackFromSource(context.Background(), 123456789, 0, 42, 123456789, statusToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("/status help callback failed: %v", err)
	}
	if statusResponse == nil || !strings.Contains(helpResponseText(statusResponse), "Status") {
		t.Fatalf("/status help callback response = %#v, want status response", statusResponse)
	}
}

func TestHelpVersionCheckAndUpdateCallbacks(t *testing.T) {
	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	oldCheckForUpdate := checkForUpdate
	oldApplyUpdate := applyUpdate
	oldExitAfterAutoUpdate := exitAfterAutoUpdate
	t.Cleanup(func() {
		checkForUpdate = oldCheckForUpdate
		applyUpdate = oldApplyUpdate
		exitAfterAutoUpdate = oldExitAfterAutoUpdate
	})
	checkForUpdate = func(ctx context.Context, opts updater.Options) (updater.Result, error) {
		return updater.Result{
			CurrentVersion: opts.CurrentVersion,
			LatestVersion:  "9.9.9",
			ReleaseURL:     "https://example.test/release",
		}, nil
	}
	applyUpdate = func(ctx context.Context, opts updater.Options) (updater.Result, error) {
		return updater.Result{
			CurrentVersion: opts.CurrentVersion,
			LatestVersion:  "9.9.9",
			Updated:        true,
		}, nil
	}
	exited := false
	exitAfterAutoUpdate = func(code int) { exited = true }

	help := service.workspaceHelpCommand(context.Background())
	checkToken := callbackTokenForHelpCommand(help.Sections, "Version")
	if checkToken == "" {
		t.Fatalf("/help sections = %#v, want Version callback", help.Sections)
	}
	checked, err := service.HandleCallbackFromSource(context.Background(), 123456789, 0, 42, 123456789, checkToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(update_check) failed: %v", err)
	}
	if checked == nil || !strings.Contains(helpResponseText(checked), "9.9.9") {
		t.Fatalf("checked response = %#v, want latest version", checked)
	}
	updateToken := callbackTokenForButton(checked.Buttons, "Update now")
	if updateToken == "" {
		t.Fatalf("checked buttons = %#v, want Update button", checked.Buttons)
	}
	if len(checked.Sections) < 2 || len(checked.Sections[1].Rows) < 2 || checked.Sections[1].Rows[1].BackgroundStyle == "" {
		t.Fatalf("checked sections = %#v, want styled version rows", checked.Sections)
	}

	updated, err := service.HandleCallbackFromSource(context.Background(), 123456789, 0, 42, 123456789, updateToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(update_apply) failed: %v", err)
	}
	if updated == nil || !strings.Contains(helpResponseText(updated), "Updated") {
		t.Fatalf("updated response = %#v, want updated status", updated)
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0].text, "Update complete") {
		t.Fatalf("sender messages = %#v, want update completion message", sender.messages)
	}
	time.Sleep(2200 * time.Millisecond)
	if !exited {
		t.Fatal("exitAfterAutoUpdate was not called after update")
	}
}

func TestTopicHelpOnlyShowsChatCommands(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	response, err := service.handleCommandFromSource(ctx, 123456789, 9001, "/help", 9001, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/help) failed: %v", err)
	}
	if response == nil {
		t.Fatal("handleCommand(/help) returned nil response")
	}
	helpText := helpResponseText(response)
	for _, visible := range []string{"/plan", "/goal", "/goal clear", "/stop"} {
		if !strings.Contains(helpText, visible) {
			t.Fatalf("topic /help text = %q, want visible command %s", helpText, visible)
		}
	}
	if strings.Contains(helpText, "/reply") {
		t.Fatalf("topic /help text = %q, want no /reply command", helpText)
	}
	for _, hidden := range []string{"/chats", "/projects", "/new", "/setting", "/repair"} {
		if strings.Contains(helpText, hidden) {
			t.Fatalf("topic /help exposes workspace command %q:\n%s", hidden, helpText)
		}
	}
	if callbackTokenForHelpCommand(response.Sections, "/plan <文本>") == "" || callbackTokenForHelpCommand(response.Sections, "/goal <目标>") == "" || callbackTokenForHelpCommand(response.Sections, "/stop") == "" {
		t.Fatalf("topic /help sections = %#v, want topic command callbacks", response.Sections)
	}
	if response.Options.FeishuReplyToMessageID != 9001 || !response.Options.FeishuReplyInThread || response.DeliveryTopicID != 9001 {
		t.Fatalf("topic /help response options = %#v delivery=%d, want reply in topic 9001", response.Options, response.DeliveryTopicID)
	}
}

func helpResponseText(response *DirectResponse) string {
	var parts []string
	parts = append(parts, response.Text)
	for _, section := range response.Sections {
		parts = append(parts, section.Text)
		for _, row := range section.Rows {
			parts = append(parts, row.Title, row.Trailing, row.Button.Text)
		}
		for _, buttonRow := range section.Buttons {
			for _, button := range buttonRow {
				parts = append(parts, button.Text)
			}
		}
	}
	for _, buttonRow := range response.Buttons {
		for _, button := range buttonRow {
			parts = append(parts, button.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func countInteractiveRows(sections []model.MessageSection) int {
	count := 0
	for _, section := range sections {
		for _, row := range section.Rows {
			if row.Button.CallbackData != "" {
				count++
			}
		}
	}
	return count
}

func callbackTokenForHelpCommand(sections []model.MessageSection, title string) string {
	for _, section := range sections {
		for _, row := range section.Rows {
			if row.Title == title {
				return row.Button.CallbackData
			}
		}
	}
	return ""
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func TestStopSetsDefaultOverrideForActiveThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "stop-active-default-thread",
		Title:        "Stop active default",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "active",
		ActiveTurnID: "turn-active",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/stop "+thread.ID, 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/stop) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID || response.TurnID != thread.ActiveTurnID {
		t.Fatalf("response = %#v, want active stop response", response)
	}
	if strings.TrimSpace(response.Text) == "" {
		t.Fatalf("response = %#v, want visible /stop command text", response)
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != collaborationModeDefault {
		t.Fatalf("threadCollaborationOverride = %q, want default after /stop", got)
	}
}

func TestStopSetsDefaultOverrideForIdleThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "stop-idle-default-thread",
		Title:       "Stop idle default",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/stop "+thread.ID, 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/stop idle) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.CallbackText, "already idle") {
		t.Fatalf("response = %#v, want idle stop response", response)
	}
	if !strings.Contains(response.Text, "already idle") {
		t.Fatalf("response = %#v, want visible idle /stop command text", response)
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != collaborationModeDefault {
		t.Fatalf("threadCollaborationOverride = %q, want default after idle /stop", got)
	}
}

func TestStopTreatsCompletedThreadWithStaleActiveTurnAsIdle(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "stop-completed-stale-active-thread",
		Title:        "Stop completed stale active",
		ProjectName:  "Codex",
		CWD:          "/Users/example/project",
		Status:       "completed",
		ActiveTurnID: "stale-completed-turn",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/stop "+thread.ID, 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/stop stale completed) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.CallbackText, "already idle") {
		t.Fatalf("response = %#v, want idle stop response", response)
	}
	if !strings.Contains(response.Text, "already idle") {
		t.Fatalf("response = %#v, want visible idle /stop command text", response)
	}
	if len(stub.turnInterruptCalls) != 0 {
		t.Fatalf("turnInterruptCalls = %#v, want no interrupt for completed thread", stub.turnInterruptCalls)
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != collaborationModeDefault {
		t.Fatalf("threadCollaborationOverride = %q, want default after stale completed /stop", got)
	}
}

func TestPlanModeCommandCanRouteByReply(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "reply-routed-plan-thread",
		Title:       "Reply routed plan",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 812,
		ThreadID:  thread.ID,
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/plan plan this reply-routed task", 812, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/plan) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want routed thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	got := stub.turnStartCalls[0]
	if got.collaborationMode != collaborationModePlan || got.message != "plan this reply-routed task" {
		t.Fatalf("turn start call = %#v, want reply-routed plan text", got)
	}
}

func TestContextCardBoundThreadIncludesFullThreadID(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "full-context-thread-id",
		Title:       "Context title",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	text, err := service.contextCard(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("contextCard failed: %v", err)
	}
	for _, want := range []string{
		"Mode: Unbound",
		"Use /chats or /projects to choose a thread.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("context card missing %q in:\n%s", want, text)
		}
	}
}

func TestSummaryPanelGetThreadIDButtonSendsCopyableIDs(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "summary-thread-full-id",
		Title:       "Summary",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "active",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:             thread,
		LatestTurnID:       "summary-turn-full-id",
		LatestTurnStatus:   "inProgress",
		LatestProgressText: "Working",
		LatestProgressFP:   "progress-fp",
	}

	_, buttons, _ := service.renderSummaryPanel(ctx, thread, snapshot, nil)
	token := callbackTokenForButton(buttons, "Get thread id")
	if token == "" {
		t.Fatalf("Get thread id button not found in %#v", buttons)
	}

	response, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(get_thread_id) failed: %v", err)
	}
	if response == nil || response.Text != "Thread ID:\nsummary-thread-full-id\n\nTurn ID:\nsummary-turn-full-id" {
		t.Fatalf("response = %#v, want copyable thread/turn ids", response)
	}
	if response.ThreadID != thread.ID || response.TurnID != "summary-turn-full-id" {
		t.Fatalf("response route = thread %q turn %q, want full ids", response.ThreadID, response.TurnID)
	}
}

func TestRunningSummaryPanelDoesNotShowSteerButton(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "summary-thread-no-steer",
		Title:       "Summary no steer",
		ProjectName: "Codex",
		Status:      "active",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:             thread,
		LatestTurnID:       "summary-turn-no-steer",
		LatestTurnStatus:   "inProgress",
		LatestProgressText: "Working",
		LatestProgressFP:   "progress-fp",
	}

	_, buttons, _ := service.renderSummaryPanel(ctx, thread, snapshot, nil)
	if token := callbackTokenForButton(buttons, "Steer"); token != "" {
		t.Fatalf("summary buttons = %#v, want no Steer button", buttons)
	}
	if token := callbackTokenForButton(buttons, "引导"); token != "" {
		t.Fatalf("summary buttons = %#v, want no 引导 button", buttons)
	}
	if token := callbackTokenForButton(buttons, "Stop"); token == "" {
		t.Fatalf("summary buttons = %#v, want Stop button", buttons)
	}
	if token := callbackTokenForButton(buttons, "Show context"); token != "" {
		t.Fatalf("summary buttons = %#v, want no Show context button", buttons)
	}
}

func TestFinalSummaryPanelHasGetThreadIDButton(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "final-thread-full-id",
		Title:       "Final",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "final-turn-full-id",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Done.",
		LatestFinalFP:    "final-fp",
	}

	_, buttons, _ := service.renderSummaryPanel(ctx, thread, snapshot, nil)
	if callbackTokenForButton(buttons, "Refresh") == "" {
		t.Fatalf("final summary buttons = %#v, want Refresh", buttons)
	}
	if callbackTokenForButton(buttons, "Stop") != "" {
		t.Fatalf("final summary buttons = %#v, want no running buttons", buttons)
	}
}

func TestFinalCardGetThreadIDButtonSendsCopyableIDs(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "final-card-thread-full-id",
		Title:       "Final card",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "final-card-turn-full-id",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Done.",
		LatestFinalFP:    "final-card-fp",
	}

	_, buttons, _ := service.renderFinalCard(ctx, 42, thread, snapshot)
	token := callbackTokenForButton(buttons, "Get thread id")
	if token == "" {
		t.Fatalf("Get thread id button not found in final card buttons %#v", buttons)
	}

	response, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 42, 123456789, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(final get_thread_id) failed: %v", err)
	}
	if response == nil || response.Text != "Thread ID:\nfinal-card-thread-full-id\n\nTurn ID:\nfinal-card-turn-full-id" {
		t.Fatalf("response = %#v, want copyable final card ids", response)
	}
}

func TestFinalCardShowsRunDuration(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "final-duration-thread",
		Title:       "Final duration",
		ProjectName: "Codex",
		Status:      "idle",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:              thread,
		LatestTurnID:        "final-duration-turn",
		LatestTurnStatus:    "completed",
		LatestTurnStartedAt: "2026-05-02T12:00:00Z",
		LatestTurnUpdatedAt: "2026-05-02T12:01:12Z",
		LatestFinalText:     "Done.",
		LatestFinalFP:       "final-duration-fp",
	}

	message, _, _ := service.renderFinalCard(ctx, 42, thread, snapshot)
	if !strings.Contains(message.Text, "Done.") {
		t.Fatalf("final card = %q, want final answer", message.Text)
	}
	if !strings.Contains(message.Text, "Run duration: 1m 12s") {
		t.Fatalf("final card = %q, want run duration footer", message.Text)
	}
}

func TestPlanFinalCardShowsTurnOffPlanButton(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "final-plan-thread",
		Title:       "Final plan",
		ProjectName: "Codex",
		Status:      "idle",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "final-plan-turn",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Plan mode final.",
		LatestFinalFP:    "final-plan-fp",
		DetailItems: []model.DetailItem{
			{ID: "plan-1", Kind: model.DetailItemPlan, Text: "Plan text.", CommentaryIndex: 1},
			{ID: "final-1", Kind: model.DetailItemFinal, Text: "Plan mode final.", CommentaryIndex: 1},
		},
	}

	_, buttons, _ := service.renderFinalCard(ctx, 42, thread, snapshot)
	if token := callbackTokenForButton(buttons, "Turn off Plan"); token == "" {
		t.Fatalf("Turn off Plan button not found in final card buttons %#v", buttons)
	}
}

func TestPlanFinalCardShowsTurnOffPlanButtonFromLocalMarker(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "final-plan-marker-thread",
		Title:       "Final plan marker",
		ProjectName: "Codex",
		Status:      "idle",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "final-plan-marker-turn",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Plan mode final.",
		LatestFinalFP:    "final-plan-marker-fp",
		DetailItems: []model.DetailItem{
			{ID: "final-1", Kind: model.DetailItemFinal, Text: "Plan mode final."},
		},
	}
	if err := service.setThreadCollaborationMarker(ctx, thread.ID, snapshot.LatestTurnID, collaborationModePlan); err != nil {
		t.Fatalf("setThreadCollaborationMarker failed: %v", err)
	}

	_, buttons, _ := service.renderFinalCard(ctx, 42, thread, snapshot)
	if token := callbackTokenForButton(buttons, "Turn off Plan"); token == "" {
		t.Fatalf("Turn off Plan button not found in marker-based final card buttons %#v", buttons)
	}
}

func TestNormalFinalCardDoesNotShowTurnOffPlanButton(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "final-normal-thread",
		Title:       "Final normal",
		ProjectName: "Codex",
		Status:      "idle",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "final-normal-turn",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Done.",
		LatestFinalFP:    "final-normal-fp",
		DetailItems: []model.DetailItem{
			{ID: "final-1", Kind: model.DetailItemFinal, Text: "Done."},
		},
	}

	_, buttons, _ := service.renderFinalCard(ctx, 42, thread, snapshot)
	if token := callbackTokenForButton(buttons, "Turn off Plan"); token != "" {
		t.Fatalf("Turn off Plan button token = %q, want absent in non-plan final buttons %#v", token, buttons)
	}
}

func TestReplyCommandKeepsDefaultCollaborationMode(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	if err := service.store.SetState(ctx, codexReasoningStateKey, "high"); err != nil {
		t.Fatalf("SetState(reasoning) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "plain-reply-thread",
		Title:       "Plain reply",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/reply "+thread.ID+" do the work", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/reply) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want reply thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.collaborationMode != "" {
		t.Fatalf("collaborationMode = %q, want empty default turn", got.collaborationMode)
	}
}

func TestReplyCommandConsumesDefaultOverrideOnce(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "plain-reply-default-override-thread",
		Title:       "Plain reply default override",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.setThreadCollaborationDefaultOverride(ctx, thread.ID); err != nil {
		t.Fatalf("setThreadCollaborationDefaultOverride failed: %v", err)
	}
	if err := service.setThreadCollaborationMarker(ctx, thread.ID, "old-plan-turn", collaborationModePlan); err != nil {
		t.Fatalf("setThreadCollaborationMarker failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/reply "+thread.ID+" do the work", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/reply) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want reply thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.collaborationMode != collaborationModeDefault {
		t.Fatalf("collaborationMode = %q, want default override", got.collaborationMode)
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != "" {
		t.Fatalf("threadCollaborationOverride = %q, want cleared after successful start", got)
	}
	if got := service.threadCollaborationMarker(ctx, thread.ID, "started-turn"); got != "" {
		t.Fatalf("threadCollaborationMarker = %q, want cleared after default override start", got)
	}
}

func TestDefaultOverrideSurvivesTurnStartFailure(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "default-override-failure-thread",
		Title:       "Default override failure",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.setThreadCollaborationDefaultOverride(ctx, thread.ID); err != nil {
		t.Fatalf("setThreadCollaborationDefaultOverride failed: %v", err)
	}
	stub := &stubSession{turnStartErr: errors.New("turn start failed")}
	service.live = stub
	service.liveConnected = true

	_, err := service.handleCommandFromSource(ctx, 123456789, 0, "/reply "+thread.ID+" do the work", 0, model.PanelSourceFeishuInput)
	if err == nil {
		t.Fatal("handleCommand(/reply) succeeded, want turn start error")
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != collaborationModeDefault {
		t.Fatalf("threadCollaborationOverride = %q, want default retained after failed start", got)
	}
}

func TestPlanCommandClearsStaleDefaultOverride(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SetState(ctx, codexModelStateKey, "gpt-test"); err != nil {
		t.Fatalf("SetState(model) failed: %v", err)
	}
	thread := model.Thread{
		ID:          "plan-clears-default-override-thread",
		Title:       "Plan clears default override",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.setThreadCollaborationDefaultOverride(ctx, thread.ID); err != nil {
		t.Fatalf("setThreadCollaborationDefaultOverride failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handleCommandFromSource(ctx, 123456789, 0, "/plan "+thread.ID+" propose options", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/plan) failed: %v", err)
	}
	if response == nil || response.ThreadID != thread.ID {
		t.Fatalf("response = %#v, want plan thread", response)
	}
	if len(stub.turnStartCalls) != 1 {
		t.Fatalf("turnStartCalls = %#v, want one start", stub.turnStartCalls)
	}
	if got := stub.turnStartCalls[0]; got.collaborationMode != collaborationModePlan {
		t.Fatalf("collaborationMode = %q, want plan", got.collaborationMode)
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != "" {
		t.Fatalf("threadCollaborationOverride = %q, want cleared after explicit plan start", got)
	}
	if got := service.threadCollaborationMarker(ctx, thread.ID, "started-turn"); got != collaborationModePlan {
		t.Fatalf("threadCollaborationMarker = %q, want plan after explicit plan start", got)
	}
}

func TestSettingsFormPersistsSelectedValues(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	stub := &stubSession{models: []appserver.ModelOption{
		{ID: "gpt-default", IsDefault: true, SupportedReasoningEffort: []string{"low", "medium", "high"}},
		{ID: "gpt-menu", SupportedReasoningEffort: []string{"minimal", "low"}},
	}}
	service.live = stub
	service.liveConnected = true

	settings, err := service.handleCommandFromSource(ctx, 123456789, 0, "/setting", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/setting) failed: %v", err)
	}
	if settings.SettingsForm == nil {
		t.Fatalf("settings response = %#v, want settings form", settings)
	}
	if !selectOptionsContain(settings.SettingsForm.ModelOptions, "gpt-menu") {
		t.Fatalf("model options = %#v, want gpt-menu", settings.SettingsForm.ModelOptions)
	}
	if !selectOptionsContain(settings.SettingsForm.ReasoningOptions, "high") {
		t.Fatalf("reasoning options = %#v, want default model reasoning options", settings.SettingsForm.ReasoningOptions)
	}
	callbackResponse, err := service.HandleCallbackPayloadFromSource(ctx, 123456789, 0, 900, 123456789, settings.SettingsForm.SubmitToken, map[string]any{
		"form_value": map[string]any{
			"model":     "gpt-menu",
			"reasoning": "minimal",
			"language":  botLanguageEnglish,
		},
	}, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(settings submit) failed: %v", err)
	}
	if callbackResponse == nil || !callbackResponse.SilentCallback || callbackResponse.CallbackText != "Settings applied." {
		t.Fatalf("callback response = %#v, want silent applied callback", callbackResponse)
	}
	if len(sender.edits) != 1 || !strings.Contains(sender.edits[0].text, "Model: gpt-menu") || !strings.Contains(sender.edits[0].text, "Reasoning effort: minimal") || !strings.Contains(sender.edits[0].text, "Language: English") {
		t.Fatalf("edits = %#v, want applied settings summary", sender.edits)
	}
	modelValue, err := service.store.GetState(ctx, codexModelStateKey)
	if err != nil {
		t.Fatalf("GetState(model) failed: %v", err)
	}
	reasoningValue, err := service.store.GetState(ctx, codexReasoningStateKey)
	if err != nil {
		t.Fatalf("GetState(reasoning) failed: %v", err)
	}
	languageValue, err := service.store.GetState(ctx, botLanguageStateKey)
	if err != nil {
		t.Fatalf("GetState(language) failed: %v", err)
	}
	if modelValue != "gpt-menu" || reasoningValue != "minimal" || languageValue != botLanguageEnglish {
		t.Fatalf("stored values model=%q reasoning=%q language=%q, want gpt-menu/minimal/en", modelValue, reasoningValue, languageValue)
	}
	workspace, err := service.handleCommandFromSource(ctx, 123456789, 0, "/start", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/start) failed: %v", err)
	}
	if workspace == nil || !strings.Contains(workspace.Text, "Codex Workspace") {
		t.Fatalf("workspace = %#v, want English text after language selection", workspace)
	}
}

func TestSettingsFormPrefillsCodexConfig(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	stub := &stubSession{
		models: []appserver.ModelOption{
			{ID: "gpt-default", IsDefault: true, SupportedReasoningEffort: []string{"low", "medium", "high"}},
			{ID: "gpt-codex", SupportedReasoningEffort: []string{"minimal", "high"}},
		},
		configReadResult: map[string]any{
			"data": map[string]any{
				"config": map[string]any{
					"model":            "gpt-codex",
					"reasoning_effort": "high",
				},
			},
		},
	}
	service.live = stub
	service.liveConnected = true

	settings, err := service.handleCommandFromSource(ctx, 123456789, 0, "/setting", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handleCommand(/setting) failed: %v", err)
	}
	if settings.SettingsForm == nil {
		t.Fatalf("settings response = %#v, want settings form", settings)
	}
	if settings.SettingsForm.ModelValue != "gpt-codex" || settings.SettingsForm.ReasoningValue != "high" {
		t.Fatalf("settings form values model=%q reasoning=%q, want gpt-codex/high", settings.SettingsForm.ModelValue, settings.SettingsForm.ReasoningValue)
	}
	if !strings.Contains(settings.Text, "Model: gpt-codex") || !strings.Contains(settings.Text, "Reasoning effort: high") {
		t.Fatalf("settings text = %q, want codex config summary", settings.Text)
	}
	if !selectOptionsContain(settings.SettingsForm.ModelOptions, "gpt-codex") {
		t.Fatalf("model options = %#v, want gpt-codex", settings.SettingsForm.ModelOptions)
	}
	if stub.configReadCalls != 1 {
		t.Fatalf("configReadCalls = %d, want 1", stub.configReadCalls)
	}
}

func TestSettingsCallbacksMissingValueUseAuto(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()

	modelResponse, err := service.setCodexModel(ctx, 123456789, 0, 0, nil)
	if err != nil {
		t.Fatalf("setCodexModel(nil payload) failed: %v", err)
	}
	if modelResponse == nil || strings.Contains(modelResponse.Text, "<nil>") {
		t.Fatalf("model response = %#v, want no <nil>", modelResponse)
	}
	modelValue, err := service.store.GetState(ctx, codexModelStateKey)
	if err != nil {
		t.Fatalf("GetState(model) failed: %v", err)
	}
	if modelValue != "" {
		t.Fatalf("stored model = %q, want Auto/blank", modelValue)
	}

	reasoningResponse, err := service.setCodexReasoningEffort(ctx, 123456789, 0, 0, nil)
	if err != nil {
		t.Fatalf("setCodexReasoningEffort(nil payload) failed: %v", err)
	}
	if reasoningResponse == nil || strings.Contains(reasoningResponse.Text, "<nil>") {
		t.Fatalf("reasoning response = %#v, want no <nil>", reasoningResponse)
	}
	reasoningValue, err := service.store.GetState(ctx, codexReasoningStateKey)
	if err != nil {
		t.Fatalf("GetState(reasoning) failed: %v", err)
	}
	if reasoningValue != "" {
		t.Fatalf("stored reasoning = %q, want Auto/blank", reasoningValue)
	}
}

func TestAnswerChoiceMissingTextDoesNotSendNil(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.answerChoice(ctx, 123456789, 0, &model.CallbackRoute{
		ThreadID:    "thread-missing-text",
		TurnID:      "turn-missing-text",
		PayloadJSON: `{}`,
	}, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("answerChoice(missing text) failed: %v", err)
	}
	if response == nil || response.CallbackText != "Answer option is empty." {
		t.Fatalf("response = %#v, want empty answer callback", response)
	}
	if len(stub.turnSteerCalls) != 0 || len(stub.turnStartCalls) != 0 || len(stub.respondRequestCalls) != 0 {
		t.Fatalf("unexpected calls for missing answer text: steer=%#v start=%#v respond=%#v", stub.turnSteerCalls, stub.turnStartCalls, stub.respondRequestCalls)
	}
}

func TestUserInputResponsePayloadSkipsNilQuestionID(t *testing.T) {
	t.Parallel()

	response := userInputResponsePayload(`{"questions":[{"id":"<nil>","question":"Pick one."},{"question":"Missing id."}]}`, "Yes")
	if _, ok := response["answers"]; ok {
		t.Fatalf("response = %#v, want fallback text payload without <nil> answer id", response)
	}
	if response["text"] != "Yes" || response["value"] != "Yes" || response["response"] != "Yes" || response["input"] != "Yes" {
		t.Fatalf("response = %#v, want fallback text/value/response/input", response)
	}
}

func TestPlainReplyToRealPlanPromptUsesServerRequest(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	if err := service.store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:   "request-plan-reply",
		ThreadID:    "real-plan-thread",
		TurnID:      "real-plan-turn",
		PromptKind:  "user_input",
		Question:    "Need input.",
		PayloadJSON: `{"questions":[{"id":"choice","question":"Need input?","options":[{"label":"The answer","description":"Use answer."}]}]}`,
		Status:      "pending",
		UpdatedAt:   model.NowString(),
	}); err != nil {
		t.Fatalf("SavePendingApproval failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 779,
		ThreadID:  "real-plan-thread",
		TurnID:    "real-plan-turn",
		EventID:   "plan_request:request-plan-reply",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	stub := &stubSession{}
	service.live = stub
	service.liveConnected = true

	response, err := service.handlePlainTextFromSource(ctx, 123456789, 0, "The answer", 779, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("handlePlainText failed: %v", err)
	}
	if response == nil || response.ThreadID != "real-plan-thread" || response.TurnID != "real-plan-turn" {
		t.Fatalf("response = %#v, want real plan thread/turn", response)
	}
	if len(stub.respondRequestCalls) != 1 {
		t.Fatalf("respondRequestCalls = %#v, want one server request response", stub.respondRequestCalls)
	}
	got := stub.respondRequestCalls[0]
	answers, _ := got.result["answers"].(map[string]any)
	choice, _ := answers["choice"].(map[string]any)
	values, _ := choice["answers"].([]string)
	if got.requestID != "request-plan-reply" || len(values) != 1 || values[0] != "The answer" {
		t.Fatalf("respond request call = %#v, want request-plan-reply schema answers", got)
	}
	if len(stub.turnSteerCalls) != 0 || len(stub.turnStartCalls) != 0 {
		t.Fatalf("unexpected turn calls: steer=%#v start=%#v", stub.turnSteerCalls, stub.turnStartCalls)
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()

	root := t.TempDir()
	cfg := config.Config{
		Paths: config.Paths{
			Home:    root,
			DataDir: filepath.Join(root, "data"),
			LogDir:  filepath.Join(root, "logs"),
			DBPath:  filepath.Join(root, "data", "state.sqlite"),
		},
		DefaultCWD: `C:\Users\you\Projects\Codex`,
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatalf("daemon.New failed: %v", err)
	}
	if err := service.store.SetState(context.Background(), botLanguageStateKey, botLanguageEnglish); err != nil {
		t.Fatalf("SetState(language) failed: %v", err)
	}
	t.Cleanup(func() {
		_ = service.Close()
	})
	return service
}

func captureServiceLogs(service *Service) *bytes.Buffer {
	var logs bytes.Buffer
	service.SetLogger(log.New(&logs, "", 0))
	return &logs
}

func requireLogContains(t *testing.T, logs, needle string) {
	t.Helper()
	if !strings.Contains(logs, needle) {
		t.Fatalf("diagnostic log missing %q in:\n%s", needle, logs)
	}
}

func diagnosticThreadReadPayload(thread model.Thread, turnID, status string) map[string]any {
	return map[string]any{
		"thread": map[string]any{
			"id":     thread.ID,
			"title":  thread.Title,
			"cwd":    thread.CWD,
			"status": thread.Status,
			"turns": []any{
				map[string]any{
					"id":     turnID,
					"status": status,
					"items": []any{
						map[string]any{
							"id":      "user-item",
							"type":    "userMessage",
							"content": []any{map[string]any{"text": "hello"}},
						},
					},
				},
			},
		},
	}
}

func diagnosticThreadReadPayloadWithTool(thread model.Thread, turnID, status string) map[string]any {
	payload := diagnosticThreadReadPayload(thread, turnID, status)
	threadPayload := payload["thread"].(map[string]any)
	turns := threadPayload["turns"].([]any)
	turn := turns[0].(map[string]any)
	turn["items"] = []any{
		map[string]any{
			"id":      "user-item",
			"type":    "userMessage",
			"content": []any{map[string]any{"text": "hello"}},
		},
		map[string]any{
			"id":               "cmd-slow",
			"type":             "commandExecution",
			"command":          "sleep 20; printf 'slow-command-done\\n'",
			"status":           "completed",
			"aggregatedOutput": "slow-command-done\n",
		},
	}
	return payload
}

func diagnosticThreadReadPayloadWithCommentary(thread model.Thread, turnID, status, text string) map[string]any {
	payload := diagnosticThreadReadPayload(thread, turnID, status)
	threadPayload := payload["thread"].(map[string]any)
	turns := threadPayload["turns"].([]any)
	turn := turns[0].(map[string]any)
	items := turn["items"].([]any)
	turn["items"] = append(items, map[string]any{
		"id":    "commentary-item",
		"type":  "agentMessage",
		"phase": "commentary",
		"text":  text,
	})
	return payload
}

func diagnosticThreadReadPayloadWithFinal(thread model.Thread, turnID, status, finalText string) map[string]any {
	payload := diagnosticThreadReadPayloadWithTool(thread, turnID, status)
	threadPayload := payload["thread"].(map[string]any)
	turns := threadPayload["turns"].([]any)
	turn := turns[0].(map[string]any)
	items := turn["items"].([]any)
	turn["items"] = append(items, map[string]any{
		"id":    "final-item",
		"type":  "agentMessage",
		"phase": "final_answer",
		"text":  finalText,
	})
	return payload
}

func callbackTokenForButton(rows [][]model.ButtonSpec, label string) string {
	for _, row := range rows {
		for _, button := range row {
			if strings.Contains(button.Text, label) {
				return button.CallbackData
			}
		}
	}
	return ""
}

func selectOptionsContain(options []model.SelectOption, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}

func sectionTitles(sections []model.MessageSection) []string {
	out := make([]string, 0, len(sections))
	for _, section := range sections {
		out = append(out, section.Text)
	}
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func assertSectionRows(t *testing.T, section model.MessageSection, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, row := range section.Rows {
		got[row.Title] = row.Trailing
	}
	for title, trailing := range want {
		if got[title] != trailing {
			t.Fatalf("section %q row %q = %q, want %q; rows = %#v", section.Text, title, got[title], trailing, section.Rows)
		}
	}
}

func countButtonsContaining(rows [][]model.ButtonSpec, label string) int {
	count := 0
	for _, row := range rows {
		for _, button := range row {
			if strings.Contains(button.Text, label) {
				count++
			}
		}
	}
	return count
}

func requireTextOrder(t *testing.T, text, before, after string) {
	t.Helper()
	beforeIndex := strings.Index(text, before)
	afterIndex := strings.Index(text, after)
	if beforeIndex < 0 || afterIndex < 0 || beforeIndex >= afterIndex {
		t.Fatalf("text order = before %q at %d, after %q at %d in:\n%s", before, beforeIndex, after, afterIndex, text)
	}
}

func openOnlyProjectMenu(t *testing.T, service *Service, ctx context.Context) *DirectResponse {
	t.Helper()
	menu, err := service.projectMenu(ctx, map[string]any{"cwd": "/Users/example/project"})
	if err != nil {
		t.Fatalf("projectMenu failed: %v", err)
	}
	if menu == nil {
		t.Fatal("projectMenu returned nil response")
	}
	return menu
}

type startCountingSession struct {
	stubSession
	mu       sync.Mutex
	started  chan struct{}
	unblock  chan struct{}
	once     sync.Once
	starts   int
	signaled bool
}

func newStartCountingSession() *startCountingSession {
	return &startCountingSession{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
}

func (s *startCountingSession) Start(ctx context.Context) error {
	s.mu.Lock()
	s.starts++
	if !s.signaled {
		close(s.started)
		s.signaled = true
	}
	s.mu.Unlock()
	select {
	case <-s.unblock:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *startCountingSession) ThreadList(ctx context.Context, limit int, cursor string) (map[string]any, error) {
	return map[string]any{}, nil
}

func (s *startCountingSession) waitStarted(t *testing.T, role string) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(time.Second):
		t.Fatalf("%s session did not start", role)
	}
}

func (s *startCountingSession) release() {
	s.once.Do(func() {
		close(s.unblock)
	})
}

func (s *startCountingSession) StartCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

type startErrorSession struct {
	stubSession
	mu     sync.Mutex
	err    error
	starts int
}

func (s *startErrorSession) Start(ctx context.Context) error {
	s.mu.Lock()
	s.starts++
	s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return nil
}

func (s *startErrorSession) StartCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

type stubSession struct {
	threadReads         map[string]map[string]any
	threadListResult    map[string]any
	threadListResults   []map[string]any
	threadListErr       error
	threadListCalls     int
	configReadResult    map[string]any
	configReadCalls     int
	configReadErr       error
	models              []appserver.ModelOption
	collaborationModes  []appserver.CollaborationModeOption
	threadReadErr       error
	threadResumeErr     error
	threadStartErr      error
	threadStartResult   map[string]any
	turnStartErr        error
	turnSteerErr        error
	turnSteerErrs       []error
	threadStartCalls    []string
	threadResumeCalls   []threadResumeCall
	turnSteerCalls      []turnCall
	turnStartCalls      []turnCall
	turnInterruptCalls  []turnCall
	goalSetCalls        []goalCall
	goalClearCalls      []goalCall
	respondRequestCalls []respondRequestCall
	stderrTail          []string
}

type threadResumeCall struct {
	threadID string
	cwd      string
}

type turnCall struct {
	threadID          string
	turnID            string
	message           string
	cwd               string
	collaborationMode string
	model             string
	reasoningEffort   string
}

type goalCall struct {
	threadID string
	goal     string
}

type respondRequestCall struct {
	requestID string
	result    map[string]any
}

type stubDesktopInputDispatcher struct {
	loadCalls            []string
	loadCh               chan string
	startCalls           []map[string]any
	steerCalls           []string
	steerInputs          [][]map[string]any
	steerRestoreMessages []map[string]any
	loadErr              error
	loadErrs             []error
	startErr             error
	steerErr             error
	startResult          map[string]any
	steerResult          map[string]any
}

func (s *stubDesktopInputDispatcher) LoadCompleteHistory(ctx context.Context, threadID string) (map[string]any, error) {
	s.loadCalls = append(s.loadCalls, threadID)
	if s.loadCh != nil {
		select {
		case s.loadCh <- threadID:
		default:
		}
	}
	if len(s.loadErrs) > 0 {
		err := s.loadErrs[0]
		s.loadErrs = s.loadErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return map[string]any{"revision": float64(1)}, nil
}

func (s *stubDesktopInputDispatcher) StartTurn(ctx context.Context, threadID string, turnStartParams map[string]any) (map[string]any, error) {
	s.startCalls = append(s.startCalls, turnStartParams)
	if s.startErr != nil {
		return nil, s.startErr
	}
	if s.startResult != nil {
		return s.startResult, nil
	}
	return map[string]any{"turn": map[string]any{"id": "desktop-started-turn"}}, nil
}

func (s *stubDesktopInputDispatcher) SteerTurn(ctx context.Context, threadID string, input []map[string]any, restoreMessage map[string]any) (map[string]any, error) {
	s.steerCalls = append(s.steerCalls, threadID)
	s.steerInputs = append(s.steerInputs, input)
	s.steerRestoreMessages = append(s.steerRestoreMessages, restoreMessage)
	if s.steerErr != nil {
		return nil, s.steerErr
	}
	if s.steerResult != nil {
		return s.steerResult, nil
	}
	return map[string]any{"turn": map[string]any{"id": "desktop-steered-turn"}}, nil
}

func (s *stubSession) Start(ctx context.Context) error { return nil }
func (s *stubSession) Close() error                    { return nil }
func (s *stubSession) Subscribe() <-chan appserver.Event {
	return nil
}
func (s *stubSession) ThreadList(ctx context.Context, limit int, cursor string) (map[string]any, error) {
	s.threadListCalls++
	if s.threadListErr != nil {
		return nil, s.threadListErr
	}
	if len(s.threadListResults) > 0 {
		result := s.threadListResults[0]
		s.threadListResults = s.threadListResults[1:]
		return result, nil
	}
	return s.threadListResult, nil
}
func (s *stubSession) ThreadRead(ctx context.Context, threadID string, includeTurns bool) (map[string]any, error) {
	if s.threadReadErr != nil {
		return nil, s.threadReadErr
	}
	if payload, ok := s.threadReads[threadID]; ok {
		return payload, nil
	}
	return nil, nil
}
func (s *stubSession) ThreadResume(ctx context.Context, threadID, cwd string) (map[string]any, error) {
	s.threadResumeCalls = append(s.threadResumeCalls, threadResumeCall{threadID: threadID, cwd: cwd})
	if s.threadResumeErr != nil {
		return nil, s.threadResumeErr
	}
	return nil, nil
}
func (s *stubSession) ThreadStart(ctx context.Context, cwd string) (map[string]any, error) {
	s.threadStartCalls = append(s.threadStartCalls, cwd)
	if s.threadStartErr != nil {
		return nil, s.threadStartErr
	}
	return s.threadStartResult, nil
}
func (s *stubSession) ConfigRead(ctx context.Context, cwd string, includeLayers bool) (map[string]any, error) {
	s.configReadCalls++
	if s.configReadErr != nil {
		return nil, s.configReadErr
	}
	return s.configReadResult, nil
}
func (s *stubSession) ThreadGoalSet(ctx context.Context, threadID, goal string) (map[string]any, error) {
	s.goalSetCalls = append(s.goalSetCalls, goalCall{threadID: threadID, goal: goal})
	return map[string]any{"thread": map[string]any{"id": threadID}}, nil
}
func (s *stubSession) ThreadGoalClear(ctx context.Context, threadID string) (map[string]any, error) {
	s.goalClearCalls = append(s.goalClearCalls, goalCall{threadID: threadID})
	return map[string]any{"thread": map[string]any{"id": threadID}}, nil
}
func (s *stubSession) TurnStart(ctx context.Context, threadID, message, cwd string, options appserver.TurnStartOptions) (map[string]any, error) {
	if s.turnStartErr != nil {
		return nil, s.turnStartErr
	}
	s.turnStartCalls = append(s.turnStartCalls, turnCall{
		threadID:          threadID,
		message:           message,
		cwd:               cwd,
		collaborationMode: options.CollaborationMode,
		model:             options.Model,
		reasoningEffort:   options.ReasoningEffort,
	})
	return map[string]any{"turn": map[string]any{"id": "started-turn"}}, nil
}
func (s *stubSession) TurnSteer(ctx context.Context, threadID, turnID, message string) (map[string]any, error) {
	s.turnSteerCalls = append(s.turnSteerCalls, turnCall{threadID: threadID, turnID: turnID, message: message})
	if len(s.turnSteerErrs) > 0 {
		err := s.turnSteerErrs[0]
		s.turnSteerErrs = s.turnSteerErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.turnSteerErr != nil {
		return nil, s.turnSteerErr
	}
	return map[string]any{"turn": map[string]any{"id": turnID}}, nil
}
func (s *stubSession) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	s.turnInterruptCalls = append(s.turnInterruptCalls, turnCall{threadID: threadID, turnID: turnID})
	return nil
}
func (s *stubSession) ModelList(ctx context.Context, includeHidden bool) ([]appserver.ModelOption, error) {
	if s.models != nil {
		return s.models, nil
	}
	return []appserver.ModelOption{
		{ID: "gpt-default", IsDefault: true, SupportedReasoningEffort: []string{"low", "medium", "high"}},
		{ID: "gpt-alt", SupportedReasoningEffort: []string{"minimal", "low"}},
	}, nil
}
func (s *stubSession) CollaborationModeList(ctx context.Context) ([]appserver.CollaborationModeOption, error) {
	return s.collaborationModes, nil
}
func (s *stubSession) RespondServerRequest(ctx context.Context, requestID string, result map[string]any) error {
	s.respondRequestCalls = append(s.respondRequestCalls, respondRequestCall{requestID: requestID, result: result})
	return nil
}
func (s *stubSession) StderrTail() []string { return s.stderrTail }

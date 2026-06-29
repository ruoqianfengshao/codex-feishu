package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/mideco-tech/codex-tg/internal/model"
)

func TestResolveExternalIDIsStableAndDistinctByNamespace(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	first, err := store.ResolveExternalID(ctx, "feishu.chat", "oc_1")
	if err != nil {
		t.Fatalf("ResolveExternalID first failed: %v", err)
	}
	second, err := store.ResolveExternalID(ctx, "feishu.chat", "oc_1")
	if err != nil {
		t.Fatalf("ResolveExternalID second failed: %v", err)
	}
	otherNamespace, err := store.ResolveExternalID(ctx, "feishu.user", "oc_1")
	if err != nil {
		t.Fatalf("ResolveExternalID other namespace failed: %v", err)
	}
	if first == 0 || second != first {
		t.Fatalf("stable ids = %d/%d, want same non-zero", first, second)
	}
	if otherNamespace == first {
		t.Fatalf("namespace id collision = %d", first)
	}
	external, err := store.ExternalIDForNumeric(ctx, "feishu.chat", first)
	if err != nil {
		t.Fatalf("ExternalIDForNumeric failed: %v", err)
	}
	if external != "oc_1" {
		t.Fatalf("external = %q, want oc_1", external)
	}
}

func TestFeishuMessageMapRoundTrip(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.PutFeishuMessageMap(ctx, 101, "om_1", 202, "oc_1"); err != nil {
		t.Fatalf("PutFeishuMessageMap failed: %v", err)
	}
	byNumeric, err := store.GetFeishuMessageByNumericID(ctx, 101)
	if err != nil {
		t.Fatalf("GetFeishuMessageByNumericID failed: %v", err)
	}
	if byNumeric == nil || byNumeric.OpenMessageID != "om_1" || byNumeric.OpenChatID != "oc_1" {
		t.Fatalf("byNumeric = %#v", byNumeric)
	}
	byOpen, err := store.GetFeishuMessageByOpenID(ctx, "om_1")
	if err != nil {
		t.Fatalf("GetFeishuMessageByOpenID failed: %v", err)
	}
	if byOpen == nil || byOpen.MessageID != 101 || byOpen.ChatID != 202 {
		t.Fatalf("byOpen = %#v", byOpen)
	}
}

func TestSetStateIfAbsentClaimsOnlyOnce(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	claimed, err := store.SetStateIfAbsent(ctx, "feishu.inbound.test", "first")
	if err != nil {
		t.Fatalf("SetStateIfAbsent(first) failed: %v", err)
	}
	if !claimed {
		t.Fatal("SetStateIfAbsent(first) claimed = false, want true")
	}
	claimed, err = store.SetStateIfAbsent(ctx, "feishu.inbound.test", "second")
	if err != nil {
		t.Fatalf("SetStateIfAbsent(second) failed: %v", err)
	}
	if claimed {
		t.Fatal("SetStateIfAbsent(second) claimed = true, want false")
	}
	value, err := store.GetState(ctx, "feishu.inbound.test")
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if value != "first" {
		t.Fatalf("state value = %q, want first", value)
	}
}

func TestListThreadsFiltersInternalAppServerThreads(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	threads := []model.Thread{
		{
			ID:            "visible-thread",
			Title:         "Visible work",
			ProjectName:   "codex-tg",
			DirectoryName: "codex-tg",
			UpdatedAt:     10,
			LastPreview:   "normal user request",
			Raw:           json.RawMessage(`{"id":"visible-thread","preview":"normal user request"}`),
		},
		{
			ID:            "ephemeral-thread",
			Title:         "01900000-0000-7000-8000-000000000014",
			ProjectName:   "memories",
			DirectoryName: "memories",
			UpdatedAt:     30,
			Raw:           json.RawMessage(`{"thread":{"id":"ephemeral-thread","ephemeral":true,"source":{"subAgent":"memory_consolidation"}}}`),
		},
		{
			ID:            "sub-agent-thread",
			Title:         "01900000-0000-7000-8000-000000000015",
			ProjectName:   "memories",
			DirectoryName: "memories",
			UpdatedAt:     20,
			Raw:           json.RawMessage(`{"id":"sub-agent-thread","source":{"subAgent":"memory_consolidation"}}`),
		},
		{
			ID:            "archived-thread",
			Title:         "Archived work",
			ProjectName:   "archived-project",
			DirectoryName: "archived-project",
			UpdatedAt:     40,
			LastPreview:   "archived user request",
			Archived:      true,
			Raw:           json.RawMessage(`{"id":"archived-thread","preview":"archived user request"}`),
		},
	}
	for _, thread := range threads {
		if err := store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	listed, err := store.ListThreads(ctx, 10, "")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "visible-thread" {
		t.Fatalf("listed threads = %#v, want only visible-thread", listed)
	}

	searched, err := store.ListThreads(ctx, 10, "memories")
	if err != nil {
		t.Fatalf("ListThreads(search) failed: %v", err)
	}
	if len(searched) != 0 {
		t.Fatalf("searched internal threads = %#v, want none", searched)
	}

	searchedArchived, err := store.ListThreads(ctx, 10, "archived")
	if err != nil {
		t.Fatalf("ListThreads(search archived) failed: %v", err)
	}
	if len(searchedArchived) != 0 {
		t.Fatalf("searched archived threads = %#v, want none", searchedArchived)
	}

	grouped, err := store.ListProjectGroups(ctx)
	if err != nil {
		t.Fatalf("ListProjectGroups failed: %v", err)
	}
	if _, ok := grouped["memories"]; ok {
		t.Fatalf("project groups include internal memories project: %#v", grouped)
	}
	if _, ok := grouped["archived-project"]; ok {
		t.Fatalf("project groups include archived project: %#v", grouped)
	}

	count, err := store.CountThreads(ctx)
	if err != nil {
		t.Fatalf("CountThreads failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountThreads = %d, want only visible-thread", count)
	}
}

func TestDeliveryQueueClaimRetryAndComplete(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	item := model.DeliveryQueueItem{
		EventID:     "event-1",
		ChatKey:     model.ChatKey(123456789, 0),
		ChatID:      123456789,
		TopicID:     0,
		ThreadID:    "thread-1",
		Kind:        "observer",
		Status:      model.DeliveryStatusPending,
		AvailableAt: model.NowString(),
		PayloadJSON: `{"text":"hello"}`,
		CreatedAt:   model.NowString(),
		UpdatedAt:   model.NowString(),
	}
	if err := store.EnqueueDelivery(ctx, item); err != nil {
		t.Fatalf("EnqueueDelivery failed: %v", err)
	}

	batch, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimDeliveryBatch failed: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("ClaimDeliveryBatch len = %d, want 1", len(batch))
	}
	if batch[0].Status != model.DeliveryStatusPending {
		t.Fatalf("Claimed status = %q, want %q", batch[0].Status, model.DeliveryStatusPending)
	}

	retryAt := time.Now().UTC().Add(5 * time.Second)
	if err := store.FailDelivery(ctx, batch[0].ID, 1, retryAt, "temporary failure", false); err != nil {
		t.Fatalf("FailDelivery failed: %v", err)
	}
	if err := store.RecordDeliveryAttempt(ctx, batch[0].ID, 1, "send_error", "temporary failure"); err != nil {
		t.Fatalf("RecordDeliveryAttempt failed: %v", err)
	}

	backlog, err := store.DeliveryQueueBacklog(ctx)
	if err != nil {
		t.Fatalf("DeliveryQueueBacklog failed: %v", err)
	}
	if backlog != 1 {
		t.Fatalf("DeliveryQueueBacklog = %d, want 1", backlog)
	}
}

func TestThreadPanelLifecycleKeepsSingleCurrentPanelPerChatThread(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	first, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:              123456789,
		TopicID:             0,
		ProjectName:         "Codex",
		ThreadID:            "thread-1",
		SummaryMessageID:    101,
		ToolMessageID:       102,
		OutputMessageID:     103,
		CurrentTurnID:       "turn-1",
		Status:              "inProgress",
		ArchiveEnabled:      true,
		LastSummaryHash:     "summary-1",
		LastToolHash:        "tool-1",
		LastOutputHash:      "output-1",
		UserMessageID:       100,
		LastUserNoticeFP:    "user-fp-1",
		PlanPromptMessageID: 110,
		LastPlanPromptFP:    "plan-fp-1",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(first) failed: %v", err)
	}

	second, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         "thread-1",
		SummaryMessageID: 201,
		ToolMessageID:    202,
		OutputMessageID:  203,
		CurrentTurnID:    "turn-2",
		Status:           "completed",
		ArchiveEnabled:   true,
		LastSummaryHash:  "summary-2",
		LastToolHash:     "tool-2",
		LastOutputHash:   "output-2",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(second) failed: %v", err)
	}

	current, err := store.GetCurrentThreadPanel(ctx, 123456789, 0, "thread-1")
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current panel = %#v, want second panel %#v", current, second)
	}

	firstLoaded, err := store.GetThreadPanelByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(first) failed: %v", err)
	}
	if firstLoaded == nil {
		t.Fatal("GetThreadPanelByID(first) returned nil")
	}
	if firstLoaded.IsCurrent {
		t.Fatalf("first panel should no longer be current: %#v", firstLoaded)
	}
	if firstLoaded.UserMessageID != 100 || firstLoaded.LastUserNoticeFP != "user-fp-1" {
		t.Fatalf("first user notice state = id %d fp %q, want 100/user-fp-1", firstLoaded.UserMessageID, firstLoaded.LastUserNoticeFP)
	}
	if firstLoaded.PlanPromptMessageID != 110 || firstLoaded.LastPlanPromptFP != "plan-fp-1" {
		t.Fatalf("first plan prompt state = id %d fp %q, want 110/plan-fp-1", firstLoaded.PlanPromptMessageID, firstLoaded.LastPlanPromptFP)
	}

	if err := store.UpdateThreadPanelUserNotice(ctx, second.ID, 250, "user-fp-2"); err != nil {
		t.Fatalf("UpdateThreadPanelUserNotice failed: %v", err)
	}
	secondLoaded, err := store.GetThreadPanelByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(second) failed: %v", err)
	}
	if secondLoaded.UserMessageID != 250 || secondLoaded.LastUserNoticeFP != "user-fp-2" {
		t.Fatalf("second user notice state = id %d fp %q, want 250/user-fp-2", secondLoaded.UserMessageID, secondLoaded.LastUserNoticeFP)
	}
	if err := store.UpdateThreadPanelPlanPrompt(ctx, second.ID, 260, "plan-fp-2"); err != nil {
		t.Fatalf("UpdateThreadPanelPlanPrompt failed: %v", err)
	}
	secondLoaded, err = store.GetThreadPanelByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(second after plan prompt) failed: %v", err)
	}
	if secondLoaded.PlanPromptMessageID != 260 || secondLoaded.LastPlanPromptFP != "plan-fp-2" {
		t.Fatalf("second plan prompt state = id %d fp %q, want 260/plan-fp-2", secondLoaded.PlanPromptMessageID, secondLoaded.LastPlanPromptFP)
	}
}

func TestGetLatestCurrentPanelForChat(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.UpsertThread(ctx, model.Thread{
		ID:           "thread-1",
		Title:        "First",
		ProjectName:  "Codex",
		ActiveTurnID: "turn-old",
	}); err != nil {
		t.Fatalf("UpsertThread(thread-1) failed: %v", err)
	}
	if err := store.UpsertThread(ctx, model.Thread{
		ID:           "thread-2",
		Title:        "Second",
		ProjectName:  "Codex",
		ActiveTurnID: "turn-2",
	}); err != nil {
		t.Fatalf("UpsertThread(thread-2) failed: %v", err)
	}
	if err := store.UpsertThread(ctx, model.Thread{
		ID:           "thread-other",
		Title:        "Other",
		ProjectName:  "Codex",
		ActiveTurnID: "turn-other",
	}); err != nil {
		t.Fatalf("UpsertThread(thread-other) failed: %v", err)
	}

	first, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:        123456789,
		TopicID:       0,
		ProjectName:   "Codex",
		ThreadID:      "thread-1",
		SourceMode:    model.PanelSourceExplicit,
		CurrentTurnID: "turn-1",
		Status:        "completed",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(first) failed: %v", err)
	}
	second, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:        123456789,
		TopicID:       0,
		ProjectName:   "Codex",
		ThreadID:      "thread-2",
		SourceMode:    model.PanelSourceFeishuInput,
		CurrentTurnID: "turn-2",
		Status:        "inProgress",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(second) failed: %v", err)
	}
	otherChat, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:      987654321,
		TopicID:     0,
		ProjectName: "Codex",
		ThreadID:    "thread-other",
		SourceMode:  model.PanelSourceExplicit,
		Status:      "inProgress",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(otherChat) failed: %v", err)
	}
	if err := store.UpdateThreadPanelState(ctx, first.ID, "turn-1", "inProgress", "summary-newer", "", "", ""); err != nil {
		t.Fatalf("UpdateThreadPanelState(first) failed: %v", err)
	}

	current, err := store.GetLatestCurrentPanelForChat(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("GetLatestCurrentPanelForChat failed: %v", err)
	}
	if current == nil || current.ID != second.ID || current.ThreadID != "thread-2" {
		t.Fatalf("latest current panel = %#v, want second panel %#v", current, second)
	}
	if current.ID == first.ID || current.ID == otherChat.ID {
		t.Fatalf("latest current panel selected wrong chat/thread: %#v", current)
	}
}

func TestThreadPanelSourceModePersistsAndReplacesCurrentPanel(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	explicit, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         "thread-1",
		SourceMode:       model.PanelSourceExplicit,
		SummaryMessageID: 301,
		ToolMessageID:    302,
		OutputMessageID:  303,
		CurrentTurnID:    "turn-1",
		Status:           "completed",
		ArchiveEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(explicit) failed: %v", err)
	}
	if explicit.SourceMode != model.PanelSourceExplicit {
		t.Fatalf("explicit panel SourceMode = %q, want explicit", explicit.SourceMode)
	}

	feishuInput, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         "thread-1",
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 401,
		ToolMessageID:    402,
		OutputMessageID:  403,
		CurrentTurnID:    "turn-2",
		Status:           "completed",
		ArchiveEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(feishu_input) failed: %v", err)
	}
	if feishuInput.SourceMode != model.PanelSourceFeishuInput {
		t.Fatalf("feishu_input panel SourceMode = %q, want feishu_input", feishuInput.SourceMode)
	}

	current, err := store.GetCurrentThreadPanel(ctx, 123456789, 0, "thread-1")
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if current == nil {
		t.Fatal("GetCurrentThreadPanel returned nil")
	}
	if current.ID != feishuInput.ID {
		t.Fatalf("current panel ID = %d, want %d", current.ID, feishuInput.ID)
	}
	if current.SourceMode != model.PanelSourceFeishuInput {
		t.Fatalf("current panel SourceMode = %q, want feishu_input", current.SourceMode)
	}

	explicitLoaded, err := store.GetThreadPanelByID(ctx, explicit.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(explicit) failed: %v", err)
	}
	if explicitLoaded == nil {
		t.Fatal("GetThreadPanelByID(explicit) returned nil")
	}
	if explicitLoaded.SourceMode != model.PanelSourceExplicit {
		t.Fatalf("explicit panel reloaded SourceMode = %q, want explicit", explicitLoaded.SourceMode)
	}
}

func TestSteerStateArmLoadAndClear(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	state := model.SteerState{
		ChatKey:   model.ChatKey(123456789, 0),
		ChatID:    123456789,
		TopicID:   0,
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		PanelID:   77,
		ExpiresAt: model.NowString(),
		CreatedAt: model.NowString(),
		UpdatedAt: model.NowString(),
	}
	if err := store.ArmSteerState(ctx, state); err != nil {
		t.Fatalf("ArmSteerState failed: %v", err)
	}

	loaded, err := store.GetSteerState(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("GetSteerState failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("GetSteerState returned nil")
	}
	if loaded.ThreadID != "thread-1" || loaded.TurnID != "turn-1" || loaded.PanelID != 77 {
		t.Fatalf("loaded steer state = %#v, want thread-1/turn-1/panel 77", loaded)
	}

	if err := store.ClearSteerState(ctx, 123456789, 0); err != nil {
		t.Fatalf("ClearSteerState failed: %v", err)
	}
	loaded, err = store.GetSteerState(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("GetSteerState(after clear) failed: %v", err)
	}
	if loaded != nil {
		t.Fatalf("GetSteerState(after clear) = %#v, want nil", loaded)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s) failed: %v", path, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

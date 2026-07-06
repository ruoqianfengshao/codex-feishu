package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/ruoqianfengshao/codex-feishu/internal/model"
	"github.com/ruoqianfengshao/codex-feishu/internal/updater"
	"github.com/ruoqianfengshao/codex-feishu/internal/version"
)

var (
	checkForUpdate = updater.Check
	applyUpdate    = updater.Update
)

func (s *Service) helpUpdateRow(ctx context.Context, lang string) model.MessageSectionRow {
	return model.MessageSectionRow{
		Title:    localized(lang, "版本", "Version"),
		Trailing: fmt.Sprintf("v%s", version.Version),
		Button:   s.callbackButton(ctx, localized(lang, "检查更新", "Check"), "update_check", "", "", "", nil),
	}
}

func (s *Service) checkUpdateCallback(ctx context.Context, chatID, topicID int64) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	result, err := checkForUpdate(ctx, updater.Options{
		Repo:           s.cfg.UpdateRepo,
		CurrentVersion: version.Version,
	})
	if err != nil {
		return nil, err
	}
	return s.updateStatusResponse(ctx, lang, result, false, chatID, topicID), nil
}

func (s *Service) applyUpdateCallback(ctx context.Context, chatID, topicID int64) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	result, err := applyUpdate(ctx, updater.Options{
		Repo:           s.cfg.UpdateRepo,
		CurrentVersion: version.Version,
	})
	if err != nil {
		return nil, err
	}
	text := localized(lang,
		fmt.Sprintf("更新完成：v%s -> v%s。服务会自动重启。", result.CurrentVersion, result.LatestVersion),
		fmt.Sprintf("Update complete: v%s -> v%s. The service will restart automatically.", result.CurrentVersion, result.LatestVersion),
	)
	if result.AlreadyLatest {
		text = localized(lang,
			fmt.Sprintf("当前已经是最新版本：v%s。", result.CurrentVersion),
			fmt.Sprintf("Already up to date: v%s.", result.CurrentVersion),
		)
	}
	if sender := s.currentSender(); sender != nil {
		_, _ = sender.SendMessage(ctx, chatID, topicID, text, nil, notifySendOptions())
	}
	if result.Updated {
		go s.exitAfterUpdateSoon()
	}
	return s.updateStatusResponse(ctx, lang, result, true, chatID, topicID), nil
}

func (s *Service) updateStatusResponse(ctx context.Context, lang string, result updater.Result, applied bool, _, topicID int64) *DirectResponse {
	current := "v" + result.CurrentVersion
	latest := "v" + result.LatestVersion
	statusTitle := localized(lang, "发现新版本", "Update available")
	statusText := localized(lang, "可以直接更新，完成后服务会自动重启。", "Install it now; the service will restart automatically.")
	statusBackground := "cus-4"
	statusBorder := "cus-7"
	callback := localized(lang, "版本检查完成", "Version checked")
	if result.AlreadyLatest {
		statusTitle = localized(lang, "已是最新版本", "Up to date")
		statusText = localized(lang, "当前安装版本已经与 GitHub Release 保持一致。", "The installed version matches the latest GitHub Release.")
		statusBackground = "cus-5"
	} else if applied {
		statusTitle = localized(lang, "更新完成", "Updated")
		statusText = localized(lang, "已完成安装，服务会自动重启并加载新版本。", "The update is installed; the service will restart and load the new version.")
		statusBackground = "cus-5"
		callback = localized(lang, "更新完成", "Update complete")
	}
	rows := []model.MessageSectionRow{
		{
			Title:           current + "  →  " + latest,
			Trailing:        localized(lang, "当前版本到最新版本", "Current to latest"),
			BackgroundStyle: "cus-2",
			BorderColor:     "cus-1",
			Button:          s.callbackButton(ctx, localized(lang, "重新检查", "Check again"), "update_check", "", "", "", nil),
		},
		{
			Title:           statusTitle,
			Trailing:        statusText,
			BackgroundStyle: statusBackground,
			BorderColor:     statusBorder,
		},
	}
	if result.ReleaseURL != "" {
		rows = append(rows, model.MessageSectionRow{
			Title:           localized(lang, "发布页", "Release"),
			Trailing:        result.ReleaseURL,
			BackgroundStyle: "cus-0",
			BorderColor:     "cus-1",
		})
	}
	buttons := [][]model.ButtonSpec{}
	if !result.AlreadyLatest && !applied {
		buttons = append(buttons, []model.ButtonSpec{s.callbackButton(ctx, localized(lang, "立即更新", "Update now"), "update_apply", "", "", "", nil)})
	}
	return &DirectResponse{
		Text: localized(lang, "Codex Feishu 版本", "Codex Feishu version"),
		Sections: []model.MessageSection{
			{Text: localized(lang, "版本检查", "Version check"), Heading: true},
			{Rows: rows},
		},
		Buttons:         buttons,
		CallbackText:    callback,
		DeliveryTopicID: topicID,
	}
}

func (s *Service) currentSender() Sender {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sender
}

func (s *Service) exitAfterUpdateSoon() {
	time.Sleep(2 * time.Second)
	exitAfterAutoUpdate(0)
}

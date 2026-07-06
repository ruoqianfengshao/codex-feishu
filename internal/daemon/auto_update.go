package daemon

import (
	"context"
	"os"
	"time"

	"github.com/ruoqianfengshao/codex-feishu/internal/updater"
	"github.com/ruoqianfengshao/codex-feishu/internal/version"
)

var exitAfterAutoUpdate = os.Exit

func (s *Service) autoUpdateLoop(ctx context.Context) {
	interval := s.cfg.UpdateCheckInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	timer := time.NewTimer(nextAutoUpdateDelay(ctx, s, interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			updated := s.runAutoUpdateOnce(ctx)
			if updated {
				return
			}
			timer.Reset(interval)
		}
	}
}

func nextAutoUpdateDelay(ctx context.Context, s *Service, interval time.Duration) time.Duration {
	raw, err := s.store.GetState(ctx, "update.last_checked_at")
	if err != nil || raw == "" {
		return 2 * time.Minute
	}
	last, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return 2 * time.Minute
	}
	due := last.Add(interval)
	now := s.now().UTC()
	if !due.After(now) {
		return time.Second
	}
	return due.Sub(now)
}

func (s *Service) runAutoUpdateOnce(ctx context.Context) bool {
	now := s.now().UTC().Format(time.RFC3339Nano)
	_ = s.store.SetState(ctx, "update.last_checked_at", now)
	_ = s.store.SetState(ctx, "update.last_error", "")
	result, err := updater.Update(ctx, updater.Options{
		Repo:           s.cfg.UpdateRepo,
		CurrentVersion: version.Version,
	})
	if err != nil {
		msg := sanitizeDiagnosticString(err.Error())
		_ = s.store.SetState(ctx, "update.last_error", msg)
		s.logLifecycle("auto_update_failed", lifecycleFields{"error": msg})
		return false
	}
	_ = s.store.SetState(ctx, "update.latest_version", result.LatestVersion)
	if result.AlreadyLatest {
		_ = s.store.SetState(ctx, "update.last_result", "already_latest")
		return false
	}
	if !result.Updated {
		_ = s.store.SetState(ctx, "update.last_result", "checked")
		return false
	}
	_ = s.store.SetState(ctx, "update.last_result", "updated")
	_ = s.store.SetState(ctx, "update.updated_at", now)
	_ = s.store.SetState(ctx, "update.updated_from", result.CurrentVersion)
	_ = s.store.SetState(ctx, "update.updated_to", result.LatestVersion)
	s.logLifecycle("auto_update_applied", lifecycleFields{
		"from": result.CurrentVersion,
		"to":   result.LatestVersion,
	})
	go s.exitAfterUpdateSoon()
	return true
}

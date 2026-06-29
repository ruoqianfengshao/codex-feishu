package daemon

import "context"

type compactFeishuTopicCardKey struct{}

func withCompactFeishuTopicCard(ctx context.Context) context.Context {
	return context.WithValue(ctx, compactFeishuTopicCardKey{}, true)
}

func compactFeishuTopicCard(ctx context.Context) bool {
	compact, _ := ctx.Value(compactFeishuTopicCardKey{}).(bool)
	return compact
}

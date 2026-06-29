package model

import "context"

type forceThreadTopicActivationKey struct{}

func WithForcedThreadTopicActivation(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceThreadTopicActivationKey{}, true)
}

func ForceThreadTopicActivation(ctx context.Context) bool {
	force, _ := ctx.Value(forceThreadTopicActivationKey{}).(bool)
	return force
}

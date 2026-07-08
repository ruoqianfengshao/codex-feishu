package model

import "context"

type forceThreadTopicActivationKey struct{}
type suppressThreadTopicActivationKey struct{}
type silentThreadTopicActivationKey struct{}

func WithForcedThreadTopicActivation(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceThreadTopicActivationKey{}, true)
}

func ForceThreadTopicActivation(ctx context.Context) bool {
	force, _ := ctx.Value(forceThreadTopicActivationKey{}).(bool)
	return force
}

func WithSuppressedThreadTopicActivation(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressThreadTopicActivationKey{}, true)
}

func SuppressThreadTopicActivation(ctx context.Context) bool {
	suppress, _ := ctx.Value(suppressThreadTopicActivationKey{}).(bool)
	return suppress
}

func WithSilentThreadTopicActivation(ctx context.Context) context.Context {
	return context.WithValue(ctx, silentThreadTopicActivationKey{}, true)
}

func SilentThreadTopicActivation(ctx context.Context) bool {
	silent, _ := ctx.Value(silentThreadTopicActivationKey{}).(bool)
	return silent
}

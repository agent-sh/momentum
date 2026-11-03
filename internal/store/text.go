package store

import "context"

type textCleanerKey struct{}

// WithTextCleaner cleans generated case notes before the store clips them.
// The caller owns the cleaner; the store has no dependency on a model or process.
func WithTextCleaner(ctx context.Context, clean func(context.Context, string) (string, error)) context.Context {
	return context.WithValue(ctx, textCleanerKey{}, clean)
}

func cleanText(ctx context.Context, text string) (string, error) {
	if clean, ok := ctx.Value(textCleanerKey{}).(func(context.Context, string) (string, error)); ok {
		return clean(ctx, text)
	}
	return text, nil
}

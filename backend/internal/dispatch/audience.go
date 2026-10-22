package dispatch

import (
	"context"

	"smart-campaign-composer/backend/internal/domain"
)

// UserLoader loads users by id; repository.UserRepository satisfies it.
type UserLoader interface {
	ForEachByIDs(ctx context.Context, ids []int64, fn func(domain.User) error) error
}

// IDAudience is an audience given as user ids (for example the ids matched
// by the approved segment rule). Users are loaded at dispatch time, so the
// opt-in flags the dispatcher checks are current, not those at approval.
func IDAudience(loader UserLoader, ids []int64) AudienceSource {
	return AudienceFunc(func(ctx context.Context, _ int64, yield func(domain.User) error) error {
		return loader.ForEachByIDs(ctx, ids, yield)
	})
}

// StaticAudience yields the given users as they are (tests, fixtures).
func StaticAudience(users ...domain.User) AudienceSource {
	return AudienceFunc(func(ctx context.Context, _ int64, yield func(domain.User) error) error {
		for _, u := range users {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(u); err != nil {
				return err
			}
		}
		return nil
	})
}

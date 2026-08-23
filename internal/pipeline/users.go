package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

// User is UserRepository's read view of one household member — just the
// fields Nutrition Advisor input (docs/adr/006-nutrition-guidance.md §7,
// §10) needs: BirthDate/Sex (the pair docs/domain/02-user.md §2 already
// earmarks for exactly this purpose — "используется для расчёта возраста
// в Medical Profile" / "используется для референсных диапазонов,
// специфичных по полу") and Language (config.UserConfig.Language, passed
// straight through). BirthDate is nil when unset or unparseable — a
// missing age is a rendering gap for Nutrition Advisor input, not a fatal
// error, the same posture profile.Builder.documentTitle already takes for
// a missing document title.
type User struct {
	BirthDate *time.Time
	Sex       string
	Language  string
}

// UserRepository implements docs/domain/02-user.md §7's long-documented but
// (until Nutrition Advisor) never-built interface, narrowed to what this
// package needs — FindByID only, no List. The only previous consumer of
// birthDate/sex, internal/mcpserver's userGate, reads config.UserConfig
// directly instead, because it only ever needed sharedWith/encryption.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (User, error)
}

// ErrUserNotFound is returned by configUserRepository.FindByID for an id
// absent from the loaded configuration.
var ErrUserNotFound = errors.New("pipeline: user not found")

// configUserRepository implements UserRepository over the in-memory
// []config.UserConfig loaded once at startup (docs/domain/02-user.md §3:
// "User не хранится в SQLite") — the same source userGate already reads,
// just wrapped so internal/pipeline doesn't reach into internal/config's
// raw YAML shape directly.
type configUserRepository struct {
	users map[string]config.UserConfig
}

// NewConfigUserRepository builds a UserRepository over users.
func NewConfigUserRepository(users []config.UserConfig) UserRepository {
	m := make(map[string]config.UserConfig, len(users))
	for _, u := range users {
		m[u.ID] = u
	}
	return &configUserRepository{users: m}
}

func (r *configUserRepository) FindByID(_ context.Context, id string) (User, error) {
	u, ok := r.users[id]
	if !ok {
		return User{}, ErrUserNotFound
	}
	var birthDate *time.Time
	if u.BirthDate != "" {
		// config.Load's validate() already rejects an unparseable
		// birth_date at startup, so this Parse failing here would mean
		// config validation itself has a gap — treated the same as
		// "unset" rather than propagated, consistent with BirthDate's own
		// doc comment ("a missing age is a rendering gap, not a fatal
		// error").
		if t, err := time.Parse("2006-01-02", u.BirthDate); err == nil {
			birthDate = &t
		}
	}
	return User{BirthDate: birthDate, Sex: u.Sex, Language: u.Language}, nil
}

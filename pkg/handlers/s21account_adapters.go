package handlers

import (
	"context"
	"errors"

	s21account "github.com/arseniisemenow/s21-account-go"

	"github.com/arseniisemenow/s21-identity-bot/pkg/s21"
)

// s21ClientAdapter wraps the bot's s21.Client so it satisfies
// s21account.S21Client. The only translation is mapping s21.ErrInvalidCredentials
// → s21account.ErrInvalidCredentials so the shared package's mark-bad path
// fires on the right error class.
type s21ClientAdapter struct{ inner s21.Client }

func (a s21ClientAdapter) Authenticate(ctx context.Context, login, password string) (s21account.Profile, error) {
	p, err := a.inner.Authenticate(ctx, login, password)
	switch {
	case errors.Is(err, s21.ErrInvalidCredentials):
		return s21account.Profile{}, s21account.ErrInvalidCredentials
	case err != nil:
		return s21account.Profile{}, err
	}
	return s21account.Profile{
		Login:      p.Login,
		CampusID:   p.CampusID,
		CampusName: p.CampusName,
	}, nil
}

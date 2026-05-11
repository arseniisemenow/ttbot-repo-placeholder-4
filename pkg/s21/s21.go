// Package s21 wraps s21auto-client-go for the identity bot's /admin command.
// We only need credential validation; nickname resolution is delegated to
// the identity service.
package s21

import (
	"context"
	"errors"
	"strings"

	s21client "github.com/arseniisemenow/s21auto-client-go"
	"github.com/arseniisemenow/s21auto-client-go/requests"
)

// Profile is what we extract from a successful Authenticate.
type Profile struct {
	Login         string
	CampusID      string
	CampusName    string
	CoalitionName string
}

// Client is the interface used by handlers; production implementation talks
// to S21, tests use Mock.
type Client interface {
	Authenticate(ctx context.Context, login, password string) (Profile, error)
}

// Errors.
var (
	ErrInvalidCredentials = errors.New("s21: invalid credentials")
	ErrUnavailable        = errors.New("s21: unavailable")
)

type realClient struct{}

// NewClient returns the production Client.
func NewClient() Client { return realClient{} }

func (realClient) Authenticate(_ context.Context, login, password string) (Profile, error) {
	c := s21client.New(s21client.DefaultAuth(login, password))
	data, err := c.R().DashboardHeaderGetInfo(requests.DashboardHeaderGetInfo_Variables{})
	if err != nil {
		return Profile{}, mapErr(err)
	}
	u := data.User.GetCurrentUser
	if u.Login == "" {
		return Profile{}, ErrInvalidCredentials
	}
	var campusID, campusName string
	if len(u.StudentRoles) > 0 {
		campusID = u.StudentRoles[0].School.ID
		campusName = u.StudentRoles[0].School.ShortName
	}
	return Profile{
		Login:         u.Login,
		CampusID:      campusID,
		CampusName:    campusName,
		CoalitionName: data.Student.GetUserTournamentWidget.CoalitionMember.Coalition.Name,
	}, nil
}

func mapErr(err error) error {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "authentication failed"),
		strings.Contains(msg, "401"),
		strings.Contains(msg, "invalid"):
		return errors.Join(ErrInvalidCredentials, err)
	}
	return errors.Join(ErrUnavailable, err)
}

// Mock is a test Client.
type Mock struct {
	Profiles  map[string]Profile
	Passwords map[string]string
	FailNext  error
}

// NewMock returns a Mock with empty maps.
func NewMock() *Mock {
	return &Mock{Profiles: map[string]Profile{}, Passwords: map[string]string{}}
}

// SetUser configures the mock.
func (m *Mock) SetUser(login, password string, p Profile) {
	if p.Login == "" {
		p.Login = login
	}
	m.Profiles[login] = p
	m.Passwords[login] = password
}

// Authenticate validates and returns.
func (m *Mock) Authenticate(_ context.Context, login, password string) (Profile, error) {
	if m.FailNext != nil {
		err := m.FailNext
		m.FailNext = nil
		return Profile{}, err
	}
	if want, ok := m.Passwords[login]; !ok || want != password {
		return Profile{}, ErrInvalidCredentials
	}
	return m.Profiles[login], nil
}

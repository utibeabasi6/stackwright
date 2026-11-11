package api

import (
	"errors"
	"net/mail"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

func invalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func checkOrg(slug, name string) error {
	if !slugPattern.MatchString(slug) {
		return invalid("the org's short name needs 1 to 40 lowercase letters, digits or hyphens")
	}
	if n := len(strings.TrimSpace(name)); n == 0 || n > 100 {
		return invalid("the org's name needs 1 to 100 characters")
	}
	return nil
}

func checkEmail(email string) error {
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return invalid("that is not an email address")
	}
	return nil
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInviteNotFound = errors.New("this invite does not exist, has expired, or was used")
	ErrInviteEmail    = errors.New("this invite is for another email address")
)

type Invite struct {
	ID      string
	OrgID   string
	OrgSlug string
	OrgName string
	Email   string
	Role    string
}

// LookupInvite leaves the transaction's org setting on the invite's org.
func LookupInvite(ctx context.Context, tx pgx.Tx, token string) (Invite, error) {
	if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.invite_token_hash', $1, true)`,
		hex.EncodeToString(HashToken(token))); err != nil {
		return Invite{}, err
	}
	var inv Invite
	err := tx.QueryRow(ctx, `SELECT id, org_id, email, role FROM invites
		WHERE token_hash = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()`,
		HashToken(token)).Scan(&inv.ID, &inv.OrgID, &inv.Email, &inv.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, ErrInviteNotFound
	}
	if err != nil {
		return Invite{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, inv.OrgID); err != nil {
		return Invite{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT slug, name FROM orgs WHERE id = $1`, inv.OrgID).Scan(&inv.OrgSlug, &inv.OrgName); err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// A user who is already a member keeps their role.
func AcceptInvite(ctx context.Context, tx pgx.Tx, inv Invite, userID, email string) error {
	if !strings.EqualFold(inv.Email, email) {
		return ErrInviteEmail
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (org_id, user_id) DO NOTHING`, inv.OrgID, userID, inv.Role); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET accepted_at = now() WHERE id = $1`, inv.ID); err != nil {
		return fmt.Errorf("close invite: %w", err)
	}
	return nil
}

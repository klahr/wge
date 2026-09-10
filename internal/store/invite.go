package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Invitation problems a player can be told about. They are distinguished
// deliberately: a code is not a secret the holder has to be protected from, and
// "that one has already been used" saves somebody hunting for a typo that is
// not there.
var (
	ErrInviteUnknown = errors.New("no such invitation")
	ErrInviteSpent   = errors.New("invitation already used")
	ErrInviteExpired = errors.New("invitation expired")
)

// inviteAlphabet is what an invitation code is written in.
//
// Unlike a derived password, this gets read off a screen and typed in by hand,
// possibly from a printout or a whiteboard. I, L, O, U, 0 and 1 are left out
// because the pairs people confuse are the pairs people mistype.
const inviteAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

// inviteLength is the number of characters in a code, before grouping.
// Thirty symbols over sixteen places is a little over 78 bits, which is not
// guessable at any rate a network will carry.
const inviteLength = 16

// NewInviteToken returns a fresh invitation code, grouped for reading aloud.
func NewInviteToken() (string, error) {
	raw := make([]byte, 0, inviteLength)

	// Rejection sampling: 256 is not a multiple of 30, so folding every byte
	// would make the first few letters likelier than the rest.
	const limit = byte(256 - (256 % len(inviteAlphabet)))
	buf := make([]byte, inviteLength*2)

	for len(raw) < inviteLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate invitation: %w", err)
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			raw = append(raw, inviteAlphabet[int(b)%len(inviteAlphabet)])
			if len(raw) == inviteLength {
				break
			}
		}
	}

	var groups []string
	for i := 0; i < inviteLength; i += 4 {
		groups = append(groups, string(raw[i:i+4]))
	}
	return strings.Join(groups, "-"), nil
}

// normalizeToken strips the grouping and anything else a person may have typed
// around the code, so that a pasted "wge-ABCD efgh" still matches.
func normalizeToken(token string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(token) {
		if strings.ContainsRune(inviteAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hashToken is what the database holds.
//
// An invitation is a bearer credential: whoever has it can enrol. Storing the
// hash means a copy of the database is not a stack of usable invitations, and
// it costs nothing, because nobody ever needs to read one back -- a code that
// has been lost is replaced, not recovered.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(normalizeToken(token)))
	return hex.EncodeToString(sum[:])
}

// Invite is permission to become a player.
type Invite struct {
	ID        int64
	Note      string
	UsesLeft  int
	Uses      int
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Expired reports whether the invitation is past its date.
func (i *Invite) Expired(now time.Time) bool {
	return !i.ExpiresAt.IsZero() && now.After(i.ExpiresAt)
}

// CreateInvite records an invitation and returns it. The token itself is not
// stored and cannot be read back.
func (s *Store) CreateInvite(ctx context.Context, token string, uses int, expires time.Time, note string) (*Invite, error) {
	if uses < 1 {
		return nil, fmt.Errorf("an invitation with %d uses is no invitation", uses)
	}
	now := time.Now()

	var expiresAt any
	if !expires.IsZero() {
		expiresAt = expires.Unix()
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO invites (hash, note, uses, uses_left, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hashToken(token), note, uses, uses, expiresAt, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("create invitation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Invite{
		ID: id, Note: note, Uses: uses, UsesLeft: uses,
		ExpiresAt: expires, CreatedAt: now,
	}, nil
}

// RedeemInvite spends an invitation and creates the player who spent it.
//
// Both happen in one transaction. Splitting them would leave either a player
// who never used an invitation or an invitation spent on a player who was
// never created, and the second is the one that costs somebody their place.
func (s *Store) RedeemInvite(ctx context.Context, token, handle, fingerprint string) (*Player, error) {
	now := time.Now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		id        int64
		usesLeft  int
		expiresAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, uses_left, expires_at FROM invites WHERE hash = ?`,
		hashToken(token)).Scan(&id, &usesLeft, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInviteUnknown
	}
	if err != nil {
		return nil, err
	}

	if expiresAt.Valid && now.After(time.Unix(expiresAt.Int64, 0)) {
		return nil, ErrInviteExpired
	}
	if usesLeft < 1 {
		return nil, ErrInviteSpent
	}

	// Guarded in the statement as well as checked above: two people redeeming
	// the last use of a workshop code at the same moment must not both get it.
	spent, err := tx.ExecContext(ctx,
		`UPDATE invites SET uses_left = uses_left - 1 WHERE id = ? AND uses_left > 0`, id)
	if err != nil {
		return nil, err
	}
	if n, err := spent.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrInviteSpent
	}

	player, err := createPlayerTx(ctx, tx, handle, fingerprint, now)
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO invite_redemptions (invite_id, player_id, redeemed_at) VALUES (?, ?, ?)`,
		id, player.ID, now.Unix()); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return player, nil
}

// Invites lists the invitations, newest first.
func (s *Store) Invites(ctx context.Context) ([]*Invite, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, note, uses, uses_left, expires_at, created_at
		   FROM invites ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Invite
	for rows.Next() {
		var (
			i         Invite
			expiresAt sql.NullInt64
			created   int64
		)
		if err := rows.Scan(&i.ID, &i.Note, &i.Uses, &i.UsesLeft, &expiresAt, &created); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			i.ExpiresAt = time.Unix(expiresAt.Int64, 0)
		}
		i.CreatedAt = time.Unix(created, 0)
		out = append(out, &i)
	}
	return out, rows.Err()
}

// RevokeInvite spends an invitation's remaining uses without deleting it, so
// that who used it and when survives the revocation.
func (s *Store) RevokeInvite(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE invites SET uses_left = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrInviteUnknown
	}
	return nil
}

// CheckInvite reports whether an invitation could be redeemed, without
// spending it.
//
// It exists so a player learns their code is wrong when they type it, rather
// than after choosing a handle. It is not the guard: RedeemInvite spends the
// use under a condition in the statement, so two people racing for the last
// use of a code still cannot both have it.
func (s *Store) CheckInvite(ctx context.Context, token string) error {
	var (
		usesLeft  int
		expiresAt sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT uses_left, expires_at FROM invites WHERE hash = ?`,
		hashToken(token)).Scan(&usesLeft, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInviteUnknown
	}
	if err != nil {
		return err
	}

	if expiresAt.Valid && time.Now().After(time.Unix(expiresAt.Int64, 0)) {
		return ErrInviteExpired
	}
	if usesLeft < 1 {
		return ErrInviteSpent
	}
	return nil
}

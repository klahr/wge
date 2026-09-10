// Package store persists the only state that outlives a container.
//
// Deliberately absent: credentials. A run holds a salt, and every password,
// passphrase and key is re-derived from it on demand. Losing the database
// loses progress; it never leaks a game.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/klahr/wge/internal/creds"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a lookup has no match.
var ErrNotFound = errors.New("not found")

// Store is the engine's database.
type Store struct {
	db *sql.DB
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS players (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	handle     TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);

-- A player is identified by SSH public key before any password is asked for,
-- so the broker knows whose run to derive against.
CREATE TABLE IF NOT EXISTS player_keys (
	fingerprint TEXT    PRIMARY KEY,
	player_id   INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,
	added_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	player_id    INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,
	game_id      TEXT    NOT NULL,
	-- Pinned at start: an author publishing v4 mid-game must not move the
	-- floor under a player who is halfway through v3.
	game_version INTEGER NOT NULL,
	-- The run's entire secret. Everything else is derived.
	salt         TEXT    NOT NULL,
	-- Sticky routing: while a container is live the player returns to the host
	-- holding it. Cleared by the reaper.
	current_host TEXT,
	created_at   INTEGER NOT NULL,
	UNIQUE (player_id, game_id)
);

-- Permission to become a player. The token itself is never stored: a copy of
-- this database must not be a stack of usable invitations.
CREATE TABLE IF NOT EXISTS invites (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	hash       TEXT    NOT NULL UNIQUE,
	note       TEXT    NOT NULL DEFAULT '',
	uses       INTEGER NOT NULL,
	uses_left  INTEGER NOT NULL,
	expires_at INTEGER,
	created_at INTEGER NOT NULL
);

-- Who spent which invitation. Kept when an invitation is revoked, because the
-- record of who came in on it is the reason to keep it.
CREATE TABLE IF NOT EXISTS invite_redemptions (
	invite_id   INTEGER NOT NULL REFERENCES invites(id) ON DELETE CASCADE,
	player_id   INTEGER NOT NULL REFERENCES players(id) ON DELETE CASCADE,
	redeemed_at INTEGER NOT NULL,
	PRIMARY KEY (invite_id, player_id)
);

CREATE TABLE IF NOT EXISTS progress (
	run_id           INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	level_id         TEXT    NOT NULL,
	first_reached_at INTEGER NOT NULL,
	PRIMARY KEY (run_id, level_id)
);
`

// Open opens (and if necessary initialises) the database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// The broker is concurrent but SQLite writes are not; one writer avoids
	// SQLITE_BUSY entirely at the scale this runs at.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialise schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Player is someone who plays. Identity is a set of SSH public keys.
type Player struct {
	ID        int64
	Handle    string
	CreatedAt time.Time
}

// CreatePlayer registers a player and their first public key.
func (s *Store) CreatePlayer(ctx context.Context, handle, fingerprint string) (*Player, error) {
	now := time.Now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	player, err := createPlayerTx(ctx, tx, handle, fingerprint, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return player, nil
}

// createPlayerTx registers a player inside an existing transaction, so that
// redeeming an invitation and creating the player it admits are one operation.
func createPlayerTx(ctx context.Context, tx *sql.Tx, handle, fingerprint string, now time.Time) (*Player, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO players (handle, created_at) VALUES (?, ?)`, handle, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("create player %q: %w", handle, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO player_keys (fingerprint, player_id, added_at) VALUES (?, ?, ?)`,
		fingerprint, id, now.Unix()); err != nil {
		return nil, fmt.Errorf("register key: %w", err)
	}
	return &Player{ID: id, Handle: handle, CreatedAt: now}, nil
}

// PlayerByKey resolves an SSH public key fingerprint to its owner.
func (s *Store) PlayerByKey(ctx context.Context, fingerprint string) (*Player, error) {
	var p Player
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT p.id, p.handle, p.created_at
		   FROM players p JOIN player_keys k ON k.player_id = p.id
		  WHERE k.fingerprint = ?`, fingerprint).Scan(&p.ID, &p.Handle, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0)
	return &p, nil
}

// PlayerByHandle resolves a player's chosen name, for the operator commands
// where a public key is not what somebody has to hand.
func (s *Store) PlayerByHandle(ctx context.Context, handle string) (*Player, error) {
	var p Player
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, handle, created_at FROM players WHERE handle = ?`, handle).
		Scan(&p.ID, &p.Handle, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0)
	return &p, nil
}

// AddKey associates another public key with an existing player.
func (s *Store) AddKey(ctx context.Context, playerID int64, fingerprint string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO player_keys (fingerprint, player_id, added_at) VALUES (?, ?, ?)`,
		fingerprint, playerID, time.Now().Unix())
	return err
}

// Run is one player's playthrough of one game.
type Run struct {
	ID          int64
	PlayerID    int64
	GameID      string
	GameVersion int
	Salt        creds.Salt
	CurrentHost string
	CreatedAt   time.Time
}

// Deriver returns the secret source for this run.
func (r *Run) Deriver() *creds.Deriver { return creds.New(r.Salt, r.GameID) }

// StartRun begins a playthrough, generating the salt that will define every
// credential in it. Version is pinned here for the life of the run.
func (s *Store) StartRun(ctx context.Context, playerID int64, gameID string, gameVersion int) (*Run, error) {
	salt, err := creds.NewSalt()
	if err != nil {
		return nil, err
	}
	now := time.Now()

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (player_id, game_id, game_version, salt, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		playerID, gameID, gameVersion, salt.String(), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("start run of %q: %w", gameID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Run{
		ID: id, PlayerID: playerID, GameID: gameID,
		GameVersion: gameVersion, Salt: salt, CreatedAt: now,
	}, nil
}

// Run returns a player's run of a game.
func (s *Store) Run(ctx context.Context, playerID int64, gameID string) (*Run, error) {
	var r Run
	var salt string
	var host sql.NullString
	var created int64

	err := s.db.QueryRowContext(ctx,
		`SELECT id, player_id, game_id, game_version, salt, current_host, created_at
		   FROM runs WHERE player_id = ? AND game_id = ?`, playerID, gameID).
		Scan(&r.ID, &r.PlayerID, &r.GameID, &r.GameVersion, &salt, &host, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if r.Salt, err = creds.ParseSalt(salt); err != nil {
		return nil, fmt.Errorf("run %d has a corrupt salt: %w", r.ID, err)
	}
	r.CurrentHost = host.String
	r.CreatedAt = time.Unix(created, 0)
	return &r, nil
}

// PlayerRuns returns every run a player has, oldest first.
//
// The front door needs it to resolve a login that names a level rather than a
// game: the account name is unique inside a game but nothing stops two games
// from employing a sysop, so the candidates are every level of that name the
// player's own runs contain, and the credential decides between them.
func (s *Store) PlayerRuns(ctx context.Context, playerID int64) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, player_id, game_id, game_version, salt, current_host, created_at
		   FROM runs WHERE player_id = ? ORDER BY created_at, id`, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Run
	for rows.Next() {
		var r Run
		var salt string
		var host sql.NullString
		var created int64

		if err := rows.Scan(&r.ID, &r.PlayerID, &r.GameID, &r.GameVersion,
			&salt, &host, &created); err != nil {
			return nil, err
		}
		if r.Salt, err = creds.ParseSalt(salt); err != nil {
			return nil, fmt.Errorf("run %d has a corrupt salt: %w", r.ID, err)
		}
		r.CurrentHost = host.String
		r.CreatedAt = time.Unix(created, 0)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ResetRun re-rolls a run's salt, giving the player the same game with entirely
// new answers, and clears their progress. This is the remedy for a spoiled run:
// nobody has to rebuild anything, because the container was only ever a
// function of the salt.
func (s *Store) ResetRun(ctx context.Context, runID int64, gameVersion int) error {
	salt, err := creds.NewSalt()
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET salt = ?, game_version = ?, current_host = NULL WHERE id = ?`,
		salt.String(), gameVersion, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM progress WHERE run_id = ?`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetHost records which machine holds this run's live container, or clears it
// when the reaper takes the container away.
func (s *Store) SetHost(ctx context.Context, runID int64, host string) error {
	var value any
	if host != "" {
		value = host
	}
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET current_host = ? WHERE id = ?`, value, runID)
	return err
}

// ClearHost unpins a run from a machine, but only if that is where it was.
//
// The condition is what makes this safe in a pool. Every machine sweeps what it
// can see, and a machine that collects an old container of a run now living
// elsewhere would otherwise unpin it from the machine that really holds it --
// sending the player to a rebuilt box and leaving their scratch behind.
func (s *Store) ClearHost(ctx context.Context, runID int64, host string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET current_host = NULL WHERE id = ? AND current_host = ?`, runID, host)
	return err
}

// RecordProgress notes that a player has reached a level. It is idempotent: the
// first arrival is the one worth keeping, and a player may pass through a level
// many times.
func (s *Store) RecordProgress(ctx context.Context, runID int64, levelID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO progress (run_id, level_id, first_reached_at) VALUES (?, ?, ?)
		 ON CONFLICT (run_id, level_id) DO NOTHING`,
		runID, levelID, time.Now().Unix())
	return err
}

// Reached is one level a player has arrived at.
type Reached struct {
	LevelID string
	At      time.Time
}

// Progress returns the levels a run has reached, oldest first.
func (s *Store) Progress(ctx context.Context, runID int64) ([]Reached, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT level_id, first_reached_at FROM progress
		  WHERE run_id = ? ORDER BY first_reached_at, level_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Reached
	for rows.Next() {
		var r Reached
		var at int64
		if err := rows.Scan(&r.LevelID, &at); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasReached reports whether a run has already arrived at a level.
func (s *Store) HasReached(ctx context.Context, runID int64, levelID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM progress WHERE run_id = ? AND level_id = ?`, runID, levelID).Scan(&n)
	return n > 0, err
}

// Package creds derives every secret in a game run from a single per-run salt.
//
// The engine's central invariant is that a running container is a pure function
// of (game_version, run_salt). Credentials are therefore never generated and
// stored -- they are re-derived on every container start. A player's passwords
// are stable across sessions (they can resume tomorrow with the notes they took
// today) while differing between players (a published walkthrough is useless).
package creds

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// SaltLen is the size of a run salt in bytes.
const SaltLen = 32

// Salt is the per-run secret. It is the only credential material persisted;
// everything else is derived from it on demand.
type Salt [SaltLen]byte

// NewSalt returns a cryptographically random run salt.
func NewSalt() (Salt, error) {
	var s Salt
	if _, err := rand.Read(s[:]); err != nil {
		return s, fmt.Errorf("generate run salt: %w", err)
	}
	return s, nil
}

func (s Salt) String() string { return hex.EncodeToString(s[:]) }

// ParseSalt decodes a salt previously rendered by String.
func ParseSalt(str string) (Salt, error) {
	var s Salt
	b, err := hex.DecodeString(str)
	if err != nil {
		return s, fmt.Errorf("decode run salt: %w", err)
	}
	if len(b) != SaltLen {
		return s, fmt.Errorf("run salt is %d bytes, want %d", len(b), SaltLen)
	}
	copy(s[:], b)
	return s, nil
}

// Purposes are domain separators. Deriving two different kinds of secret for
// the same level must never yield related values, so every derivation is
// tagged with the purpose it was drawn for.
const (
	purposePassword   = "password"
	purposePassphrase = "passphrase"
	purposeSSHKey     = "ssh-key"
)

// PasswordLen is the character length of a derived password. Long enough that
// guessing is hopeless, short enough to copy off a screen by hand.
const PasswordLen = 24

// alphabet is base62. Derived passwords should look like the high-entropy
// strings that turn up in real config files, so ambiguous characters are kept
// rather than stripped -- realism outranks transcription comfort here, and
// players copy/paste anyway.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Deriver produces the secrets for one run of one game.
type Deriver struct {
	salt   Salt
	gameID string
}

// New returns a Deriver for a run of gameID under salt.
func New(salt Salt, gameID string) *Deriver {
	return &Deriver{salt: salt, gameID: gameID}
}

// Password returns the login password for levelID.
func (d *Deriver) Password(levelID string) string {
	return encode(d.stream(purposePassword, levelID, PasswordLen), PasswordLen)
}

// Passphrase returns the passphrase protecting levelID's SSH key. Split
// credentials -- key in one level, passphrase in another -- are how a level
// comes to have two parents in the graph.
func (d *Deriver) Passphrase(levelID string) string {
	return encode(d.stream(purposePassphrase, levelID, PasswordLen), PasswordLen)
}

// SSHKey returns levelID's ed25519 private key, derived deterministically so
// that it too survives a container rebuild.
func (d *Deriver) SSHKey(levelID string) ed25519.PrivateKey {
	seed := d.Secret(purposeSSHKey, levelID, ed25519.SeedSize)
	return ed25519.NewKeyFromSeed(seed)
}

// Secret returns n bytes of key material for an arbitrary purpose, letting
// games derive their own stable secrets (API tokens, database passwords)
// without reaching for randomness that would not survive a rebuild.
func (d *Deriver) Secret(purpose, name string, n int) []byte {
	return d.stream(purpose, name, n)
}

// Match reports which of levels the submitted password belongs to. The broker
// uses it to turn an SSH password prompt into a level selection: the credential
// a player found is literally their login.
//
// Every candidate is compared in constant time and the loop is never short
// circuited, so neither the matched level nor the failure is distinguishable by
// timing.
func (d *Deriver) Match(levels []string, submitted string) (string, bool) {
	var found string
	var ok bool
	for _, id := range levels {
		want := d.Password(id)
		if subtle.ConstantTimeCompare([]byte(want), []byte(submitted)) == 1 {
			found, ok = id, true
		}
	}
	return found, ok
}

// stream expands the salt into n bytes bound to (purpose, gameID, name).
//
// The unit separator is not valid in any identifier the manifest accepts, so
// the concatenation is unambiguous and no pair of distinct inputs can collide.
func (d *Deriver) stream(purpose, name string, n int) []byte {
	info := []byte(purpose + "\x1f" + d.gameID + "\x1f" + name + "\x1f")

	out := make([]byte, 0, n+sha256.Size)
	var counter [4]byte
	for i := uint32(0); len(out) < n; i++ {
		binary.BigEndian.PutUint32(counter[:], i)
		mac := hmac.New(sha256.New, d.salt[:])
		mac.Write(info)
		mac.Write(counter[:])
		out = mac.Sum(out)
	}
	return out[:n]
}

// encode maps key material onto the password alphabet.
//
// Rejection sampling rather than a plain modulo: 256 is not a multiple of 62,
// so folding every byte would make the first few characters measurably more
// likely than the rest. Bytes at or above the largest usable multiple are
// discarded and the stream is extended as needed.
func encode(seed []byte, n int) string {
	const limit = byte(256 - (256 % len(alphabet))) // 248

	out := make([]byte, 0, n)
	buf := seed
	for round := 1; len(out) < n; round++ {
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				return string(out)
			}
		}
		// Vanishingly unlikely (p < 0.04 per byte), but the loop must terminate
		// with a full-length password rather than a short one.
		buf = extend(seed, round)
	}
	return string(out)
}

func extend(seed []byte, round int) []byte {
	var counter [4]byte
	binary.BigEndian.PutUint32(counter[:], uint32(round))
	sum := sha256.Sum256(append(append([]byte{}, seed...), counter[:]...))
	return sum[:]
}

package creds

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func testSalt(b byte) Salt {
	var s Salt
	for i := range s {
		s[i] = b
	}
	return s
}

// The resume property: a player who writes down a password today must be able
// to use it tomorrow, against a container that has been destroyed and rebuilt
// in between.
func TestPasswordIsStableAcrossRebuilds(t *testing.T) {
	salt := testSalt(0x01)

	today := New(salt, "heist").Password("backup-op")
	tomorrow := New(salt, "heist").Password("backup-op")

	if today != tomorrow {
		t.Fatalf("password changed across rebuild: %q then %q", today, tomorrow)
	}
}

// The anti-walkthrough property: the same level in the same game must yield a
// different password for a different run.
func TestPasswordDiffersPerRun(t *testing.T) {
	a := New(testSalt(0x01), "heist").Password("backup-op")
	b := New(testSalt(0x02), "heist").Password("backup-op")

	if a == b {
		t.Fatal("two runs derived the same password; a walkthrough would spoil both")
	}
}

// Domain separation: nothing about one secret may be recoverable from another
// drawn against the same salt.
func TestSecretsAreDomainSeparated(t *testing.T) {
	d := New(testSalt(0x03), "heist")

	seen := map[string]string{
		"password/a":   d.Password("a"),
		"password/b":   d.Password("b"),
		"passphrase/a": d.Passphrase("a"),
		"passphrase/b": d.Passphrase("b"),
	}
	// A different game must not reuse another game's secrets under the same salt.
	seen["other-game/password/a"] = New(testSalt(0x03), "other").Password("a")

	inverse := make(map[string]string, len(seen))
	for label, secret := range seen {
		if prior, dup := inverse[secret]; dup {
			t.Errorf("%s and %s derived the same secret", prior, label)
		}
		inverse[secret] = label
	}
}

// Identifiers are concatenated to build the derivation info string. Distinct
// level names must not be able to collide by shifting the separator.
func TestAdjacentIdentifiersDoNotCollide(t *testing.T) {
	d := New(testSalt(0x04), "heist")

	if d.Password("ab") == d.Password("a\x1fb") {
		t.Fatal("level identifiers collided across the separator")
	}
}

func TestPasswordShape(t *testing.T) {
	d := New(testSalt(0x05), "heist")

	for _, level := range []string{"mailroom", "backup-op", "sysadmin"} {
		got := d.Password(level)
		if len(got) != PasswordLen {
			t.Errorf("%s: password is %d chars, want %d", level, len(got), PasswordLen)
		}
		if i := strings.IndexFunc(got, func(r rune) bool {
			return !strings.ContainsRune(alphabet, r)
		}); i >= 0 {
			t.Errorf("%s: password %q contains out-of-alphabet byte at %d", level, got, i)
		}
	}
}

// Rejection sampling exists to keep the alphabet uniform. A modulo fold would
// over-represent the first 256%62 = 8 characters by ~25%; this asserts the bias
// is not present across a large sample.
func TestAlphabetIsNotBiased(t *testing.T) {
	counts := make(map[rune]int)
	const runs = 4000

	for i := 0; i < runs; i++ {
		salt := testSalt(byte(i % 256))
		for _, r := range New(salt, "heist").Password(string(rune('a' + i%26))) {
			counts[r]++
		}
	}

	if len(counts) != len(alphabet) {
		t.Fatalf("sample covered %d of %d alphabet characters", len(counts), len(alphabet))
	}

	total := runs * PasswordLen
	expect := float64(total) / float64(len(alphabet))
	for r, n := range counts {
		if ratio := float64(n) / expect; ratio < 0.85 || ratio > 1.15 {
			t.Errorf("character %q appeared %d times, %.2fx expected", r, n, ratio)
		}
	}
}

func TestMatchSelectsTheOwningLevel(t *testing.T) {
	d := New(testSalt(0x06), "heist")
	levels := []string{"mailroom", "backup-op", "sysadmin"}

	for _, want := range levels {
		got, ok := d.Match(levels, d.Password(want))
		if !ok || got != want {
			t.Errorf("password for %s matched (%q, %v)", want, got, ok)
		}
	}

	if got, ok := d.Match(levels, "not-a-password"); ok {
		t.Errorf("unknown password matched level %q", got)
	}
	// A password from another run must not open this one.
	other := New(testSalt(0x07), "heist").Password("sysadmin")
	if got, ok := d.Match(levels, other); ok {
		t.Errorf("another run's password matched level %q", got)
	}
}

func TestSSHKeyIsStableAndValid(t *testing.T) {
	salt := testSalt(0x08)

	key := New(salt, "heist").SSHKey("sysadmin")
	again := New(salt, "heist").SSHKey("sysadmin")

	if !key.Equal(again) {
		t.Fatal("ssh key changed across rebuild")
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("key is %d bytes, want %d", len(key), ed25519.PrivateKeySize)
	}

	msg := []byte("wge")
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), msg, ed25519.Sign(key, msg)) {
		t.Fatal("derived key does not verify its own signature")
	}
}

func TestSaltRoundTrip(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}

	got, err := ParseSalt(salt.String())
	if err != nil {
		t.Fatalf("ParseSalt: %v", err)
	}
	if got != salt {
		t.Fatal("salt did not survive a round trip")
	}

	if _, err := ParseSalt("abcd"); err == nil {
		t.Error("ParseSalt accepted a short salt")
	}
	if _, err := ParseSalt("zz"); err == nil {
		t.Error("ParseSalt accepted non-hex input")
	}
}

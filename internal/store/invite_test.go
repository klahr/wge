package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func newToken(t *testing.T) string {
	t.Helper()
	token, err := NewInviteToken()
	if err != nil {
		t.Fatalf("NewInviteToken: %v", err)
	}
	return token
}

// The code is read off a screen and typed in by hand, so it avoids the
// characters people confuse and is grouped for reading aloud.
func TestInviteTokensAreReadable(t *testing.T) {
	seen := map[string]bool{}

	for i := 0; i < 200; i++ {
		token := newToken(t)
		if seen[token] {
			t.Fatalf("NewInviteToken repeated itself: %q", token)
		}
		seen[token] = true

		groups := strings.Split(token, "-")
		if len(groups) != 4 {
			t.Fatalf("token %q is not four groups", token)
		}
		for _, g := range groups {
			if len(g) != 4 {
				t.Fatalf("token %q has an odd group", token)
			}
		}
		for _, r := range strings.ReplaceAll(token, "-", "") {
			if !strings.ContainsRune(inviteAlphabet, r) {
				t.Fatalf("token %q contains %q, which is easy to mistype", token, r)
			}
		}
	}
}

// Whatever somebody types around the code, the code is the code.
func TestTokensAreNormalisedOnTheWayIn(t *testing.T) {
	s, ctx := open(t)
	token := newToken(t)
	if _, err := s.CreateInvite(ctx, token, 1, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}

	bare := strings.ReplaceAll(token, "-", "")
	for _, typed := range []string{
		token,
		bare,
		strings.ToLower(token),
		" " + token + " ",
		strings.ReplaceAll(token, "-", " "),
	} {
		if err := s.CheckInvite(ctx, typed); err != nil {
			t.Errorf("CheckInvite(%q) = %v", typed, err)
		}
	}
}

// A copy of the database must not be a stack of usable invitations.
func TestTheTokenIsNotStored(t *testing.T) {
	s, ctx := open(t)
	token := newToken(t)
	if _, err := s.CreateInvite(ctx, token, 1, time.Time{}, "workshop"); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT hash FROM invites`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, strings.ReplaceAll(token, "-", "")) {
		t.Fatal("the invitation code itself is in the database")
	}
}

func TestRedeemingSpendsAUse(t *testing.T) {
	s, ctx := open(t)
	token := newToken(t)
	if _, err := s.CreateInvite(ctx, token, 2, time.Time{}, "pair"); err != nil {
		t.Fatal(err)
	}

	first, err := s.RedeemInvite(ctx, token, "rook", "SHA256:aaa")
	if err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if _, err := s.RedeemInvite(ctx, token, "vega", "SHA256:bbb"); err != nil {
		t.Fatalf("second redemption: %v", err)
	}

	if _, err := s.RedeemInvite(ctx, token, "nyx", "SHA256:ccc"); !errors.Is(err, ErrInviteSpent) {
		t.Fatalf("third redemption: got %v, want ErrInviteSpent", err)
	}

	// The player really was created, and by that invitation.
	got, err := s.PlayerByHandle(ctx, "rook")
	if err != nil || got.ID != first.ID {
		t.Fatalf("PlayerByHandle: %+v %v", got, err)
	}
}

// A failure must not cost somebody their place: the use and the player are one
// transaction.
func TestAFailedEnrolmentDoesNotSpendTheInvitation(t *testing.T) {
	s, ctx := open(t)
	token := newToken(t)
	if _, err := s.CreateInvite(ctx, token, 1, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePlayer(ctx, "rook", "SHA256:aaa"); err != nil {
		t.Fatal(err)
	}

	// The handle is taken, so the player cannot be created.
	if _, err := s.RedeemInvite(ctx, token, "rook", "SHA256:bbb"); err == nil {
		t.Fatal("expected a duplicate handle to fail")
	}

	// And the invitation is untouched.
	if err := s.CheckInvite(ctx, token); err != nil {
		t.Fatalf("the invitation was spent by a failed enrolment: %v", err)
	}
}

func TestUnknownAndExpiredInvitations(t *testing.T) {
	s, ctx := open(t)

	if err := s.CheckInvite(ctx, "ABCD-EFGH-JKMN-PQRS"); !errors.Is(err, ErrInviteUnknown) {
		t.Fatalf("unknown code: got %v", err)
	}

	past := newToken(t)
	if _, err := s.CreateInvite(ctx, past, 1, time.Now().Add(-time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckInvite(ctx, past); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("expired code: got %v", err)
	}
	if _, err := s.RedeemInvite(ctx, past, "rook", "SHA256:aaa"); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("redeeming an expired code: got %v", err)
	}

	future := newToken(t)
	if _, err := s.CreateInvite(ctx, future, 1, time.Now().Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckInvite(ctx, future); err != nil {
		t.Fatalf("a code that has not expired: %v", err)
	}
}

// Two people racing for the last use of a workshop code must not both get it.
func TestTheLastUseGoesToExactlyOnePerson(t *testing.T) {
	s, ctx := open(t)
	token := newToken(t)
	if _, err := s.CreateInvite(ctx, token, 1, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var admitted int

	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.RedeemInvite(ctx, token,
				fmt.Sprintf("player%d", i), fmt.Sprintf("SHA256:%d", i))
			if err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if admitted != 1 {
		t.Fatalf("%d people redeemed a one-use invitation", admitted)
	}
}

// Revoking keeps the record of who came in on it, which is the reason to keep
// the row at all.
func TestRevokingSpendsWithoutForgetting(t *testing.T) {
	s, ctx := open(t)
	token := newToken(t)
	invite, err := s.CreateInvite(ctx, token, 5, time.Time{}, "class of 26")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, token, "rook", "SHA256:aaa"); err != nil {
		t.Fatal(err)
	}

	if err := s.RevokeInvite(ctx, invite.ID); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if err := s.CheckInvite(ctx, token); !errors.Is(err, ErrInviteSpent) {
		t.Fatalf("after revoking: got %v", err)
	}

	var redemptions int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM invite_redemptions WHERE invite_id = ?`, invite.ID).
		Scan(&redemptions); err != nil {
		t.Fatal(err)
	}
	if redemptions != 1 {
		t.Fatalf("revoking lost the record of who used it (%d rows)", redemptions)
	}

	if err := s.RevokeInvite(ctx, 9999); !errors.Is(err, ErrInviteUnknown) {
		t.Fatalf("revoking a missing invitation: got %v", err)
	}
}

func TestInviteListing(t *testing.T) {
	s, ctx := open(t)
	for _, note := range []string{"first", "second"} {
		if _, err := s.CreateInvite(ctx, newToken(t), 3, time.Time{}, note); err != nil {
			t.Fatal(err)
		}
	}

	invites, err := s.Invites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(invites) != 2 {
		t.Fatalf("listed %d invitations", len(invites))
	}
	for _, i := range invites {
		if i.Uses != 3 || i.UsesLeft != 3 {
			t.Errorf("invitation %d: uses %d/%d", i.ID, i.UsesLeft, i.Uses)
		}
	}

	if _, err := s.CreateInvite(ctx, newToken(t), 0, time.Time{}, ""); err == nil {
		t.Error("an invitation with no uses should be refused")
	}
}

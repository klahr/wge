package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/store"
)

// cmdInvite manages the invitations that let somebody become a player.
//
// A server nobody has to be invited to is a server anybody can fill, so
// enrolment is closed unless `serve -open` says otherwise, and this is how
// people are let in.
func cmdInvite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	dbPath := fs.String("db", "wge.db", "path to the engine database")
	uses := fs.Int("uses", 1, "how many people may enrol with this invitation")
	expires := fs.String("expires", "", "how long the invitation lasts, e.g. 7d (default: forever)")
	note := fs.String("note", "", "what this invitation is for; shown in the listing")
	list := fs.Bool("list", false, "show the invitations instead of creating one")
	revoke := fs.Int64("revoke", 0, "spend an invitation's remaining uses, by id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyEnv(fs); err != nil {
		return err
	}

	ctx := context.Background()
	st, err := openStore(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	switch {
	case *list:
		return listInvites(ctx, st)
	case *revoke != 0:
		if err := st.RevokeInvite(ctx, *revoke); err != nil {
			if errors.Is(err, store.ErrInviteUnknown) {
				return fmt.Errorf("no invitation %d", *revoke)
			}
			return err
		}
		fmt.Printf("invitation %d revoked\n", *revoke)
		return nil
	}

	var expiresAt time.Time
	if *expires != "" {
		d, err := manifest.ParseDuration(*expires)
		if err != nil {
			return err
		}
		expiresAt = time.Now().Add(d)
	}

	token, err := store.NewInviteToken()
	if err != nil {
		return err
	}
	invite, err := st.CreateInvite(ctx, token, *uses, expiresAt, *note)
	if err != nil {
		return err
	}

	// Printed once. The database holds only a hash, so an invitation that is
	// lost is replaced rather than recovered.
	fmt.Printf("%s\n", token)
	fmt.Printf("  invitation %d, %s\n", invite.ID, plural(*uses, "use"))
	if !expiresAt.IsZero() {
		fmt.Printf("  expires %s\n", expiresAt.Format(time.RFC1123))
	}
	fmt.Printf("  redeem with: ssh enroll@<this host>\n")
	return nil
}

func listInvites(ctx context.Context, st *store.Store) error {
	invites, err := st.Invites(ctx)
	if err != nil {
		return err
	}
	if len(invites) == 0 {
		fmt.Println("no invitations")
		return nil
	}

	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tUSES\tSTATE\tEXPIRES\tCREATED\tNOTE")

	for _, i := range invites {
		state := "open"
		switch {
		case i.Expired(now):
			state = "expired"
		case i.UsesLeft < 1:
			state = "spent"
		}

		expiry := "-"
		if !i.ExpiresAt.IsZero() {
			expiry = i.ExpiresAt.Format("2006-01-02")
		}

		fmt.Fprintf(w, "%d\t%d/%d\t%s\t%s\t%s\t%s\n",
			i.ID, i.UsesLeft, i.Uses, state, expiry,
			i.CreatedAt.Format("2006-01-02"), i.Note)
	}
	return w.Flush()
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

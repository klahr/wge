package build

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/klahr/wge/internal/manifest"
)

// account is a Unix user the image will carry.
type account struct {
	User     string
	UID, GID int
	Home     string
	Shell    string
	Gecos    string
	// Role is what the manifest asked for, if it asked for anything.
	Role Role

	// Level is set for the accounts that are levels; the rest are decoys and
	// service accounts.
	Level   *manifest.Level
	Service bool
	Noise   bool
}

// Role is the flavour of an account, used to choose plausible shell history and
// mail for it.
type Role string

const (
	RoleMail   Role = "mail"
	RoleBackup Role = "backup"
	RoleOps    Role = "ops"
	RoleAdmin  Role = "admin"
	RolePlain  Role = "plain"
)

// planAccounts decides every account on one machine: the levels that live
// here, the services they declare, and the decoys.
//
// Scoping to the host matters. A level's home directory is laid down for its
// account, so an account that exists on every machine puts that level's files
// on every machine -- which on a multi-host game means the final level's payoff
// sitting on the box the player starts from. Permissions still hide it, but
// content that has no business being there is one chmod away from being a leak,
// and a player who finds a document store carrying the mailroom's notes has
// learned the machines are not really separate.
//
// Decoys stay everywhere: the same company's staff plausibly have accounts on
// both machines.
func planAccounts(g *manifest.Game, host string) ([]*account, error) {
	var accounts []*account
	taken := map[string]bool{}
	uids := newUIDPool()

	for _, l := range g.Levels {
		if l.Host != host {
			continue
		}
		if taken[l.User] {
			return nil, fmt.Errorf("account %q is claimed twice", l.User)
		}
		taken[l.User] = true

		uid := uids.assign(l.User, 1000)
		accounts = append(accounts, &account{
			User: l.User, UID: uid, GID: uid,
			Home:  "/home/" + l.User,
			Shell: "/bin/bash",
			Gecos: l.Name,
			Role:  Role(l.Role),
			Level: l,
		})
	}

	for _, l := range g.Levels {
		if l.Host != host {
			continue
		}
		for _, s := range l.Services {
			if taken[s.User] {
				continue
			}
			taken[s.User] = true

			// Service accounts live in the system range and cannot log in.
			// A daemon account with a shell is a gift to whoever compromises it.
			uid := uids.assign(s.User, 150)
			accounts = append(accounts, &account{
				User: s.User, UID: uid, GID: uid,
				Home:    "/var/lib/" + s.User,
				Shell:   "/usr/sbin/nologin",
				Gecos:   s.Name + " service",
				Service: true,
			})
		}
	}

	for _, st := range g.Staff {
		if taken[st.User] {
			continue
		}
		// Staff can be pinned to one machine; by default they work for the
		// whole company and have an account on all of them.
		if st.Host != "" && st.Host != host {
			continue
		}
		taken[st.User] = true

		uid := uids.assign(st.User, 1000)
		accounts = append(accounts, &account{
			User: st.User, UID: uid, GID: uid,
			Home:  "/home/" + st.User,
			Shell: "/bin/bash",
			Gecos: st.Name,
			Role:  Role(st.Role),
			Noise: true,
		})
	}

	sort.Slice(accounts, func(i, j int) bool { return accounts[i].UID < accounts[j].UID })
	return accounts, nil
}

// role is what an account does, which decides the flavour of the shell history
// and mail generated for it.
//
// The manifest has the last word. Where it says nothing, a level takes its cue
// from what it runs, because that is a better guess than none.
func (a *account) role() Role {
	if a.Role != "" {
		return a.Role
	}
	if a.Service {
		return RolePlain
	}
	if a.Level != nil {
		if len(a.Level.Grants) == 0 {
			return RoleAdmin // the final level is somebody with the keys
		}
		for _, s := range a.Level.Services {
			if s.Port != 0 {
				return RoleOps
			}
		}
		return RolePlain
	}
	return RolePlain
}

// uidPool hands out stable, non-contiguous uids.
//
// Sequential uids from 1000 upward in exactly the order the manifest lists its
// levels is a pattern; real machines accumulate accounts with gaps where people
// have come and gone. The assignment is hashed so it is stable across builds.
type uidPool struct {
	used map[int]bool
}

func newUIDPool() *uidPool { return &uidPool{used: map[int]bool{}} }

func (p *uidPool) assign(user string, base int) int {
	sum := sha256.Sum256([]byte("uid\x1f" + user))
	span := 800
	uid := base + int(binary.BigEndian.Uint32(sum[:4])%uint32(span))

	for p.used[uid] {
		uid++
	}
	p.used[uid] = true
	return uid
}

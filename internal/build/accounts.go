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

// noiseNames is the pool decoy accounts are drawn from.
//
// Decoys are not decoration. A box whose /home holds exactly as many
// directories as the game has levels has handed over its own structure; the
// first thing a player does is `ls /home`, and it should not be a table of
// contents.
var noiseNames = []struct {
	User, Gecos string
	Role        Role
}{
	{"aringdal", "Astrid Ringdal", RolePlain},
	{"pkoskela", "Petri Koskela", RoleOps},
	{"mhalvorsen", "Mette Halvorsen", RolePlain},
	{"tsaarinen", "Tuomas Saarinen", RoleBackup},
	{"lbergqvist", "Lova Bergqvist", RolePlain},
	{"hnyberg", "Henrik Nyberg", RoleOps},
	{"iolsen", "Ingrid Olsen", RolePlain},
	{"vlindqvist", "Viktor Lindqvist", RoleAdmin},
	{"skallio", "Sanna Kallio", RolePlain},
	{"ejakobsen", "Erik Jakobsen", RoleBackup},
	{"nfredriksen", "Nora Fredriksen", RolePlain},
	{"ostrand", "Olle Strand", RoleOps},
}

// planAccounts decides every account on the box: the levels, the services they
// declare, and the decoys.
func planAccounts(g *manifest.Game) ([]*account, error) {
	var accounts []*account
	taken := map[string]bool{}
	uids := newUIDPool()

	for _, l := range g.Levels {
		if taken[l.User] {
			return nil, fmt.Errorf("account %q is claimed twice", l.User)
		}
		taken[l.User] = true

		uid := uids.assign(l.User, 1000)
		accounts = append(accounts, &account{
			User: l.User, UID: uid, GID: uid,
			Home:  "/home/" + l.User,
			Shell: "/bin/bash",
			Gecos: gecosFor(l.User),
			Level: l,
		})
	}

	for _, l := range g.Levels {
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

	for i := 0; i < g.NoiseUsers && i < len(noiseNames); i++ {
		n := noiseNames[i]
		if taken[n.User] {
			continue
		}
		taken[n.User] = true

		uid := uids.assign(n.User, 1000)
		accounts = append(accounts, &account{
			User: n.User, UID: uid, GID: uid,
			Home:  "/home/" + n.User,
			Shell: "/bin/bash",
			Gecos: n.Gecos,
			Noise: true,
		})
	}

	if g.NoiseUsers > len(noiseNames) {
		return nil, fmt.Errorf("noise_users is %d but only %d decoy identities are available",
			g.NoiseUsers, len(noiseNames))
	}

	sort.Slice(accounts, func(i, j int) bool { return accounts[i].UID < accounts[j].UID })
	return accounts, nil
}

// role guesses what an account does, so its generated history and mail match
// the job. Level accounts take their cue from the services they run.
func (a *account) role() Role {
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
	for _, n := range noiseNames {
		if n.User == a.User {
			return n.Role
		}
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

// gecosFor invents a full name for a level account from its username, so the
// passwd file reads like a company's rather than a puzzle's.
func gecosFor(user string) string {
	for _, n := range noiseNames {
		if n.User == user {
			return n.Gecos
		}
	}
	if name, ok := levelGecos[user]; ok {
		return name
	}
	return ""
}

// levelGecos names the accounts the example game uses. Authors can override any
// of this from a level's setup.sh.
var levelGecos = map[string]string{
	"jposti":     "Janne Posti",
	"bkup":       "Backup operator",
	"oncall":     "Operations on-call",
	"dsundqvist": "Daniel Sundqvist",
}

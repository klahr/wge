# wge

An engine for hacking simulators played over SSH.

Each game is a Linux box. Each level is a real user account on it. Progress is
made the way it would be made on a real system — `ls`, `find`, `sudo -l`,
file permissions, reading somebody else's mail — and each level ends in the
credentials for the next. There is no flag to submit and no scoring menu,
because the game mechanic and the operating system are the same thing.

## The invariant

> **A container is a pure function of `(game_version, run_salt)`.**

Everything else follows from it. Credentials are never generated and stored;
they are *derived* from the run's salt every time a container starts. So:

- **Resume works.** The password a player wrote down on Monday is the password
  the rebuilt container derives on Tuesday.
- **Walkthroughs don't.** Another player's salt yields entirely different
  answers to the same puzzles.
- **Containers are disposable.** Reaping an idle box, migrating a run to
  another host, or recovering from a crash all reduce to rebuilding from two
  values in a database row.
- **Support is possible.** A player who lost their notes can be helped, because
  the engine can re-derive what it never stored.

The first feature that breaks this invariant costs all four at once.

## How a login works

The credential a player found *is* their login:

```
ssh heist@game.example.com
```

1. Their **public key** identifies the player and resolves their run.
2. The server returns a partial success and asks for a **password**.
3. That password is matched against every level's derived password for
   *their* run. It selects the level they land on.

A player who has found level 6's password logs straight in as level 6. Inside
the container all levels exist as real accounts, so `su` works too — both paths
are legitimate.

Containers do not run `sshd`. One broker terminates every connection and
attaches the player to a container it creates on demand, which is what makes
lazy creation, idle reaping, progress tracking and session recording possible.

## Authoring a game

A game is a directory:

```
games/heist/
  game.yaml              # image, packages, fictional timeline, noise users
  levels/
    01-mailroom/
      level.yaml         # user, prerequisites, the credential it grants
      home/              # file tree copied into the level user's home
      files/             # artifacts elsewhere: files/var/backups/... -> /var/backups/...
      mail/              # messages delivered to the level user's mailbox
      setup.sh           # permissions, services, cron
```

Any file containing `{{` is a template, rendered per run:

```
The operator account is:

    user: {{ .User "backup-op" }}
    pass: {{ .Password "backup-op" }}
```

Available: `.Password`, `.Passphrase`, `.PrivateKey`, `.UnencryptedPrivateKey`,
`.PublicKey`, `.User`, `.Handle`. A missing level id is a build error, not an
empty string -- a credential that silently renders as nothing is a level nobody
can solve.

Levels form a DAG, not a chain. A level with two prerequisites is gated on a
**split credential** — an SSH key found on one level, its passphrase on
another:

```yaml
# levels/04-sysadmin/level.yaml
id: sysadmin
user: dsundqvist
requires: [backup-op, ops-oncall]   # key from one, passphrase from the other
```

`grants.placed_in` is not documentation. It names where the credential
actually sits, and the validator reads it as that level's user to prove the
level is solvable.

### The static validator

Permission mistakes are the main way an authored game silently breaks, and they
are mechanically checkable. `wge validate` rejects, among others:

- a prerequisite that never grants anything (the level is unreachable)
- a credential granted along an edge the graph does not declare
- two parents handing over the same kind of credential (one isn't needed)
- a passphrase with no key to unlock
- cycles, orphans, a game with no ending
- two levels sharing a Unix account
- a service running as root, or sharing a level's account

Every fault is reported in one pass.

### The permission test pass

`wge test` answers the question the manifest cannot: whether the filesystem the
build actually produced enforces the graph the manifest describes. It boots the
image, seeds it with a throwaway run, and then — as each level's own uid —
attempts the reads. Nothing here reasons about modes and groups; a permission
model derived from the manifest would only ever agree with itself.

- **Solvability.** Every credential a level grants must be readable by that
  level, or everything after it is unreachable.
- **Ownership.** Nothing belonging to a level a player has not reached may be
  readable from where they stand.
- **Credential sweep.** Every file the level can read is searched for the
  credentials it should not hold. This is the check that matters most: it
  assumes nothing about where a credential stays, and catches a password copied
  into a log, left in a backup, or sitting in a config nothing in the manifest
  mentions.
- **Setuid audit.** `find / -perm -4000` is the first thing a competent player
  runs, and it should return exactly what the author intended. The expected set
  is the base image's own, diffed at test time, so the check cannot go stale.

The verifier's own tests build deliberately broken games and assert each check
fires. A permission check that cannot detect a leak is worse than none: it says
the game is safe to serve.

## The image pipeline

Building a game splits in two, and the split is forced by the invariant. An
image is shared by every run of a game version, so it can never contain a
password: it holds unrendered templates. Rendering happens **per run**, against
a container, in the window between the container starting and the player being
let in -- files uploaded as a tar, passwords piped to `chpasswd`. Nothing is
written to disk on the way, and the image needs no tooling of its own, which
matters: a `/usr/local/bin/wge-seed` would be the loudest thing on the box.

### The aging pass

A freshly built image stamps every file with the build time, and `ls -la` gives
that away in about four seconds. This is where realism is actually won:

- **Stratified mtimes.** Timestamps are hashed from paths, not drawn from a
  random stream, so they are reproducible across rebuilds and stable under
  edits -- adding one file does not re-age the rest of the game. Dotfiles land
  on the account's creation date, working files cluster around a per-directory
  moment, and a deliberate minority are much older than their neighbours.
- **Shell history that stops mid-thought.** Timestamped, flavoured by what the
  account does, ending on a half-typed command.
- **A login history the box remembers.** `last` and `lastlog` in both the
  classic binary format and the SQLite one Debian 13 replaced it with, plus a
  matching `/var/log/auth.log` -- all rendered from one list of sessions, so a
  player who cross-checks them finds one story rather than two.
- **Decoy accounts.** A `/home` with exactly as many directories as the game has
  levels has handed over its own structure.

The plan is applied in two phases around the level setup scripts, because
`COPY` discards tar ownership and every later step re-stamps mtimes: ownership
before `setup.sh` so it can build on it, timestamps last so nothing undoes them.
Permissions belong to `setup.sh` alone — that is where a game's access control
is expressed, so seeding reads the mode a file already has rather than imposing
the author's on-disk one.

Two details are worth knowing before touching this code:

- **The package manager's logs are the loudest tell on the box.**
  `/var/log/apt/history.log` keeps the literal `apt-get install` command line —
  which reads like a game engine's recipe — and `dpkg.log` dates every install
  to the minute the image was built. Both are purged.
- **Docker's layer diff drops a directory whose only change is its mtime.** A
  build-time sweep silently loses every empty directory it stamps, so the game's
  own directories survive (their contents changed in the same layer) while
  `/opt`, `/mnt` and a couple of hundred others keep the base image's build
  date. Directories are therefore swept again at container start, where there is
  no layer diff to lose them.

The result is a box with no clock leak: nothing on it is dated after the
fictional present, including the files the container runtime writes at startup.

## Commands

```
wge validate <game-dir>    check a manifest and its level graph
wge graph    <game-dir>    print the level graph and the credentials along it
wge creds    <game-dir>    re-derive a run's credentials (support tool)
wge base     <base-dir>    build a base image games are built on
wge build    <game-dir>    compile a game into a container image
wge test     <game-dir>    verify a built image enforces its level graph
wge serve                  run the SSH front door
```

To play the example game:

```
wge base bases/debian-13
wge build games/heist
wge test games/heist
wge serve

ssh enroll@localhost -p 2222      # register, and collect the first password
ssh heist@localhost -p 2222       # play
```

## Sandboxing

Players are invited to attack the box, so the container boundary is the only
boundary that matters. Every game container runs with capabilities dropped, no
new privileges, no network egress, and caps on memory, CPU and PIDs.

One counter-intuitive detail: dropping *all* capabilities is wrong here. With
an empty set, root inside the container cannot **drop** privilege either. `su`
fails with `cannot set groups`, breaking the move between levels, and `chpasswd`
cannot write `/etc/shadow` because it cannot chown its replacement to
`root:shadow` — so no run's passwords ever get set.

The reasoning is therefore inverted from the usual: the set is what uid 0 needs
to administer its own users and files, none of which helps a player who is not
already root. `SETUID`, `SETGID`, `CHOWN`, `FOWNER`, `FSETID`, `DAC_OVERRIDE`,
`KILL`, `AUDIT_WRITE`, `NET_BIND_SERVICE`. What stays dropped is what gets a
player off the box or onto the network: `SYS_ADMIN`, `SYS_PTRACE`, `NET_ADMIN`,
`NET_RAW`, `MKNOD`, `SETFCAP` and the rest.

## Status

Built and tested:

| | |
|---|---|
| `internal/creds` | credential derivation — the invariant |
| `internal/manifest` | authoring format, loader, static validator |
| `internal/store` | players, runs, progress (no credentials) |
| `internal/broker` | SSH front door and the auth chain |
| `internal/runtime` | container lifecycle over the Docker Engine API |
| `internal/library` | game loading and image naming |
| `internal/build` | the image pipeline, the aging pass, seeding, and verification |
| `internal/docker` | a small Engine API client |

The example game is playable end to end: four levels, a DAG whose final level
is gated on a split credential — an encrypted SSH key recovered from a staged
backup on one level, its passphrase found on another.

Next, roughly in order:

1. **Credentials inside archives.** `placed_in` accepts an archive member, but
   rendering into one means repacking it during seeding. Until it does, the
   validator rejects the syntax rather than producing a game whose credential
   never appears.
2. **Session recording and the hint engine.** The broker proxies every byte, so
   it already knows whether a player is circling or stalled — no in-container
   agent to find or tamper with. Hints arrive as mail from an in-fiction
   correspondent, escalating in tiers, and unprompted when a player stalls.
3. **Services and cron.** Declared in the manifest and validated, but not yet
   installed or started by the build; PID 1 is `sleep infinity`.
4. **The reaper**, scratch-volume persistence, and multi-host runs.

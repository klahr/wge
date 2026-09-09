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

Levels declare the services and scheduled work that belong to them:

```yaml
services:
  - name: backup-agent
    user: bkupd                       # never root, never a level's account
    description: Ardent nightly backup agent
    exec: /usr/local/lib/backup-agent/agent
    port: 8377

cron:
  - schedule: "17 2 * * *"
    comment: stage last night's restore for review
    command: /usr/local/bin/stage-nightly
```

Each service becomes a real LSB init script wired into the runlevels, so
`service backup-agent status`, `/etc/init.d/backup-agent restart` and the boot
sequence all behave as a player expects. Cron entries land in `/etc/cron.d`,
where they are visible and readable — a scheduled job is a puzzle surface in
its own right, and a writable script run by somebody else's crontab is a
classic.

A `setup.sh` at the root of a game directory configures the machine itself, for
the things that belong to no single level.

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

`serve` takes `-grace` (how long a container outlives its last session, default
15m), `-sweep` (how often abandoned containers are collected, default 5m) and
`-node` (this machine's name in the runs table).

To play the example game:

```
wge base bases/debian-13
wge build games/heist
wge test games/heist
wge serve

ssh enroll@localhost -p 2222      # register, and collect the first password
ssh heist@localhost -p 2222       # play
```

## Multi-host runs

A game can span several machines, and pivoting between them is the point:

```yaml
hosts:
  - id: relay2
    external: true        # the machine the front door will attach you to
    peers: [vault]
  - id: vault             # not on the internet; reached from something that
    packages: [sqlite3]   # can already see it
```

Each host is a separate image and a separate container, joined by private
**internal** networks — no route to the host or the internet, and Docker's
resolver answers the host id, so `ssh dsundqvist@vault` works from the relay and
nowhere else. Every machine in a run is built when a player connects, not lazily
when they reach a level on it: `ssh vault` has to answer the first time it is
typed.

`external` is what makes the segmentation mean something. Without it a player
holding a credential for an internal machine could present it at the front door
and land there directly, and the private networks would be decoration. A machine
that is not external is reached the way it would be on a real network.

`peers` is reachability, and it is mutual rather than one-way: it is implemented
as shared networks, and a network cannot be traversed in one direction only. A
host that declares no peers can reach every other host. When the reachability
graph is complete the run shares one network; when it is not, there is one
network per adjacent pair, since membership is the only way a Docker network can
express segmentation.

Host keys are derived from the game rather than the run, so a reaped and rebuilt
machine keeps its identity — no `REMOTE HOST IDENTIFICATION HAS CHANGED` from
the engine's own lifecycle — and a game can ship a `known_hosts` that is
actually correct, via `{{ .HostKey "vault" }}`.

Accounts are scoped to the machine their level lives on. An account that existed
everywhere would put its level's home directory on every machine, which on a
multi-host game means the final level's payoff sitting on the box the player
starts from.

## Container lifecycle

Containers are created when a player connects and destroyed when they stop
using them. The reaper is a reference count with a delay: sessions hold a
container, and when the last one lets go destruction is scheduled for the end
of a grace period — long enough to survive a dropped connection or a closed
laptop, short enough that an abandoned game is not still holding memory an hour
later. A session arriving inside that window cancels it.

A periodic sweep collects anything nothing is holding, which includes every
container a previous engine process left behind. Collecting those on startup is
the right thing rather than a compromise: nothing can tell whether a player is
still behind them, and rebuilding one costs a reconnection.

All of this is only safe because of the invariant. Reaping a container costs a
player their scrollback and whatever they wrote, and nothing else — the same
password opens the rebuilt box, because the box is a pure function of the salt.
(Persisting the scratch they wrote is the obvious next improvement.)

While a run has a live container, `runs.current_host` pins it to that machine so
a returning player is sent back to the box they left; the reaper clears it when
the last of the run's containers goes.

## The box is alive

`/sbin/init` is a real Debian sysvinit, not a supervisor script, so the box has
a real process tree: init, syslogd, cron, postfix and the game's own daemons,
each started from the runlevels and reaped properly. `ps -ef`, `ss -tlnp`,
`service`, `mailq` and a tail of `/var/log` all return what they should.

**Not systemd**, and that is a security decision before it is an aesthetic one.
systemd will not boot in a container without `CAP_SYS_ADMIN` — the single
capability most likely to get a player out of the container, which is not a
trade worth making on a box we invite strangers to attack. sysvinit needs none
of it. systemd is then purged outright: leaving `systemctl` on a box that did
not boot with systemd means leaving a tool that answers every question with an
error, and a tool that cannot do its job is a tell.

This is also why a game's timeline should be **anchored to the build** rather
than to a fixed date. As soon as anything on the box is alive, it writes with
the real clock: a cron job that fires, a daemon appending to its log. Against a
fixed date in the past, every one of those entries lands months after the newest
file on a carefully aged box. Leave `timeline.start` unset and the fictional
present and the real present are the same moment — the generated history runs
right up to the live entries with no seam. The resolved anchor travels as an
image label, invisible from inside the container, so seeding and verification
age against the same clock the build used.

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

`no-new-privileges` is deliberately **not** set, for the same reason. It makes
the kernel ignore the setuid bit, so `su` cannot become root to read
`/etc/shadow` and fails with `Authentication failure`, and `sudo` refuses to run
at all. What it would have prevented is a player finding a setuid binary and
becoming root *inside* the container — which is the game working, and is not a
way out. The compensating control is the setuid audit in `wge test`, which
proves the box carries exactly the setuid binaries the base image ships.

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
3. **Progress for in-run transitions.** The broker records a level when it
   attaches a player to it, so a level reached by `su` or by pivoting to
   another machine is never recorded — which for an internal host means never
   at all, since the front door will not attach there. The box already knows:
   the answer is to read the login databases (`/var/log/wtmp.db`) back out of
   each container and take any session later than the container's creation as
   real. No in-container agent, and nothing for a player to find.
4. **Scratch persistence.** A reap currently loses whatever the player wrote. A
   small per-run volume mounted somewhere they keep notes would cost little and
   remove the one real sting.
5. **Admission control.** Nothing yet refuses a connection when a machine is
   full; the resource caps are per container, not per host.

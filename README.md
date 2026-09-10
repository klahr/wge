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

Games live one to a directory under `games/`. There are two:

| | |
|---|---|
| `games/demo` | **Late Return** — three levels, one machine, nothing clever. The smallest game the engine will run, and the one to copy. Its last level is about one of its own staff accounts. |
| `games/heist` | **The Mailroom Job** — four levels, a DAG whose last level is gated on a split credential, two machines, a service and cron. |

A game is a directory:

```
games/heist/
  game.yaml              # image, packages, fictional timeline, staff
  levels/
    01-mailroom/
      level.yaml         # user, prerequisites, the credential it grants
      home/              # file tree copied into the level user's home
      files/             # artifacts elsewhere: files/var/backups/... -> /var/backups/...
      mail/              # messages delivered to the level user's mailbox
      setup.sh           # permissions, services, cron
```

A game names its own cast. The engine invents no names: a Finnish library and
a Swedish freight company do not employ the same people, and an account whose
name the engine chose would belong to somebody else's story.

```yaml
staff:                      # the people who work here and are not the game
  - user: tkoivisto
    name: Tuomas Koivisto
  - user: mvirta
    name: Marja Virta
    role: admin             # flavours their generated shell history and mail
```

Staff are not decoration. A machine with a home directory for every level and
nobody else has written out its own structure, so the validator refuses a game
with none. They can be pinned to one machine with `host:`, or left to work for
the whole company. Level accounts carry a `name:` in the same way, and a name
is optional — a real machine has accounts with an empty gecos field too.

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
wge invite                 create, list or revoke an invitation to enrol
wge reset <handle> <game>  start a player's game over with new credentials
```

`serve` takes `-open` (enrol without an invitation), `-container-runtime`,
`-max-machines`, `-min-free`, `-scratch-size`, `-storage-size`,
`-enroll-limit`, `-grace` (how long a container outlives its last session,
default 15m), `-sweep` (how often abandoned containers are collected, default
5m) and `-node` (this machine's name in the runs table).

To play the example game:

```
wge base bases/debian-13          # the image games are built on
wge build games/demo              # compile the game
wge test  games/demo              # prove the built image enforces its graph
wge invite                        # print an invitation code
wge serve                         # or: wge serve -open

ssh enroll@localhost -p 2222      # redeem the code, collect the first password
ssh demo@localhost -p 2222        # play
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

## Progress

The front door records a level when it attaches somebody to one, which misses
every move a player makes from inside: `su` between two accounts on a machine,
and `ssh` to another machine in the run. For a level on a host the front door
will not attach to, that is the difference between recorded late and never
recorded at all.

The box already knows, so nothing is installed to ask it. Both routes leave the
same line in `/var/log/auth.log` — sshd and `su` each write
`pam_unix(<service>:session): session opened for user <name>` — and at the end
of a session the engine reads back what has been added and matches the accounts
against the game's levels.

The trick is telling a live session from the history the aging pass wrote, and
it is deliberately not a matter of parsing: the generated history uses exactly
the format a real session uses, because anything else would be a tell. Instead
the engine records how far the file had got the moment the machine finished
booting. Everything past that offset happened during play, by construction —
no timestamp is parsed, which matters because syslog lines carry no year.

A failed `su` records nothing, and a session opened as a decoy account is not a
level, so neither reaches the table.

## Resetting a game

A spoiled run is the case the derivation was designed around, and the remedy
costs nothing: re-roll the salt and the same puzzles come back with different
answers. No image is rebuilt and no game content is touched.

A player can do it themselves at the enrollment entrance, which is the one
place the engine speaks out of character. The word has to be typed out —
a reset is the only irreversible thing a player can do to themselves here, and
a menu number is too easy to press by accident. An operator can do it with
`wge reset <handle> <game>`, whether or not a server is running.

Order matters more than it looks. The machines are taken down **before** the
salt is re-rolled: a box still carrying the old credentials would leave the
player with a game whose answers depend on which of the two they reached. And
a reset takes the scratch volume that a reap deliberately keeps, because notes
written against the old credentials are the old answers.

## Getting in

Enrolment is **closed by default**. A server nobody has to be invited to is a
server anybody can fill, and admission control does not help: a script creating
players is using the node exactly as intended, just faster than anyone wanted.

```
wge invite -uses 30 -expires 7d -note "workshop"
CXXK-2V55-R3FH-XBGV
```

The code is read off a screen and typed by hand, so it leaves out the
characters people confuse — no I, L, O, U, 0 or 1 — and is grouped in fours.
Thirty symbols over sixteen places is a little over 78 bits, which is not
guessable at any rate a network will carry. Only its hash is stored: a copy of
the database is not a stack of usable invitations, and a code that has been
lost is replaced rather than recovered.

Redeeming spends a use and creates the player in **one transaction**. Splitting
them would leave either a player who never used an invitation, or an invitation
spent on a player who was never created — and the second costs somebody their
place. The use is spent under a condition in the statement, so two people
racing for the last place on a workshop code cannot both have it.

A player is told *why* a code failed: unknown, spent or expired. A code is not
a secret its holder needs protecting from, and somebody hunting a typo that is
not there gives up on the game rather than on the code.

`wge invite -list` shows what is outstanding and `-revoke` spends the rest of
an invitation's uses without deleting it, so the record of who came in on it
survives. `serve -open` drops the requirement entirely.

Either way, enrolment attempts are rate limited per address — 60 an hour by
default, which is generous because a workshop of thirty behind one office
address is the normal case and a limit that turns them away is worse than the
abuse it prevents. What it stops is the unbounded case.

## The runtime under the containers

A player is invited to attack the box they are on, so the boundary that matters
is the container's. By default that is runc and the host kernel, which means
escape is one kernel bug away. gVisor puts a reimplemented kernel in between
and makes it a genuinely hard problem, so `serve -container-runtime runsc`
exists.

It needs registering with `--allow-suid`:

```json
"runtimes": {
  "runsc": { "path": "/usr/bin/runsc", "runtimeArgs": ["--allow-suid"] }
}
```

**gVisor ignores the setuid bit by default, and the failure is silent.**
Containers start, every service runs, and the only thing that does not work is
`su` — which is how a player moves between levels. The game would be broken in
the one way nobody would think to test.

So the engine tests it at startup, by doing it rather than reasoning about it:
make a setuid copy of a binary that reports its effective uid, run it as
somebody who is not root, and see who it says it is. If the answer is not root,
`serve` refuses to start and prints the `daemon.json` above. Everything the
boxes do turned out to work under gVisor otherwise — sysvinit as PID 1, sshd's
privilege-separation chroot, cron, the MTA.

One thing to be straight about: the check that gVisor ignores setuid was run
here, and so was the refusal it produces. That `--allow-suid` then fixes it is
what the `runsc` flag documents; registering it needs a daemon config change
that was not made on this machine, so that half is documented rather than
demonstrated.

## What a player can write

Three mechanisms, because only one of them is a kernel quota and saying
otherwise would be the whole problem.

**The host keeps room back.** Below `-min-free` no new run starts. This works
on any storage driver and protects everything on the filesystem, not just one
run. It is the only one of the three that keeps the machine alive, and it is on
by default. A player turned away is told the host is at capacity, which is
true; the operator's log says which resource ran out.

**A run's scratch is measured, not capped.** Over `-scratch-size` the player is
told on login — `/srv/scratch is over quota: 4M of 1M. Clear some files.` —
and the operator gets a warning. Nothing portable can stop the write, so this
is a notice, and a shared machine telling somebody their space is full is what
a shared machine does. The measurement is taken from inside the run's own
container, because the engine cannot read the volume's directory on the host
and does not need to.

**A container's own filesystem can have a real quota, if the driver has one.**
`-storage-size` sets it, and the engine **proves it works before relying on
it**: at startup it writes past the limit in a throwaway container and refuses
to serve if the write succeeds.

That check is not hypothetical. Docker accepts `--storage-opt size` on drivers
that do nothing with it — on `overlayfs` over ext4 a container limited to 64M
wrote 200M without complaint. A quota an operator believes in and does not have
is worse than no quota, because it is the one they stop watching. So the
default is off, and configuring it on a host that cannot enforce it is a
service that will not start rather than a limit that quietly does nothing.

## Admission control

A node refuses work it cannot do rather than accepting everything and serving
everybody badly. Capacity is counted in **machines**, not players, because a
run spanning two hosts costs twice as much to serve, and admission is
all-or-nothing: half a run leaves the player on a box whose peer will never
come up, which is a worse failure than being turned away.

The default is derived from the memory the engine reports, keeping a quarter
back for the daemon, the engine and the page cache every container reads its
image through. `-max-machines` overrides it.

Two policies worth stating. A player whose machines are already up is never
refused — they are already resident and already counted, and turning them away
frees nothing. And a machine inside its grace period still counts, because it
is still running.

A refused player is told so, and told their game is untouched. Somebody turned
away with nothing to go on cannot tell a full host from a game they have
broken, and will spend the evening looking for the mistake they did not make.
The session exits `75` (`EX_TEMPFAIL`) so anything scripted can tell a wait
from a failure.

Reservation is the same mechanism as reaping: admission takes the reaper's
hold on every machine the run needs, under the lock the count was taken under,
so two connections arriving together cannot both be told there is room for one.

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
player their scrollback and nothing else — the same password opens the rebuilt
box, because the box is a pure function of the salt.

The one thing that is *not* a function of the salt is what the player wrote
themselves, so that lives on a volume rather than in the container:
`/srv/scratch` is mounted into every machine in the run, survives reaping, and
follows the player when they pivot. Enrollment says so in as many words, since
it is the one place the engine speaks out of character.

Getting a shared scratch to actually be shared took two goes. Copying `/tmp`'s
`1777` looks right and fails twice: the kernel's `fs.protected_regular` refuses
to open a file owned by another user with `O_CREAT` inside a world-writable
sticky directory — and `>>` passes `O_CREAT` — so a note written under one level
could be read under the next but never appended to. Dropping the sticky bit
lands on the ordinary permission instead, since a file created with the default
umask is `0664` owned by its author's private group. It is now done the way a
shared project directory is done on any Unix box: a `scratch` group every
login account belongs to, setgid so new files inherit it, and `UMASK 002` in
`login.defs`. Both of the defaults that got in the way exist to protect users
from each other, and inside one run every account is the same person.

The volume is not quota'd. A disk quota on the backing filesystem is the only
control, the same as for the container's own writable layer.

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

## Running it on a machine

```
make dist                  # a static binary, no libc to match
sudo ./deploy/install.sh   # user, directories, unit, configuration
```

The installer is idempotent: run it again to upgrade. It never overwrites
configuration or game content that is already there, and it does not start
anything, because the images have to exist first.

```
sudo -u wge wge base  /var/lib/wge/bases/debian-13
sudo -u wge wge build /var/lib/wge/games/demo
sudo -u wge wge test  /var/lib/wge/games/demo
systemctl enable --now wge
sudo -u wge wge invite
```

**`serve` refuses to start if a game's image was never built**, naming the
image and the command that builds it. The alternative is a server that starts,
reports itself healthy, and fails at the moment a player presents a correct
password — which reads to them as a broken game and to whoever deployed it as
nothing at all.

Every flag has an environment equivalent — `-max-machines` is
`WGE_MAX_MACHINES` — so the unit carries an `EnvironmentFile` rather than a
line of flags nobody can comment. A flag given on the command line still wins,
so a setting can be overridden for one run without editing the file.

Two things worth being straight about:

- **The engine is in the `docker` group, which is root on that host by another
  name.** That is why it runs as its own user and why the unit drops every
  capability, forbids new privileges, and confines it to `/var/lib/wge`. None
  of that makes the socket safe; it limits what a compromise of the engine
  reaches.
- **Back up `wge.db`.** It holds the run salts, and a salt is the only thing on
  the machine that cannot be rebuilt — images, containers, networks and
  credentials are all derived or disposable. Lose it and every player starts
  over; keep it and a destroyed host costs nobody their game.

## Building and testing

`make` is the entry point, and CI runs the same targets rather than a script of
its own.

```
make check              # gofmt, vet, a tidy go.mod, and a build
make test               # the tests that need nothing but Go, with -race
make base               # the image games are built on
make games              # every game validates, compiles and enforces its graph
make test-integration   # the tests that build images and boot containers
```

The split is by what a target needs, not by how long it takes: `check` and
`test` need only Go and finish in seconds, and the rest need a Docker engine.

One detail is load-bearing. The container-backed tests skip themselves when
there is no engine to talk to, which is right on a laptop and wrong in CI: a
run that skipped everything reports the same green as a run that proved
something. `make test-integration` sets `WGE_REQUIRE_DOCKER`, which turns those
skips into failures, so a CI job that cannot reach an engine says so instead of
passing quietly.

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

Both games are playable end to end. `heist` finishes with a real pivot: an
encrypted SSH key recovered from a staged backup on one machine, its passphrase
found on another level, and an `ssh` across a private network to a document
store the front door will not attach anybody to.

Next, roughly in order:

1. **Credentials inside archives.** `placed_in` accepts an archive member, but
   rendering into one means repacking it during seeding. Until it does, the
   validator rejects the syntax rather than producing a game whose credential
   never appears — a tarball of somebody's home directory is better fiction
   than the extracted copy standing in for it.
2. **A quota on the scratch volumes.** Admission control caps how many machines
   a node runs, but nothing caps what a player writes. A disk quota on the
   backing filesystem is the only control there is today, and the same is true
   of each container's writable layer.
3. **The dead login databases.** The aging pass writes `wtmpdb` and `lastlog2`
   SQLite databases, but purging systemd from the base took the commands that
   read them, so on this base they are generated and never looked at. The
   binary `wtmp` and `lastlog` are the live ones. Either reinstate the tools or
   stop writing the databases.
4. **gVisor or Kata.** The design called for a sandboxed runtime and it was
   never done. It costs syscall performance nobody will notice on a box where
   people run `grep`, and it turns container escape from one kernel bug away
   into a genuinely hard problem — which matters on a machine whose whole
   purpose is to invite strangers to attack it.
5. **More than one node.** A run is a pure function of its salt and
   `runs.current_host` already pins it to a machine, so the scheduling is
   mostly there; what is missing is anything that routes a player to a second
   node, and the scratch volume is the one thing that does not travel.

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/store"

	"golang.org/x/crypto/ssh"
)

type ptyRequest struct {
	Term              string
	Columns, Rows     uint32
	WidthPx, HeightPx uint32
	Modes             string
}

type windowChange struct {
	Columns, Rows     uint32
	WidthPx, HeightPx uint32
}

type execRequest struct {
	Command string
}

type exitStatus struct {
	Code uint32
}

// exitTempFail is sysexits' EX_TEMPFAIL: the request was refused for a reason
// that will pass. Anything driving the engine over ssh can tell it apart from
// a real failure and come back later.
const exitTempFail = 75

// serveSession drives one session channel: the SSH request dance, then either
// the enrollment dialog or an attachment to the player's level.
func (s *Server) serveSession(ctx context.Context, o *outcome, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	sess := &Session{
		Player: o.player,
		Run:    o.run,
		Game:   o.game,
		Term:   "xterm-256color",
		Width:  80,
		Height: 24,
		Stdin:  ch,
		Stdout: ch,
		Stderr: ch.Stderr(),
	}

	resize := make(chan WindowSize, 8)
	sess.Resize = resize
	defer close(resize)

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var pty ptyRequest
			if err := ssh.Unmarshal(req.Payload, &pty); err != nil {
				reply(req, false)
				continue
			}
			sess.Term = pty.Term
			sess.Width, sess.Height = int(pty.Columns), int(pty.Rows)
			reply(req, true)

		case "window-change":
			var wc windowChange
			if err := ssh.Unmarshal(req.Payload, &wc); err != nil {
				reply(req, false)
				continue
			}
			sess.Width, sess.Height = int(wc.Columns), int(wc.Rows)
			// Non-blocking: a player resizing faster than the runtime can keep
			// up should not wedge the request loop.
			select {
			case resize <- WindowSize{Width: int(wc.Columns), Height: int(wc.Rows)}:
			default:
			}
			reply(req, false)

		case "env":
			// The container's environment is the game's to define. Accepting
			// client-supplied variables would let a player alter the box before
			// they have a shell on it.
			reply(req, false)

		case "shell":
			reply(req, true)
			s.finish(ctx, o, sess, ch)
			return

		case "exec":
			var e execRequest
			if err := ssh.Unmarshal(req.Payload, &e); err != nil {
				reply(req, false)
				continue
			}
			sess.Command = e.Command
			reply(req, true)
			s.finish(ctx, o, sess, ch)
			return

		case "subsystem":
			// No sftp: file transfer would let a player pull a level's entire
			// filesystem to their own machine and grep it at leisure, which is
			// a different game from the one being played.
			reply(req, false)

		default:
			reply(req, false)
		}
	}
}

func (s *Server) finish(ctx context.Context, o *outcome, sess *Session, ch ssh.Channel) {
	if o.kind == outcomeEnroll {
		s.enroll(ctx, o, sess)
		sendExit(ch, 0)
		return
	}

	level, ok := o.game.Level(o.levelID)
	if !ok {
		fmt.Fprintf(sess.Stderr, "this level is no longer part of the game\r\n")
		sendExit(ch, 1)
		return
	}
	sess.Level = level

	// Arriving is the progress event. A player who reaches a level by su-ing
	// inside the container is recorded by the container's PAM hook instead;
	// this covers the path through the front door.
	if err := s.cfg.Store.RecordProgress(ctx, o.run.ID, level.ID); err != nil {
		s.log.Error("record progress", "run", o.run.ID, "level", level.ID, "error", err)
	}

	// Only a login shell gets the message of the day. Writing it ahead of a
	// non-interactive `ssh host <cmd>` would prepend it to that command's
	// output, which breaks anything scripted and is not what sshd does.
	if sess.Command == "" {
		if motd := strings.TrimSpace(level.Narrative.MOTD); motd != "" {
			fmt.Fprintf(sess.Stdout, "%s\r\n", strings.ReplaceAll(motd, "\n", "\r\n"))
		}
	}

	code, err := s.cfg.Runtime.Attach(ctx, sess)
	switch {
	case errors.Is(err, ErrAtCapacity):
		// Worth saying plainly. A player turned away with nothing to go on
		// cannot tell a full host from a game they have broken, and will spend
		// the evening looking for the mistake they did not make.
		s.log.Warn("refused at capacity",
			"handle", o.handle(), "game", o.gameID, "level", level.ID, "error", err)
		fmt.Fprintf(sess.Stderr,
			"This host is at capacity. Your game is untouched -- try again in a\r\n"+
				"few minutes.\r\n")
		sendExit(ch, exitTempFail)
		return

	case err != nil:
		s.log.Error("attach", "handle", o.handle(), "level", level.ID, "error", err)
		fmt.Fprintf(sess.Stderr, "could not reach the host, try again shortly\r\n")
		sendExit(ch, 255)
		return
	}
	sendExit(ch, code)
}

var handlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)

// enroll is the only out-of-world conversation in the engine. It exists to hand
// over an entry credential; from then on every interaction is in fiction.
func (s *Server) enroll(ctx context.Context, o *outcome, sess *Session) {
	out := sess.Stdout

	player := o.player
	if player == nil {
		var err error
		player, err = s.registerPlayer(ctx, o, sess)
		if err != nil {
			fmt.Fprintf(sess.Stderr, "%v\r\n", err)
			return
		}
	}

	game, err := s.chooseGame(sess)
	if err != nil {
		fmt.Fprintf(sess.Stderr, "%v\r\n", err)
		return
	}

	entry, ok := game.Entry()
	if !ok {
		fmt.Fprintf(sess.Stderr, "this game has no entry level\r\n")
		return
	}

	// A player who already has a run is almost always here because they lost
	// their notes. Nothing was stored, but everything can be derived again,
	// so that is a question the engine can simply answer.
	run, err := s.cfg.Store.Run(ctx, player.ID, game.ID)
	resuming := err == nil
	if errors.Is(err, store.ErrNotFound) {
		run, err = s.cfg.Store.StartRun(ctx, player.ID, game.ID, game.Version)
	}
	if err != nil {
		s.log.Error("start run", "handle", player.Handle, "game", game.ID, "error", err)
		fmt.Fprintf(sess.Stderr, "could not start the game, try again shortly\r\n")
		return
	}

	password := run.Deriver().Password(entry.ID)

	fmt.Fprintf(out, "\r\n%s\r\n", game.Title)
	if synopsis := strings.TrimSpace(game.Synopsis); synopsis != "" && !resuming {
		fmt.Fprintf(out, "\r\n%s\r\n", wrap(synopsis, 72))
	}

	if resuming {
		wanted, err := s.offerReset(ctx, sess, run, entry)
		if err != nil {
			fmt.Fprintf(sess.Stderr, "%v\r\n", err)
			return
		}
		if !wanted {
			fmt.Fprintf(out, "\r\nIts first account is %s, and the password is:\r\n\r\n    %s\r\n",
				entry.User, password)
			fmt.Fprintf(out, "\r\nYour progress is untouched.\r\n\r\n")
			return
		}

		// The salt has been re-rolled, so everything derived from it has
		// changed; read the run back rather than trusting the copy in hand.
		run, err = s.cfg.Store.Run(ctx, player.ID, game.ID)
		if err != nil {
			fmt.Fprintf(sess.Stderr, "the reset did not complete, try again shortly\r\n")
			return
		}
		password = run.Deriver().Password(entry.ID)

		fmt.Fprintf(out, "\r\nDone. This is a new game: the same puzzles, different answers.\r\n")
	}

	fmt.Fprintf(out, "\r\nYour account is %s. The password is:\r\n\r\n    %s\r\n", entry.User, password)
	// The account is the username, which is the whole rule stated once by
	// example: what a level hands over is a login, not a note to decode.
	fmt.Fprintf(out, "\r\nWrite it down. Reconnect with:\r\n\r\n    ssh %s@<this host>\r\n", entry.User)
	fmt.Fprintf(out, "\r\nThese credentials are yours alone -- another player's notes will not\r\n")
	fmt.Fprintf(out, "open your copy of the game. Each level you reach ends in the credential\r\n")
	fmt.Fprintf(out, "for the next: log back in as the account it names, or as %s if the\r\n", game.ID)
	fmt.Fprintf(out, "password came with no name attached.\r\n")

	// The one thing worth saying out of character about the machines: an idle
	// box is eventually rebuilt, and anything written outside this directory
	// goes with it.
	fmt.Fprintf(out, "\r\nAnything you leave in %s is kept between sessions.\r\n", build.ScratchPath)
	fmt.Fprintf(out, "The rest of the machine is rebuilt when you have been away a while.\r\n\r\n")
}

// offerReset asks whether to start the game over, and does it.
//
// The word has to be typed out. A reset is the one irreversible thing a player
// can do to themselves here -- new credentials throughout, progress gone, notes
// gone -- and a menu number is too easy to press by accident.
func (s *Server) offerReset(
	ctx context.Context, sess *Session, run *store.Run, entry *manifest.Level,
) (bool, error) {
	out := sess.Stdout

	fmt.Fprintf(out, "\r\nYou already have this game in progress.\r\n")
	fmt.Fprintf(out, "\r\n%s\r\n",
		wrap("Press enter to see the first password again, or type reset to start "+
			"over. A reset gives you new credentials throughout, and erases your "+
			"progress along with anything you left in "+build.ScratchPath+".", 72))
	fmt.Fprintf(out, "\r\n> ")

	line, err := readLine(sess.Stdin, sess.Stdout)
	if err != nil {
		return false, nil // hung up; nothing has been touched
	}
	if strings.ToLower(strings.TrimSpace(line)) != "reset" {
		return false, nil
	}

	// The machines go first. Re-rolling the salt while a box carrying the old
	// credentials is still up would leave the player with a game whose answers
	// depend on which of the two they reach.
	resetter, ok := s.cfg.Runtime.(Resetter)
	if !ok {
		return false, fmt.Errorf("this server cannot reset games")
	}
	if err := resetter.DestroyRun(ctx, run.ID); err != nil {
		s.log.Error("destroy run for reset", "run", run.ID, "error", err)
		return false, fmt.Errorf("could not take the machines down, nothing has changed")
	}

	if err := s.cfg.Store.ResetRun(ctx, run.ID, entryGameVersion(s, run)); err != nil {
		s.log.Error("reset run", "run", run.ID, "error", err)
		return false, fmt.Errorf("the machines are down but the reset did not complete; reconnect to try again")
	}

	s.log.Info("run reset", "run", run.ID)
	return true, nil
}

// entryGameVersion is the version a reset adopts: the one being served now,
// not the one the abandoned run was pinned to.
func entryGameVersion(s *Server, run *store.Run) int {
	if game, ok := s.cfg.Games.Game(run.GameID); ok {
		return game.Version
	}
	return run.GameVersion
}

// chooseGame asks which game to start, skipping the question when there is
// only one on offer.
func (s *Server) chooseGame(sess *Session) (*manifest.Game, error) {
	games := s.cfg.Games.All()
	switch len(games) {
	case 0:
		return nil, fmt.Errorf("no games are available")
	case 1:
		return games[0], nil
	}

	fmt.Fprintf(sess.Stdout, "\r\nGames:\r\n\r\n")
	for i, g := range games {
		fmt.Fprintf(sess.Stdout, "  %d) %-16s %s\r\n", i+1, g.ID, g.Title)
	}

	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(sess.Stdout, "\r\nWhich one? ")
		line, err := readLine(sess.Stdin, sess.Stdout)
		if err != nil {
			return nil, fmt.Errorf("cancelled")
		}

		choice := strings.ToLower(strings.TrimSpace(line))
		for i, g := range games {
			if choice == g.ID || choice == strconv.Itoa(i+1) {
				return g, nil
			}
		}
		fmt.Fprintf(sess.Stdout, "No game by that name.\r\n")
	}
	return nil, fmt.Errorf("no game chosen")
}

func (s *Server) registerPlayer(ctx context.Context, o *outcome, sess *Session) (*store.Player, error) {
	// Counted before anything is asked, so that a script working through
	// invitation codes is stopped by the same limit as one creating players.
	if !s.limiter.allow(o.remote, time.Now()) {
		s.log.Warn("enrolment refused: too many attempts", "remote", hostOf(o.remote))
		return nil, fmt.Errorf("too many enrolment attempts from this address; try again later")
	}

	fmt.Fprintf(sess.Stdout, "\r\nThis key hasn't been seen before.\r\n")

	var token string
	if !s.cfg.OpenEnrollment {
		var err error
		if token, err = s.askInvite(ctx, sess); err != nil {
			return nil, err
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(sess.Stdout, "Choose a handle: ")
		line, err := readLine(sess.Stdin, sess.Stdout)
		if err != nil {
			return nil, fmt.Errorf("enrollment cancelled")
		}

		handle := strings.ToLower(strings.TrimSpace(line))
		if !handlePattern.MatchString(handle) {
			fmt.Fprintf(sess.Stdout, "Handles are 2-32 characters: letters, digits, - and _.\r\n")
			continue
		}

		var player *store.Player
		if token == "" {
			player, err = s.cfg.Store.CreatePlayer(ctx, handle, o.fingerprint)
		} else {
			// The invitation is spent in the same transaction that creates the
			// player, so a failure here costs nobody their place.
			player, err = s.cfg.Store.RedeemInvite(ctx, token, handle, o.fingerprint)
		}
		if err == nil {
			s.log.Info("player enrolled", "handle", handle, "invited", token != "",
				"remote", hostOf(o.remote))
			return player, nil
		}

		// An invitation that went while the handle was being chosen is not the
		// handle's fault, and saying "that handle is taken" would send them
		// hunting for the wrong problem.
		switch {
		case errors.Is(err, store.ErrInviteSpent):
			return nil, fmt.Errorf("that invitation was taken while you were choosing a handle")
		case errors.Is(err, store.ErrInviteUnknown), errors.Is(err, store.ErrInviteExpired):
			return nil, fmt.Errorf("that invitation is no longer valid")
		}

		// Almost always a duplicate handle; nothing here is worth leaking
		// database detail over.
		fmt.Fprintf(sess.Stdout, "That handle is taken.\r\n")
	}
	return nil, fmt.Errorf("no handle chosen")
}

// askInvite collects an invitation code and checks it before going further.
func (s *Server) askInvite(ctx context.Context, sess *Session) (string, error) {
	fmt.Fprintf(sess.Stdout, "\r\nThis server is invitation only.\r\n")

	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(sess.Stdout, "\r\nInvitation code: ")
		line, err := readLine(sess.Stdin, sess.Stdout)
		if err != nil {
			return "", fmt.Errorf("enrollment cancelled")
		}

		// A code is not a secret its holder needs protecting from, so the
		// reason it failed is worth saying: somebody hunting a typo that is not
		// there gives up on the game, not on the code.
		switch err := s.cfg.Store.CheckInvite(ctx, line); {
		case err == nil:
			return line, nil
		case errors.Is(err, store.ErrInviteUnknown):
			fmt.Fprintf(sess.Stdout, "That is not a code I recognise.\r\n")
		case errors.Is(err, store.ErrInviteSpent):
			fmt.Fprintf(sess.Stdout, "That code has already been used.\r\n")
		case errors.Is(err, store.ErrInviteExpired):
			fmt.Fprintf(sess.Stdout, "That code has expired.\r\n")
		default:
			s.log.Error("check invitation", "error", err)
			return "", fmt.Errorf("could not check that code, try again shortly")
		}
	}
	return "", fmt.Errorf("no valid invitation")
}

// readLine reads one line, echoing as it goes. The client's terminal is in raw
// mode, so the server is responsible for echo and for backspace.
func readLine(r io.Reader, w io.Writer) (string, error) {
	var line []byte
	buf := make([]byte, 1)

	for {
		n, err := r.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}

		switch c := buf[0]; c {
		case '\r', '\n':
			fmt.Fprint(w, "\r\n")
			return string(line), nil
		case 0x03, 0x04: // Ctrl-C, Ctrl-D
			return "", io.EOF
		case 0x7f, 0x08: // backspace
			if len(line) > 0 {
				line = line[:len(line)-1]
				fmt.Fprint(w, "\b \b")
			}
		default:
			if c < 0x20 || len(line) >= 64 {
				continue
			}
			line = append(line, c)
			_, _ = w.Write(buf[:1])
		}
	}
}

func wrap(s string, width int) string {
	var b strings.Builder
	col := 0
	for _, word := range strings.Fields(s) {
		if col > 0 && col+1+len(word) > width {
			b.WriteString("\r\n")
			col = 0
		} else if col > 0 {
			b.WriteString(" ")
			col++
		}
		b.WriteString(word)
		col += len(word)
	}
	return b.String()
}

func reply(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

func sendExit(ch ssh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatus{Code: uint32(code)}))
}

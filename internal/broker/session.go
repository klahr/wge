package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

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
	if err != nil {
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
		fmt.Fprintf(out, "\r\nYou already have this game in progress. Its first account is %s,\r\n", entry.User)
		fmt.Fprintf(out, "and the password is:\r\n\r\n    %s\r\n", password)
		fmt.Fprintf(out, "\r\nYour progress is untouched.\r\n\r\n")
		return
	}

	fmt.Fprintf(out, "\r\nYour account is %s. The password is:\r\n\r\n    %s\r\n", entry.User, password)
	fmt.Fprintf(out, "\r\nWrite it down. Reconnect with:\r\n\r\n    ssh %s@<this host>\r\n", game.ID)
	fmt.Fprintf(out, "\r\nThese credentials are yours alone -- another player's notes will not\r\n")
	fmt.Fprintf(out, "open your copy of the game. Each level you reach ends in the credential\r\n")
	fmt.Fprintf(out, "for the next; log back in with it to continue.\r\n\r\n")
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
	fmt.Fprintf(sess.Stdout, "\r\nThis key hasn't been seen before.\r\n")

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

		player, err := s.cfg.Store.CreatePlayer(ctx, handle, o.fingerprint)
		if err != nil {
			// Almost always a duplicate handle; nothing here is worth leaking
			// database detail over.
			fmt.Fprintf(sess.Stdout, "That handle is taken.\r\n")
			continue
		}
		return player, nil
	}
	return nil, fmt.Errorf("no handle chosen")
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

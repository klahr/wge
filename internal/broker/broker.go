// Package broker is the SSH front door.
//
// Containers do not run sshd. One server terminates every player connection,
// identifies the player by public key, resolves their run, and only then
// attaches them to a container it can create on demand. That indirection is
// what makes lazy creation, idle reaping, session recording and progress
// tracking possible at all.
//
// The auth chain is also the game's central mechanic. A player's public key
// says who they are; the password they then supply says which level they have
// earned their way into. The credential found on one level is literally the
// login for the next, so there is no flag to submit and no menu to break the
// fiction.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/klahr/wge/internal/manifest"
	"github.com/klahr/wge/internal/store"

	"golang.org/x/crypto/ssh"
)

// Games resolves a game id to its loaded definition.
type Games interface {
	Game(id string) (*manifest.Game, bool)
	All() []*manifest.Game
}

// ErrAtCapacity is returned by a Runtime that has no room to build a run's
// machines. It is part of the interface contract rather than a runtime detail,
// because the broker has to tell the player something true about it: a refusal
// they cannot distinguish from a broken game is worse than a wait.
var ErrAtCapacity = errors.New("host is at capacity")

// Runtime attaches a player to the container for their level, creating it if
// necessary. Implementations own container lifecycle; the broker only ever
// asks for a level and a pair of streams.
type Runtime interface {
	Attach(ctx context.Context, s *Session) (exitCode int, err error)
}

// Resetter destroys everything belonging to a run.
//
// A runtime that implements it lets a player start a game over from the
// enrollment entrance. Without it a reset can still re-roll the credentials,
// but the machines carrying the old ones would survive until they were reaped,
// so the broker refuses rather than handing back a game in two minds.
type Resetter interface {
	DestroyRun(ctx context.Context, runID int64) error
}

// WindowSize is a terminal geometry change.
type WindowSize struct {
	Width, Height int
}

// Session is one player's attachment to one level.
type Session struct {
	Player *store.Player
	Run    *store.Run
	Game   *manifest.Game
	Level  *manifest.Level

	Term          string
	Width, Height int
	Resize        <-chan WindowSize

	// Command is set for a non-interactive `ssh host <cmd>` invocation.
	Command string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Config configures a broker.
type Config struct {
	Addr    string
	HostKey ssh.Signer

	Store   *store.Store
	Games   Games
	Runtime Runtime

	Logger *slog.Logger

	// AuthTimeout bounds how long a connection may sit unauthenticated.
	AuthTimeout time.Duration

	// OpenEnrollment lets anybody who can reach the port become a player.
	// Without it an invitation is required, which is the safe default: a
	// server nobody has to be invited to is a server anybody can fill.
	OpenEnrollment bool

	// EnrollLimit is how many enrolment attempts one address may make in
	// EnrollWindow. Zero takes the default; negative removes the limit.
	EnrollLimit  int
	EnrollWindow time.Duration
}

// Server is the SSH front door.
type Server struct {
	cfg     Config
	log     *slog.Logger
	limiter *enrollLimiter

	listener net.Listener
	wg       sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// New returns a broker ready to listen.
func New(cfg Config) (*Server, error) {
	if cfg.HostKey == nil {
		return nil, errors.New("broker: host key is required")
	}
	if cfg.Store == nil || cfg.Games == nil || cfg.Runtime == nil {
		return nil, errors.New("broker: store, games and runtime are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.AuthTimeout == 0 {
		cfg.AuthTimeout = 30 * time.Second
	}
	return &Server{
		cfg:     cfg,
		log:     cfg.Logger,
		limiter: newEnrollLimiter(cfg.EnrollLimit, cfg.EnrollWindow),
	}, nil
}

// Listen binds the broker's address without serving, so callers (and tests)
// can learn the port before connections are accepted.
func (s *Server) Listen() (net.Addr, error) {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	s.listener = ln
	return ln.Addr(), nil
}

// Serve accepts connections until the context is cancelled or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		if _, err := s.Listen(); err != nil {
			return err
		}
	}

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

// Close stops accepting and waits for live connections to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	return err
}

func (s *Server) handle(ctx context.Context, nc net.Conn) {
	defer nc.Close()

	// An unauthenticated connection must not be able to hold a slot open.
	deadline := time.Now().Add(s.cfg.AuthTimeout)
	_ = nc.SetDeadline(deadline)

	auth := newAuthenticator(s.cfg, s.log)
	conn, chans, reqs, err := ssh.NewServerConn(nc, auth.serverConfig())
	if err != nil {
		s.log.Debug("handshake failed", "remote", nc.RemoteAddr().String(), "error", err)
		return
	}
	defer conn.Close()

	// Authentication is done; the session itself may last as long as it likes.
	_ = nc.SetDeadline(time.Time{})

	go ssh.DiscardRequests(reqs)

	outcome, err := auth.outcome(conn.Permissions)
	if err != nil {
		s.log.Error("authenticated connection carries no outcome", "error", err)
		return
	}

	s.log.Info("player connected",
		"handle", outcome.handle(),
		"game", outcome.gameID,
		"level", outcome.levelID,
		"remote", conn.RemoteAddr().String())

	var wg sync.WaitGroup
	for newChan := range chans {
		// Shells only. A player who could open a direct-tcpip channel would
		// turn the broker into an open proxy sitting outside the game's own
		// egress rules.
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are permitted")
			continue
		}

		ch, chReqs, err := newChan.Accept()
		if err != nil {
			s.log.Debug("accept channel", "error", err)
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveSession(ctx, outcome, ch, chReqs)
		}()
	}
	wg.Wait()
}

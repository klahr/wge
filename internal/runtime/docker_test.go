package runtime

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func frame(stream byte, payload string) []byte {
	header := make([]byte, dockerStreamHeader)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, payload...)
}

// Without demultiplexing, the eight-byte frame headers land in the player's
// terminal as stray control bytes ahead of every chunk of output.
func TestDemultiplexSplitsStreams(t *testing.T) {
	var stream []byte
	stream = append(stream, frame(1, "on stdout\n")...)
	stream = append(stream, frame(2, "on stderr\n")...)
	stream = append(stream, frame(1, "more stdout\n")...)

	var stdout, stderr bytes.Buffer
	if err := demultiplex(bytes.NewReader(stream), &stdout, &stderr); err != nil {
		t.Fatalf("demultiplex: %v", err)
	}

	if got := stdout.String(); got != "on stdout\nmore stdout\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := stderr.String(); got != "on stderr\n" {
		t.Errorf("stderr = %q", got)
	}
}

// A container killed mid-frame must end the session, not error into the
// player's terminal.
func TestDemultiplexToleratesTruncation(t *testing.T) {
	stream := frame(1, "complete\n")
	stream = append(stream, frame(1, "cut off here")[:dockerStreamHeader+4]...)

	var stdout, stderr bytes.Buffer
	if err := demultiplex(bytes.NewReader(stream), &stdout, &stderr); err != nil {
		t.Fatalf("truncated stream should end cleanly, got: %v", err)
	}
	if !strings.HasPrefix(stdout.String(), "complete\n") {
		t.Errorf("output before the truncation was lost: %q", stdout.String())
	}
}

// Container names are derived rather than tracked, so a broker that crashed can
// still find, reuse and reap what it left behind.
func TestContainerNameIsDerivable(t *testing.T) {
	if got := ContainerName(42, "mailsrv"); got != "wge-run-42-mailsrv" {
		t.Errorf("ContainerName = %q", got)
	}
	if ContainerName(1, "a") == ContainerName(1, "b") {
		t.Error("two hosts of one run collided on a container name")
	}
	if ContainerName(1, "a") == ContainerName(2, "a") {
		t.Error("two runs collided on a container name")
	}
}

// Dropping every capability is the obvious move and it breaks su(1), which is
// how a player moves between levels inside a live session.
func TestDefaultCapabilitiesPermitPrivilegeDropping(t *testing.T) {
	caps := DefaultLimits().Capabilities

	// su(1) needs these to drop to a level account.
	for _, required := range []string{"SETUID", "SETGID"} {
		if !contains(caps, required) {
			t.Errorf("%s is missing; su between levels will fail with \"cannot set groups\"", required)
		}
	}
	// chpasswd needs CHOWN to write its replacement /etc/shadow as root:shadow;
	// without it a run's passwords are never set and no level can be entered.
	if !contains(caps, "CHOWN") {
		t.Error("CHOWN is missing; chpasswd cannot write /etc/shadow and seeding fails")
	}
	// Anything that grants reach beyond the container must stay dropped.
	// What must stay dropped is what gets a player off the box or onto the
	// network -- not what uid 0 needs to administer the box itself.
	for _, forbidden := range []string{
		"SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_CHROOT", "SYS_BOOT",
		"NET_ADMIN", "NET_RAW", "MKNOD", "SETFCAP", "SETPCAP", "ALL",
	} {
		if contains(caps, forbidden) {
			t.Errorf("%s must not be granted to a game container", forbidden)
		}
	}
}

func TestSandboxDefaults(t *testing.T) {
	d := &Docker{limits: DefaultLimits()}
	cfg := d.hostConfig()

	if got := cfg["NetworkMode"]; got != "none" {
		t.Errorf("NetworkMode = %v; an unfiltered game box becomes someone's relay", got)
	}
	if cfg["PidsLimit"].(int64) <= 0 {
		t.Error("PidsLimit must be set; a fork bomb is the first thing a bored player tries")
	}
	if cfg["Memory"].(int64) <= 0 {
		t.Error("Memory must be capped")
	}
	if drop := cfg["CapDrop"].([]string); len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL] before adding back", drop)
	}
	// no-new-privileges must NOT be set: it makes the kernel ignore the setuid
	// bit, which breaks su(1) and with it the move between levels.
	if opt, ok := cfg["SecurityOpt"]; ok {
		t.Errorf("SecurityOpt = %v; no-new-privileges breaks su and sudo on a multi-user box", opt)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

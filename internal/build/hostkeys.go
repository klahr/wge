package build

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Host keys are derived from the game rather than the run.
//
// They are not secrets, and deriving them has two payoffs. A container that is
// reaped and rebuilt comes back with the same identity, so a player who has
// pivoted to a box once does not meet "REMOTE HOST IDENTIFICATION HAS CHANGED"
// on their next visit -- which would be the engine's lifecycle leaking into the
// fiction. And a game can ship a known_hosts file whose entries are correct,
// because the key is knowable at build time.
const hostKeyPurpose = "wge-ssh-host-key"

// HostKey returns a host's SSH identity for a game version.
func HostKey(gameID string, version int, host string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s\x1f%s\x1f%d\x1f%s",
		hostKeyPurpose, gameID, version, host)))
	return ed25519.NewKeyFromSeed(seed[:])
}

// HostKeyLine returns a host's public key as a known_hosts entry.
func HostKeyLine(gameID string, version int, host string) (string, error) {
	pub, err := ssh.NewPublicKey(HostKey(gameID, version, host).Public())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s", host, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))), nil
}

// sshdConfig is the drop-in the game images carry.
//
// Password authentication stays on: the game's credentials are passwords as
// often as keys, and a player who finds one has to be able to use it. Root
// login stays off, because no level is root and an open root login would be a
// way past the whole graph.
const sshdConfig = `# Ardent standard build.
HostKey /etc/ssh/ssh_host_ed25519_key
PermitRootLogin no
PasswordAuthentication yes
KbdInteractiveAuthentication no
X11Forwarding no
PrintMotd yes
AcceptEnv LANG LC_*
`

// addHostKeys gives the host its SSH identity and configuration.
func (p *Plan) addHostKeys() error {
	key := HostKey(p.Game.ID, p.Game.Version, p.Host)

	block, err := ssh.MarshalPrivateKey(key, p.Host)
	if err != nil {
		return fmt.Errorf("marshal host key for %s: %w", p.Host, err)
	}

	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return err
	}
	public := fmt.Sprintf("%s root@%s\n",
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), p.Host)

	owner := Owner{User: "root"}
	private := "/etc/ssh/ssh_host_ed25519_key"

	p.rootfs.add(entry{
		Path: private, Content: pem.EncodeToMemory(block),
		Mode: 0o600, UID: 0, GID: 0,
		ModTime: p.Aging.AccountCreated(owner),
	})
	p.rootfs.add(entry{
		Path: private + ".pub", Content: []byte(public),
		Mode: 0o644, UID: 0, GID: 0,
		ModTime: p.Aging.AccountCreated(owner),
	})
	p.rootfs.add(entry{
		Path: "/etc/ssh/sshd_config.d/10-ardent.conf", Content: []byte(sshdConfig),
		Mode: 0o644, UID: 0, GID: 0,
		ModTime: p.Aging.FileTime(owner, "/etc/ssh/sshd_config.d/10-ardent.conf"),
	})
	return nil
}

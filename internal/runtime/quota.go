package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klahr/wge/internal/broker"
	"github.com/klahr/wge/internal/build"
	"github.com/klahr/wge/internal/docker"
)

// ErrStorageQuotaIgnored is returned when the storage driver accepts a size
// limit and does not enforce it.
var ErrStorageQuotaIgnored = errors.New("the storage driver ignores its size limit")

// ErrDiskFull is returned when the host has less room than it is willing to
// start a new run with.
var ErrDiskFull = errors.New("not enough disk left to start a run")

// VerifyStorageQuota proves that a per-container size limit is real.
//
// It has to be proved rather than configured, because Docker accepts
// --storage-opt size on drivers that do nothing with it: overlayfs on ext4
// takes the option, reports success, and lets a container write until the disk
// is full. A quota an operator believes in and does not have is worse than no
// quota, because it is the one they will not watch.
//
// So the probe writes past the limit and sees whether it is stopped.
func (d *node) VerifyStorageQuota(ctx context.Context, image string) error {
	if d.limits.StorageBytes <= 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()

	// Names the driver in the failure, which is the first thing an operator
	// will want to know.
	if _, err := d.dockerRoot(ctx); err != nil {
		return err
	}

	name := fmt.Sprintf("wge-quota-%d", time.Now().UnixNano())

	host := d.hostConfig(nil, 0)
	delete(host, "Mounts")

	body := map[string]any{
		"Image":      image,
		"Entrypoint": []string{"/bin/sleep"},
		"Cmd":        []string{"600"},
		"Labels":     map[string]string{"wge.preflight": "1"},
		"HostConfig": host,
	}
	if err := d.api.Post(ctx, "/containers/create?name="+name, body, nil); err != nil {
		return fmt.Errorf("create quota probe: %w", err)
	}
	defer func() {
		_ = d.api.Delete(context.WithoutCancel(ctx), "/containers/"+name+"?force=true&v=true")
	}()

	if err := d.api.Post(ctx, "/containers/"+name+"/start", nil, nil); err != nil {
		return fmt.Errorf("start quota probe: %w", err)
	}

	// Twice the limit, in one-megabyte pieces. A driver that enforces the
	// limit fails partway; one that ignores it writes the lot.
	blocks := (d.limits.StorageBytes / (1 << 20)) * 2
	if blocks < 2 {
		blocks = 2
	}
	script := fmt.Sprintf(
		"dd if=/dev/zero of=/wge-quota-probe bs=1M count=%d >/dev/null 2>&1; "+
			"stat -c %%s /wge-quota-probe 2>/dev/null || echo 0", blocks)

	res, err := d.api.Exec(ctx, name, docker.ExecOptions{
		Cmd: []string{"sh", "-c", script}, User: "root",
	})
	if err != nil {
		return fmt.Errorf("run quota probe: %w", err)
	}

	written, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return fmt.Errorf("quota probe wrote something unreadable: %q", res.Stdout)
	}

	if written > d.limits.StorageBytes {
		return fmt.Errorf("%w: %s wrote %s into a container limited to %s",
			ErrStorageQuotaIgnored, d.storageDriver, humanBytes(written),
			humanBytes(d.limits.StorageBytes))
	}
	return nil
}

// freeBytes is how much room is left where the containers live.
//
// The path comes from the engine rather than being assumed, because the daemon
// may keep its data anywhere; the measurement is a plain statfs, which needs no
// privilege beyond reaching the directory.
func (d *node) freeBytes(ctx context.Context) (int64, error) {
	root, err := d.dockerRoot(ctx)
	if err != nil {
		return 0, err
	}

	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		return 0, fmt.Errorf("measure free space at %s: %w", root, err)
	}
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}

// dockerRoot asks the engine where it keeps its data, once.
func (d *node) dockerRoot(ctx context.Context) (string, error) {
	d.rootOnce.Do(func() {
		var info struct {
			DockerRootDir string `json:"DockerRootDir"`
			Driver        string `json:"Driver"`
		}
		if err := d.api.Get(ctx, "/info", &info); err != nil {
			d.rootErr = fmt.Errorf("ask the engine where it keeps its data: %w", err)
			return
		}
		d.root = info.DockerRootDir
		d.storageDriver = info.Driver
	})
	return d.root, d.rootErr
}

// checkDisk refuses a new run when the host is close to full.
//
// This is the protection that does not depend on the storage driver having a
// quota, and it protects everything: a run's writable layer, its scratch, the
// images, and whatever else shares the filesystem. It is a floor under the
// host, not a fair share between players.
func (d *node) checkDisk(ctx context.Context) error {
	if d.limits.MinFreeBytes <= 0 {
		return nil
	}

	free, err := d.freeBytes(ctx)
	if err != nil {
		// Not being able to measure is not a reason to stop serving; it is a
		// reason to say so.
		d.log.Warn("cannot measure free disk space", "error", err)
		return nil
	}

	if free < d.limits.MinFreeBytes {
		// Capacity as far as the player is concerned -- they are told to come
		// back later, which is true -- while the operator's log says which
		// resource ran out.
		return fmt.Errorf("%w: %w: %s left, %s required",
			broker.ErrAtCapacity, ErrDiskFull,
			humanBytes(free), humanBytes(d.limits.MinFreeBytes))
	}
	return nil
}

// scratchUsage measures what a run has written to its scratch space.
//
// Measured from inside the run's own container, which already has the volume
// mounted: the engine cannot read the volume's directory on the host, and does
// not need to.
func (d *node) scratchUsage(ctx context.Context, container string) (int64, error) {
	res, err := d.api.Exec(ctx, container, docker.ExecOptions{
		Cmd:  []string{"sh", "-c", "du -sb " + build.ScratchPath + " 2>/dev/null | cut -f1"},
		User: "root",
	})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
}

// humanBytes is for messages people read.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

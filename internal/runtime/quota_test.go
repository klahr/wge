package runtime

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"testing"

	"github.com/klahr/wge/internal/broker"
)

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{512, "512B"},
		{1 << 10, "1K"},
		{256 << 20, "256M"},
		{5 << 30, "5G"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %s, want %s", tc.n, got, tc.want)
		}
	}
}

// A limit is only sent once it has been proved to work; sending one that does
// nothing is how an operator ends up believing in a quota they do not have.
func TestStorageLimitReachesTheHostConfig(t *testing.T) {
	limits := DefaultLimits()
	limits.StorageBytes = 64 << 20
	d := &node{limits: limits}

	opt, ok := d.hostConfig(nil, 1)["StorageOpt"].(map[string]string)
	if !ok {
		t.Fatalf("StorageOpt = %v", d.hostConfig(nil, 1)["StorageOpt"])
	}
	if got := opt["size"]; got != strconv.FormatInt(64<<20, 10) {
		t.Errorf("size = %s", got)
	}

	// Uncapped is the default, because most drivers cannot enforce one.
	plain := &node{limits: DefaultLimits()}
	if got, ok := plain.hostConfig(nil, 1)["StorageOpt"]; ok {
		t.Errorf("StorageOpt = %v with no limit configured", got)
	}
}

// The defaults protect the host without pretending to enforce a per-container
// quota that most storage drivers cannot.
func TestDefaultLimitsAreHonest(t *testing.T) {
	l := DefaultLimits()

	if l.StorageBytes != 0 {
		t.Errorf("StorageBytes = %d; the default must not claim a quota that is usually ignored", l.StorageBytes)
	}
	if l.MinFreeBytes <= 0 {
		t.Error("MinFreeBytes must be set: it is the protection that works on any driver")
	}
	if l.ScratchBytes <= 0 {
		t.Error("ScratchBytes must be set, or nothing measures what a player writes")
	}
}

// Nothing to verify when nothing is configured.
func TestStorageQuotaCheckIsSkippedWhenUnset(t *testing.T) {
	d := &node{limits: DefaultLimits()}
	if err := d.VerifyStorageQuota(context.Background(), "unused"); err != nil {
		t.Fatalf("VerifyStorageQuota with no limit: %v", err)
	}
}

// A disk that is nearly full reads to the player as capacity, which is true,
// while the operator's log says which resource ran out.
func TestDiskFullPresentsAsCapacity(t *testing.T) {
	if !errors.Is(diskFullExample(), broker.ErrAtCapacity) {
		t.Error("a full disk is not reported as capacity, so the player is told nothing useful")
	}
	if !errors.Is(diskFullExample(), ErrDiskFull) {
		t.Error("a full disk is not distinguishable in the log")
	}
}

func diskFullExample() error {
	limits := DefaultLimits()
	limits.MinFreeBytes = 1 << 62 // more than any disk

	d := &node{limits: limits, log: discardLogger()}
	d.rootOnce.Do(func() { d.root = "/" })
	return d.checkDisk(context.Background())
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const lockPoll = time.Second

func acquireHostLock(ctx context.Context, shared string, poll time.Duration) (func() error, error) {
	file, err := os.OpenFile(filepath.Join(shared, "lock"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("host lock: %w", err)
	}
	if err := singleRegularFile(file); err != nil {
		return nil, errors.Join(fmt.Errorf("host lock: %w", err), file.Close())
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file.Close, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("host lock: %w", err), file.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("host lock: %w", context.Cause(ctx)), file.Close())
		case <-time.After(poll):
		}
	}
}

func singleRegularFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		return errors.New("not a single-link regular file")
	}
	return nil
}

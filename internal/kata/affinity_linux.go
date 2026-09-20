package kata

import (
	"errors"
	"syscall"
	"unsafe"
)

func boundAffinity(count int) error {
	var mask [128]byte
	_, _, e := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0, uintptr(len(mask)), uintptr(unsafe.Pointer(&mask[0])))
	if e != 0 {
		return e
	}
	var selected [128]byte
	used := 0
	for i, b := range mask {
		for bit := uint(0); bit < 8; bit++ {
			if b&(1<<bit) != 0 && used < count {
				selected[i] |= 1 << bit
				used++
			}
		}
	}
	if used == 0 {
		return errors.New("empty CPU affinity")
	}
	_, _, e = syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, 0, uintptr(len(selected)), uintptr(unsafe.Pointer(&selected[0])))
	if e != 0 {
		return e
	}
	return nil
}

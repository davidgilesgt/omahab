//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func exchangeDirs(a, b string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("renameat2 exchange %q <-> %q: %w", a, b, err)
	}
	return nil
}

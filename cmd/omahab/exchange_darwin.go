//go:build !linux

package main

import "fmt"

func exchangeDirs(a, b string) error {
	return fmt.Errorf("exchange not supported on this platform: renameat2 requires Linux")
}

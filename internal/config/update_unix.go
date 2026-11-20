//go:build !windows

// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"syscall"
)

func lockConfig(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

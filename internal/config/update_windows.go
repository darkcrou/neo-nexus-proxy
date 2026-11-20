// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockConfig(file *os.File) error {
	var overlap windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlap)
}

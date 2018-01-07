// +build linux darwin

package gopass

import (
	"syscall"

	"mongoIncbackup-1.1/x/crypto/ssh/terminal"
)

func getch() byte {
	if oldState, err := terminal.MakeRaw(0); err != nil {
		panic(err)
	} else {
		defer terminal.Restore(0, oldState)
	}

	var buf [1]byte
	if n, err := syscall.Read(0, buf[:]); n == 0 || err != nil {
		panic(err)
	}
	return buf[0]
}

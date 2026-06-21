//go:build windows

package codexcli

import (
	"os/exec"
	"syscall"
)

// createNoWindow is the Windows CREATE_NO_WINDOW process-creation flag.
// Setting it on the child stops Windows from allocating a console for the
// process, so no console window flashes when the parent itself has no
// console (detached server, service, GUI/tray).
const createNoWindow = 0x08000000

// hideConsoleWindow sets CREATE_NO_WINDOW on the command so the codex
// subprocess never pops a console window.
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

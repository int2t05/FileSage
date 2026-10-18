//go:build !linux

package imagepreview

import "os/exec"

func configureProcess(*exec.Cmd) {}
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

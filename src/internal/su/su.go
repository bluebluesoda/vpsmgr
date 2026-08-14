// Package su runs the few root-privileged commands the vps panel needs.
//
// The panel daemon (vps serve) runs as the unprivileged 'vps' user. A small
// sudoers whitelist (installed to /etc/sudoers.d/vps at install time) allows
// that user to run ONLY the exact commands vpsmgr needs as root — nftables
// reloads, traefik service control, IPv6 route/neighbor/addr changes, sysctl
// and the ndppd NDP proxy. Everything else is denied.
//
// Run always passes -n (non-interactive): if sudo would prompt for a password
// the call fails instead of hanging the panel.
package su

import (
	"fmt"
	"os/exec"
	"strings"
)

// Run executes the privileged command via sudo -n and returns its trimmed
// combined output. On failure the stderr text is included in the error.
func Run(args ...string) (string, error) {
	cmd := exec.Command("sudo", append([]string{"-n"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sudo %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

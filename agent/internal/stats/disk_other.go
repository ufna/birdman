//go:build !linux

package stats

// diskUsage is only implemented for the Linux target the agent ships on;
// other platforms (development hosts) report zeros.
func diskUsage(string) (used, total uint64) { return 0, 0 }

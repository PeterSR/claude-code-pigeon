//go:build !linux

package pigeon

// ProcStart has no portable equivalent outside Linux's /proc, so it returns
// "" and liveness falls back to "does this PID exist". PID reuse is therefore
// not detected on these platforms: a recycled PID can make a dead session look
// alive until something else prunes it.
func ProcStart(pid int) string { return "" }

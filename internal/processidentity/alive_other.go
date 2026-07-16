//go:build !unix

package processidentity

// On platforms without signal-0, conservatively keep bindings owned by a PID.
func Alive(pid int) bool { return pid > 0 }

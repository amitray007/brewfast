//go:build !unix

package handoff

// termOwner is a no-op on platforms without POSIX terminal process groups.
// There is no SIGTTIN stop to avoid, so there is nothing to transfer.
type termOwner struct{}

func acquireTerminal(int) *termOwner { return &termOwner{} }

func (t *termOwner) restore() {}

//go:build !linux

package agent

// Virtual terminals only exist on Linux.
type vtSwitcher struct{}

func newVTSwitcher() *vtSwitcher      { return &vtSwitcher{} }
func (v *vtSwitcher) activate() error { return nil }
func (v *vtSwitcher) release()        {}

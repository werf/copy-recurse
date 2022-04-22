//go:build windows
// +build windows

package copyrec

import "context"

func New(src, dest string, opts Options) (*CopyRecurse, error) {
	panic("not supported on Windows")
}

func (c *CopyRecurse) Run(ctx context.Context) error {
	panic("not supported on Windows")
}

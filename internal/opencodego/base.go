//go:build !hostcompat

package opencodego

import "context"

// Production artifacts have no alternate upstream or arbitrary URL setting.
func selectedBase() string                        { return upstreamBase }
func configureBase(context.Context, []byte) error { return nil }

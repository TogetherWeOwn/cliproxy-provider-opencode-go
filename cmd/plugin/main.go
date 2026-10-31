// Package main provides an experimental auth-only ABI entry point.
package main

import (
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/TogetherWeOwn/cliproxy-provider-opencode-go/internal/opencodego"
)

func main() {}

func init() {
	if err := cabi.Export(opencodego.NewRuntime); err != nil {
		panic("cannot install provider factory")
	}
}

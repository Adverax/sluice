//go:build tools

// Package tools pins the build-time code generation tools so they are tracked
// in go.mod and reproducible across environments (ADR-0011). It is never
// compiled into a binary — the `tools` build tag excludes it from normal builds.
package tools

import (
	// oapi-codegen v2 generates the OpenAPI HTTP boundary (see internal/api).
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)

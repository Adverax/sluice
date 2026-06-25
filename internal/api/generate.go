// Package api holds the OpenAPI-generated HTTP boundary for the sluice gateway
// (ADR-0011, contract-first). The generated code in this package — request/
// response/error types, the StrictServerInterface, and the net/http
// HandlerFromMux that registers routes on a *http.ServeMux (CON-001) — is
// produced from api/openapi.yaml by oapi-codegen v2.
//
// Regenerate with `make generate` (or `go generate ./...`). The generated file
// (api.gen.go) is committed; CI checks it is in sync with the spec.
package api

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen --config=../../oapi-codegen.yaml ../../api/openapi.yaml

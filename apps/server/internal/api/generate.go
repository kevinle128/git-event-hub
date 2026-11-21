// Package api holds the HTTP types and the Echo server interface generated
// from api/openapi.yaml. Run `go generate ./internal/api` after a change to
// the spec.
package api

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config ../../oapi-codegen.yaml ../../api/openapi.yaml

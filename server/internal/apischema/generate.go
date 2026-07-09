// Package apischema holds the request/response types oapi-codegen generates
// from api/openapi.yaml: the beginning of the Go server's half of the
// "spec-first, so frontend and backend cannot drift" contract
// (docs/02-architecture.md, docs/12 Milestone 0), mirroring
// web/package.json's generate:api on the TypeScript side. CI regenerates
// this file and fails if it differs from what's checked in, so the type
// definitions themselves can never silently drift from api/openapi.yaml.
//
// Handler and service code across the server now builds its responses
// directly from these generated types rather than hand-written structs
// (see ThingsToDo.txt for the migration history) - each package needed its
// own per-field review rather than a mechanical find/replace, since several
// hand-written wire types differed from what oapi-codegen produces (e.g.
// float64-vs-float32 scores, and plain strings vs. the generated UUID/enum
// types).
package apischema

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen-config.yaml ../../../api/openapi.yaml

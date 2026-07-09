// Package apischema holds the request/response types oapi-codegen generates
// from api/openapi.yaml: the beginning of the Go server's half of the
// "spec-first, so frontend and backend cannot drift" contract
// (docs/02-architecture.md, docs/12 Milestone 0), mirroring
// web/package.json's generate:api on the TypeScript side. CI regenerates
// this file and fails if it differs from what's checked in, so the type
// definitions themselves can never silently drift from api/openapi.yaml.
//
// Handler and service code has not yet been migrated to construct
// responses from these generated types (see ThingsToDo.txt) - that is a
// larger, deliberately separate follow-up, since several existing
// hand-written wire types differ from what oapi-codegen produces in ways
// that need per-field review, not a mechanical find/replace (e.g.
// analytics.QuizStats.Mean is *float64 passed straight from a DB scan,
// while apischema.QuizStats.Mean is *float32; several fields pass raw
// json.RawMessage columns through untouched where the generated type has a
// fully concrete nested struct).
package apischema

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen-config.yaml ../../../api/openapi.yaml

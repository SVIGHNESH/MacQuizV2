// Package authusers owns accounts, sessions, JWT issuance, groups, and the
// central policy function can(actor, action, resource) that every route and
// every WebSocket channel subscribe must consult.
//
// Boundary (docs/02-architecture.md section 3): it never contains quiz or
// attempt logic. Unassigned resources answer 404, not 403, so existence is
// never leaked.
package authusers

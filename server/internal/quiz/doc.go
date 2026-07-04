// Package quiz owns quiz authoring: draft CRUD, question CRUD, bulk imports,
// publish-time snapshotting/versioning, scheduling windows, and assignments.
//
// Boundary (docs/02-architecture.md section 3): it never grades and never
// touches attempt state. Publishing copies the question set into an immutable
// version; attempts pin the version they ran against.
//
// Serializer invariant: questions.correct never reaches a student client;
// the student-facing serializer strips it and tests enforce that from day one.
package quiz

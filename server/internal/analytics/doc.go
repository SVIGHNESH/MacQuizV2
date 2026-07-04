// Package analytics owns the quiz_stats and student_stats rollups (computed
// by queue jobs when a quiz closes), reporting queries, and CSV exports.
//
// Boundary (docs/02-architecture.md section 3): it only reads the
// transactional tables and writes its own rollup tables, so reporting load
// can move to a read replica without code changes.
package analytics

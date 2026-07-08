// Package realtime is the WebSocket gateway: socket lifecycle, channel
// subscribe authorization (quiz:{id}:monitor, attempt:{id}, and
// user:{id}:notify - all three now implemented), and fan-out of events
// published on Redis pub/sub.
//
// Boundary (docs/02-architecture.md section 3): it relays, it never decides.
// Every event is first persisted as an attempt_events row by the attempt
// module, then published; the gateway only forwards. This module is designed
// to be split into its own process when sustained concurrency passes ~3-4k
// sockets - a deployment change, not a design change.
package realtime

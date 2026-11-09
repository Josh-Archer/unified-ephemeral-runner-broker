// Package scaleset is the broker's Runner Scale Set message listener.
//
// The poll, ack, and message shapes follow actions/scaleset v0.5.0
// (github.com/actions/scaleset/listener). This package does not import that
// module: it requires Go 1.27 and pulls the examples' docker and cobra graph.
// A later cutover can adapt scaleset.Client to Client once that toolchain is
// acceptable. Until then the broker long-polls a message session it was given
// and scales from Statistics.TotalAssignedJobs.
//
// Messages are acknowledged only after Scale returns nil. A failed Scale
// leaves the message in the session so the next poll replays it.
package scaleset

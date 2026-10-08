// Package boxdapi is the generated client for the part of boxd's public gRPC
// API that the boxd driver uses.
//
// It registers its messages under boxd's own names (boxd.api.v1.*), which the
// wire format needs. A program that also links boxd's official Go stubs would
// register them twice, which protobuf-go answers with a panic at startup, so
// such a program has to use one or the other.
package boxdapi

// buf and both protoc plugins are go.mod tools.
//go:generate go tool buf generate

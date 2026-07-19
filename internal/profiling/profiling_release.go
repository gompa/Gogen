//go:build !debug

// Package profiling provides opt-in CPU and memory profiling for debug
// builds only. This file is compiled for normal (non-debug) builds and
// contains no-ops, so no pprof code, flags, or overhead ship in
// production binaries. Build with `-tags debug` to get the real
// implementation and the -cpuprofile/-memprofile flags.
package profiling

// Start is a no-op in production builds.
func Start() {}

// Stop is a no-op in production builds.
func Stop() {}

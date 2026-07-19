//go:build debug

// Package profiling provides opt-in CPU and memory profiling for debug
// builds only. Build with `-tags debug` to enable the -cpuprofile and
// -memprofile flags; a normal `go build` compiles this package out
// entirely (see profiling_release.go) so no pprof code or flags ship in
// production binaries.
package profiling

import (
	"flag"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
)

var (
	cpuProfile = flag.String("cpuprofile", "", "(debug builds only) write CPU profile to file")
	memProfile = flag.String("memprofile", "", "(debug builds only) write memory profile to file")
)

var stopCPU func()

// Start begins CPU profiling if -cpuprofile was set. Safe to call
// unconditionally; it's a no-op if the flag is empty. Callers should
// `defer profiling.Stop()` immediately after calling Start.
func Start() {
	if *cpuProfile == "" {
		return
	}
	f, err := os.Create(*cpuProfile)
	if err != nil {
		log.Fatalf("profiling: could not create CPU profile: %v", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		log.Fatalf("profiling: could not start CPU profile: %v", err)
	}
	stopCPU = func() {
		pprof.StopCPUProfile()
		f.Close()
	}
	log.Printf("profiling: writing CPU profile to %s", *cpuProfile)
}

// Stop stops CPU profiling (if running) and writes a heap profile if
// -memprofile was set. Safe to call unconditionally, including when
// Start was never called or the flags were left empty.
func Stop() {
	if stopCPU != nil {
		stopCPU()
		stopCPU = nil
	}
	if *memProfile == "" {
		return
	}
	f, err := os.Create(*memProfile)
	if err != nil {
		log.Printf("profiling: could not create memory profile: %v", err)
		return
	}
	defer f.Close()
	runtime.GC() // get up-to-date statistics
	if err := pprof.WriteHeapProfile(f); err != nil {
		log.Printf("profiling: could not write memory profile: %v", err)
		return
	}
	log.Printf("profiling: writing memory profile to %s", *memProfile)
}

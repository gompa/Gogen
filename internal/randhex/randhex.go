// Package randhex generates small random-hex identifiers with a deterministic
// timestamp fallback when crypto/rand is unavailable.
package randhex

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

// fallbackCounter disambiguates timestamp-based fallback ids when
// crypto/rand fails: two ids generated in the same nanosecond (e.g. two
// approvals during one delete) still differ.
var fallbackCounter atomic.Uint64

// MaxBytes is the largest number of random bytes ID can encode.
const MaxBytes = 32

// ID returns a random id: hex.EncodeToString of n random bytes, prefixed with
// prefix (e.g. "job-"). If crypto/rand fails, it falls back to
// "<prefix><unixnano>-<counter>", unique within the process. Having the
// fallback in one place keeps the id formats from drifting across the session,
// background-job, and approval id generators.
//
// n must be in [1, MaxBytes]. Out-of-range values are a programming error and
// panic rather than silently generating less entropy than requested.
func ID(n int, prefix string) string {
	if n < 1 || n > MaxBytes {
		panic(fmt.Sprintf("randhex.ID: n must be between 1 and %d, got %d", MaxBytes, n))
	}
	var b [MaxBytes]byte
	if _, err := rand.Read(b[:n]); err != nil {
		return fmt.Sprintf("%s%d-%d", prefix, time.Now().UnixNano(), fallbackCounter.Add(1))
	}
	return prefix + hex.EncodeToString(b[:n])
}

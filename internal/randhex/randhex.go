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

// ID returns a random id: hex.EncodeToString of n random bytes, prefixed with
// prefix (e.g. "job-"). If crypto/rand fails, it falls back to
// "<prefix><unixnano>-<counter>", unique within the process. Having the
// fallback in one place keeps the id formats from drifting across the session,
// background-job, and approval id generators.
func ID(n int, prefix string) string {
	var b [32]byte
	if n < 1 || n > len(b) {
		n = 16
	}
	if _, err := rand.Read(b[:n]); err != nil {
		return fmt.Sprintf("%s%d-%d", prefix, time.Now().UnixNano(), fallbackCounter.Add(1))
	}
	return prefix + hex.EncodeToString(b[:n])
}

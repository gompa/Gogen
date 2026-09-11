//go:build !windows

package ioutil

// isTransientSharingErr is always false off Windows: the sharing violations it
// covers are a Windows-only consequence of opening files without
// FILE_SHARE_DELETE (POSIX has no such open-time sharing mode). ReadFileRetry
// is therefore a plain single-attempt os.ReadFile on these platforms.
func isTransientSharingErr(error) bool { return false }

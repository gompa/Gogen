package agent

import (
	"context"
	"os"
	"path/filepath"
)

// walkOpts configures walkTree.
type walkOpts struct {
	// glob, when non-empty, restricts visits to files whose workspace-relative
	// path matches the glob (matchGlobPattern).
	glob string

	// checkReadable runs the stat/size/binary probe (searchableWalkFile)
	// before visiting files. Name-only walkers (GlobFiles, RepoOverview,
	// ListFiles, FindFile) leave it off.
	checkReadable bool

	// includeDirs also visits directories that pass skip filtering (after
	// their rel path is computed). Only ListFiles needs this, to emit
	// directory entries.
	includeDirs bool

	// includeHidden also visits hidden FILES (dotfiles). Hidden/vendor
	// DIRECTORIES are still pruned either way. Only GlobFiles uses this: as a
	// name-based discovery tool it must keep matching dotfiles (e.g. .env,
	// .gitignore via ".*" or "*.env"); content/listing walkers keep the
	// ripgrep-like default of skipping them.
	includeHidden bool
}

// walkTree is the shared file-walk skeleton used by every Executor tree
// walker (ListFiles, GlobFiles, RepoOverview, SearchCode's Go fallback,
// ReplaceInTree, FindFile, walkSymbolReferences, walkSymbolReferencesText,
// findDefinitionAST, renameWithAST, renameWithText). One place owns the
// policy that would otherwise drift between the copies:
//
//   - hidden files are skipped unless opts.includeHidden is set, and
//     hidden/vendor dirs are always pruned via filepath.SkipDir,
//   - the root itself is never visited,
//   - ctx cancellation aborts the walk (when ctx is non-nil),
//   - rel is the workspace-relative slash path with relPrefix applied, and
//   - the glob filter and (optionally) the size/binary probe run before the
//     visitor is called.
//
// visit receives the absolute path, the workspace-relative path, and the
// DirEntry. Returning a sentinel error from visit (errExploreTruncated,
// errFindFileLimit) stops the walk and is propagated unchanged; any other
// non-nil error also stops the walk.
func walkTree(ctx context.Context, searchRoot, relPrefix string, opts walkOpts, visit func(path, rel string, d os.DirEntry) error) error {
	return filepath.WalkDir(searchRoot, func(walkPath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if walkPath == searchRoot {
			return nil
		}
		if d.IsDir() {
			if shouldSkipSearchEntry(d.Name(), true) {
				return filepath.SkipDir
			}
			if !opts.includeDirs {
				return nil
			}
		} else if !opts.includeHidden && shouldSkipSearchEntry(d.Name(), false) {
			return nil
		}
		rel, err := filepath.Rel(searchRoot, walkPath)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if relPrefix != "" {
			rel = filepath.ToSlash(filepath.Join(relPrefix, rel))
		}
		if opts.glob != "" && !matchGlobPattern(opts.glob, rel) {
			return nil
		}
		if opts.checkReadable && !d.IsDir() {
			info, err := d.Info()
			if !searchableWalkFile(walkPath, info, err) {
				return nil
			}
		}
		return visit(walkPath, rel, d)
	})
}

package projectfile

import (
	"path/filepath"

	"gogen/internal/config"
)

// LoadEffective loads the project file from workingDir, merges config, and attaches guidelines metadata.
func LoadEffective(workingDir string, flags FlagOverrides) (*config.Config, error) {
	pf, err := LoadFromWorkingDir(workingDir)
	if err != nil {
		return nil, err
	}
	cfg := Merge(pf, flags)
	if pf != nil {
		cfg.ProjectGuidelines = pf.Guidelines
		cfg.ProjectFilePath = pf.Path
	}
	if cfg.WorkingDir == "" || cfg.WorkingDir == "." {
		cfg.WorkingDir = workingDir
	}
	abs, err := filepath.Abs(cfg.WorkingDir)
	if err == nil {
		cfg.WorkingDir = abs
	}
	return cfg, nil
}

// GuidelinesHeader formats project guidelines for the system prompt.
func GuidelinesHeader(path, guidelines string) string {
	if guidelines == "" {
		return ""
	}
	name := path
	if name == "" {
		name = "project file"
	}
	return "\n\nProject rules (" + name + "):\n" + guidelines
}

package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkProject(t *testing.T) (root, proj string) {
	t.Helper()
	root = t.TempDir()
	proj = filepath.Join(root, "proj")
	// A .git marker makes proj the project root (skills discovery walks up
	// to it), isolating the test from the temp-dir ancestors.
	writeSkill(t, filepath.Join(proj, ".git"), "gitdir: .")
	return root, proj
}

// TestListBundleAndFlat covers both skill forms, sorting, and the
// description from front matter.
func TestListBundleAndFlat(t *testing.T) {
	_, proj := mkProject(t)
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "review", "SKILL.md"),
		"---\ndescription: Code review checklist\n---\n# Review\n\nChecklist body")
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "build.md"), "Build procedure")

	m := NewManager(proj, false)
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("list = %+v, want 2 skills", list)
	}
	if list[0].Name != "build" || list[1].Name != "review" {
		t.Fatalf("sorted names = %v, want [build review]", []string{list[0].Name, list[1].Name})
	}
	if list[1].Description != "Code review checklist" {
		t.Fatalf("review description = %q", list[1].Description)
	}
	if list[1].Body != "# Review\n\nChecklist body" {
		t.Fatalf("review body = %q", list[1].Body)
	}
	if list[0].Source != "project" {
		t.Fatalf("source = %q, want project", list[0].Source)
	}
}

// TestBundleWinsFlat pins the precedence: a bundle dir beats a flat file of
// the same name.
func TestBundleWinsFlat(t *testing.T) {
	_, proj := mkProject(t)
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "check", "SKILL.md"), "bundle body")
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "check.md"), "flat body")

	m := NewManager(proj, false)
	list := m.List()
	if len(list) != 1 || list[0].Body != "bundle body" {
		t.Fatalf("list = %+v, want the bundle only", list)
	}
}

// TestProjectWinsDuplicateName pins the root precedence: the project root
// claims a name before the user root.
func TestProjectWinsDuplicateName(t *testing.T) {
	_, proj := mkProject(t)
	user := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("XDG_CONFIG_HOME", user)
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "fmt.md"), "project body")
	writeSkill(t, filepath.Join(user, "gogen", "skills", "fmt.md"), "user body")

	m := NewManager(proj, false)
	list := m.List()
	if len(list) != 1 || list[0].Body != "project body" || list[0].Source != "project" {
		t.Fatalf("list = %+v, want the project skill only", list)
	}
}

// TestInvalidAndHiddenSkipped pins name validation: hidden entries, invalid
// names, and non-skill files are ignored.
func TestInvalidAndHiddenSkipped(t *testing.T) {
	_, proj := mkProject(t)
	root := filepath.Join(proj, ".gogen", "skills")
	writeSkill(t, filepath.Join(root, ".hidden", "SKILL.md"), "hidden bundle")
	writeSkill(t, filepath.Join(root, ".hidden.md"), "hidden flat")
	writeSkill(t, filepath.Join(root, "Bad Name", "SKILL.md"), "bad bundle")
	writeSkill(t, filepath.Join(root, "BadName.md"), "bad flat")
	writeSkill(t, filepath.Join(root, "README.md"), "not a skill")
	writeSkill(t, filepath.Join(root, "notes.txt"), "not a skill")

	m := NewManager(proj, false)
	if len(m.List()) != 0 {
		t.Fatalf("list = %+v, want none", m.List())
	}
}

// TestOversizedBodySkipped pins the per-skill byte cap.
func TestOversizedBodySkipped(t *testing.T) {
	_, proj := mkProject(t)
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "big.md"), strings.Repeat("a", MaxSkillBodyBytes+1))
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "ok.md"), "fine")

	m := NewManager(proj, false)
	list := m.List()
	if len(list) != 1 || list[0].Name != "ok" {
		t.Fatalf("list = %+v, want only the small skill", list)
	}
}

// TestGlobalModeUserRootOnly pins that global mode scans only the user root.
func TestGlobalModeUserRootOnly(t *testing.T) {
	_, proj := mkProject(t)
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "proj.md"), "project body")
	user := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("XDG_CONFIG_HOME", user)
	writeSkill(t, filepath.Join(user, "gogen", "skills", "user.md"), "user body")

	m := NewManager(proj, true)
	list := m.List()
	if len(list) != 1 || list[0].Name != "user" {
		t.Fatalf("global list = %+v, want only the user skill", list)
	}
}

// TestRead pins Read: exact kebab-case lookup and error behavior.
func TestRead(t *testing.T) {
	_, proj := mkProject(t)
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "review", "SKILL.md"), "body")

	m := NewManager(proj, false)
	s, err := m.Read("review")
	if err != nil || s.Body != "body" {
		t.Fatalf("Read = %+v, %v", s, err)
	}
	if _, err := m.Read("missing"); err == nil {
		t.Fatal("missing skill should error")
	}
	if _, err := m.Read("Bad Name"); err == nil {
		t.Fatal("invalid name should error")
	}
}

// TestSetWorkingDirReRoots pins working-dir re-targeting: after
// SetWorkingDir the manager discovers skills from the new project root
// (the user root is unchanged).
func TestSetWorkingDirReRoots(t *testing.T) {
	base := t.TempDir()
	projA := filepath.Join(base, "a")
	writeSkill(t, filepath.Join(projA, ".git"), "gitdir: .")
	writeSkill(t, filepath.Join(projA, ".gogen", "skills", "from-a.md"), "skill a")
	projB := filepath.Join(base, "b")
	writeSkill(t, filepath.Join(projB, ".git"), "gitdir: .")
	writeSkill(t, filepath.Join(projB, ".gogen", "skills", "from-b.md"), "skill b")

	m := NewManager(projA, false)
	if list := m.List(); len(list) != 1 || list[0].Name != "from-a" {
		t.Fatalf("initial list = %+v, want [from-a]", list)
	}
	m.SetWorkingDir(projB)
	list := m.List()
	if len(list) != 1 || list[0].Name != "from-b" {
		t.Fatalf("after re-root list = %+v, want [from-b]", list)
	}
}

// TestNoRoots returns an empty list, never an error.
func TestNoRoots(t *testing.T) {
	_, proj := mkProject(t)
	m := NewManager(proj, false)
	if list := m.List(); len(list) != 0 {
		t.Fatalf("list = %+v, want none", list)
	}
}

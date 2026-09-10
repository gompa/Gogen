package main

import "testing"

func TestResolvePositionalArgs(t *testing.T) {
	tests := []struct {
		name       string
		dir        string
		prompt     string
		args       []string
		wantDir    string
		wantPrompt string
		wantUseErr bool
	}{
		{
			name:       "no args no flags",
			wantDir:    ".",
			wantPrompt: "",
		},
		{
			name:       "dir positional only",
			args:       []string{"/proj"},
			wantDir:    "/proj",
			wantPrompt: "",
		},
		{
			name:       "dir and prompt positional",
			args:       []string{"/proj", "fix the bug"},
			wantDir:    "/proj",
			wantPrompt: "fix the bug",
		},
		{
			name:       "extra positional is usage error",
			args:       []string{"/proj", "a", "b"},
			wantDir:    "/proj",
			wantUseErr: true,
		},
		{
			name:       "flag dir keeps first positional as prompt",
			dir:        "/proj",
			args:       []string{"fix the bug"},
			wantDir:    "/proj",
			wantPrompt: "fix the bug",
		},
		{
			name:       "flag dir with extra positionals is usage error",
			dir:        "/proj",
			args:       []string{"a", "b"},
			wantDir:    "/proj",
			wantUseErr: true,
		},
		{
			name:       "flag dir with no positionals",
			dir:        "/proj",
			wantDir:    "/proj",
			wantPrompt: "",
		},
		{
			name:       "prompt flag wins over positional prompt",
			prompt:     "from flag",
			args:       []string{"/proj", "from position"},
			wantDir:    "/proj",
			wantPrompt: "from flag",
		},
		{
			name:       "prompt flag with dir flag",
			dir:        "/proj",
			prompt:     "from flag",
			args:       []string{"from position"},
			wantDir:    "/proj",
			wantPrompt: "from flag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDir, gotPrompt, err := resolvePositionalArgs(tt.dir, tt.prompt, tt.args)
			if tt.wantUseErr {
				if err == nil {
					t.Fatalf("resolvePositionalArgs(%q, %q, %v) error = nil, want usage error", tt.dir, tt.prompt, tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePositionalArgs(%q, %q, %v) unexpected error: %v", tt.dir, tt.prompt, tt.args, err)
			}
			if gotDir != tt.wantDir {
				t.Errorf("workingDir = %q, want %q", gotDir, tt.wantDir)
			}
			if gotPrompt != tt.wantPrompt {
				t.Errorf("prompt = %q, want %q", gotPrompt, tt.wantPrompt)
			}
		})
	}
}

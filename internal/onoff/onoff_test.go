package onoff

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool // expected on
		ok   bool // expected ok
	}{
		{"on", "on", true, true},
		{"ON", "ON", true, true},
		{"On with spaces", "  On  ", true, true},
		{"one", "1", true, true},
		{"true", "true", true, true},
		{"TRUE", "TRUE", true, true},
		{"yes", "yes", true, true},
		{"off", "off", false, true},
		{"zero", "0", false, true},
		{"false", "false", false, true},
		{"no", "no", false, true},
		{"empty", "", false, false},
		{"blank", "   ", false, false},
		{"garbage", "maybe", false, false},
		{"onish", "online", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			on, ok := Parse(tt.in)
			if on != tt.want || ok != tt.ok {
				t.Errorf("Parse(%q) = (%v, %v), want (%v, %v)", tt.in, on, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestEnabled(t *testing.T) {
	for _, on := range []string{"on", "ON", "1", "true", "yes"} {
		if !Enabled(on) {
			t.Errorf("Enabled(%q) = false, want true", on)
		}
	}
	for _, off := range []string{"off", "0", "false", "no", "", "maybe", " "} {
		if Enabled(off) {
			t.Errorf("Enabled(%q) = true, want false", off)
		}
	}
}

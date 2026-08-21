package agent

import "testing"

// TestApplySubagentThinkingLevel pins the spawn-time reasoning-effort
// cascade shared by the web and TUI spawners: the configured level wins;
// empty inherits the parent's live level; "off" and empty set nothing; a
// level the child's FINAL model does not accept is omitted (policy B) —
// the tool's model argument can override the configured model, so validity
// is resolved against the child's model at spawn time.
func TestApplySubagentThinkingLevel(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		parent     ThinkingLevel
		childModel string
		want       ThinkingLevel
	}{
		{name: "configured-wins-over-parent", configured: "max", parent: ThinkingHigh, childModel: "glm-5.2", want: ThinkingLevel("max")},
		// The "sets nothing" cases want the zero value (""): the test
		// children are bare Agent literals, whose zero level means off
		// (NewAgent seeds ThinkingOff for real sessions).
		{name: "empty-inherits-parent", configured: "", parent: ThinkingMedium, childModel: "selfhosted", want: ThinkingMedium},
		{name: "empty-parent-off-sets-nothing", configured: "", parent: ThinkingOff, childModel: "selfhosted", want: ""},
		{name: "configured-off-sets-nothing", configured: "off", parent: ThinkingHigh, childModel: "selfhosted", want: ""},
		{name: "configured-not-accepted-omitted", configured: "low", parent: ThinkingOff, childModel: "glm-5.2", want: ""},
		{name: "inherited-not-accepted-omitted", configured: "", parent: ThinkingLevel("max"), childModel: "selfhosted", want: ""},
		{name: "configured-case-normalized", configured: " HIGH ", parent: ThinkingOff, childModel: "glm-5.2", want: ThinkingHigh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := &Agent{Provider: &effortStub{model: "selfhosted"}, ThinkingLevel: tc.parent}
			child := &Agent{Provider: &effortStub{model: tc.childModel}}
			ApplySubagentThinkingLevel(child, parent, tc.configured)
			if child.ThinkingLevel != tc.want {
				t.Fatalf("child level = %q, want %q", child.ThinkingLevel, tc.want)
			}
		})
	}
}

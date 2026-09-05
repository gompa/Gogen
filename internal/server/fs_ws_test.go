package server

import "testing"

// TestFSOpTables verifies the invariants the FS/Git dispatch tables rely on:
// every entry has a run function and stamps the conventional "<type>_result"
// reply envelope, the two tables are disjoint, and every table entry is
// routed by wsHandlers with the matching kind (so adding an op without
// registering it on the editor and main-chat sockets fails here instead of
// silently dropping the message).
func TestFSOpTables(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  map[string]fsOp
		kind wsHandlerKind
	}{
		{name: "fsReadOps", ops: fsReadOps, kind: wsKindFSRead},
		{name: "fsWriteOps", ops: fsWriteOps, kind: wsKindFSWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for typ, op := range tc.ops {
				if want := typ + "_result"; op.resultType != want {
					t.Errorf("%s[%q].resultType = %q, want %q", tc.name, typ, op.resultType, want)
				}
				if op.run == nil {
					t.Errorf("%s[%q].run = nil, want non-nil", tc.name, typ)
				}
				if e := wsHandlers[typ]; e.kind != tc.kind {
					t.Errorf("%s[%q] is not routed by wsHandlers with kind %q (got %q); the message would be dropped on at least one socket", tc.name, typ, tc.kind, e.kind)
				}
			}
		})
	}

	// A type in both tables would make the second table's op unreachable
	// (the sockets route by list lookup, not by table order).
	seen := make(map[string]string, len(fsReadOps)+len(fsWriteOps))
	for typ := range fsReadOps {
		seen[typ] = "fsReadOps"
	}
	for typ := range fsWriteOps {
		if prev, ok := seen[typ]; ok {
			t.Errorf("type %q is in both %s and fsWriteOps", typ, prev)
		}
	}
}

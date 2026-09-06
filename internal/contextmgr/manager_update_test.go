package contextmgr

import "testing"

// TestManagerUpdateSettings verifies the runtime context-settings swap:
// values apply immediately, zero values stay meaningful (same semantics as
// NewManager), and the snapshot reflects the update.
func TestManagerUpdateSettings(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{
		ContextLimit:              10000,
		CompactThreshold:          0.5,
		CompactKeepRecentMessages: 4,
		MaxToolResultBytes:        8192,
		CompactReserveTokens:      500,
	})
	if got := m.ContextLimit(); got != 10000 {
		t.Fatalf("limit = %d, want 10000", got)
	}

	m.UpdateSettings(Settings{
		ContextLimit:              20000,
		CompactThreshold:          0,
		CompactKeepRecentMessages: 0,
		MaxToolResultBytes:        0,
		CompactReserveTokens:      0,
	})
	snap := m.SettingsSnapshot()
	if snap.ContextLimit != 20000 || snap.CompactThreshold != 0 || snap.CompactKeepRecentMessages != 0 ||
		snap.MaxToolResultBytes != 0 || snap.CompactReserveTokens != 0 {
		t.Fatalf("snapshot after update = %+v, want explicit zeros preserved", snap)
	}
	if m.AutoCompactEnabled() {
		t.Fatal("threshold 0 should disable auto-compaction")
	}
	if m.CompactKeepRecentMessages() != 0 {
		t.Fatalf("keep = %d, want 0", m.CompactKeepRecentMessages())
	}
	if got := m.ContextLimit(); got != 20000 {
		t.Fatalf("limit after update = %d, want 20000", got)
	}

	// Negative values fall back to defaults (normalizeSettings).
	m.UpdateSettings(Settings{CompactThreshold: -1, CompactKeepRecentMessages: -2})
	snap = m.SettingsSnapshot()
	if snap.CompactThreshold != DefaultSettings().CompactThreshold {
		t.Fatalf("negative threshold not clamped: %v", snap.CompactThreshold)
	}

	// ContextLimit 0 returns to provider resolution (limitResolved cleared).
	m.UpdateSettings(Settings{ContextLimit: 0})
	if got := m.ContextLimit(); got == 20000 {
		t.Fatal("limit should be re-resolved after reset to 0")
	}
}

// TestUpdateSettingsPreservesUnchangedLimit pins the per-field push
// contract: a settings update that does NOT touch ContextLimit keeps the
// session's current limit AND its manual/resolved state. In particular a
// provider-resolved limit (SetContextLimit, e.g. session restore) must not
// become a manual pin — RefreshAfterModelChange must still re-resolve it
// for a newly selected model.
func TestUpdateSettingsPreservesUnchangedLimit(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{})
	// Restore-path shape: the limit is resolved from the provider, not
	// configured by the user (manualContextLimit stays 0).
	m.SetContextLimit(10000)

	// A settings push that changes only the compact threshold: the server's
	// per-field merge sends the session's current ContextLimit back.
	cur := m.SettingsSnapshot()
	cur.CompactThreshold = 0.4
	m.UpdateSettings(cur)

	if got := m.ContextLimit(); got != 10000 {
		t.Fatalf("unchanged limit = %d, want 10000 preserved", got)
	}
	// The limit must NOT have become manual: a model change re-resolves it
	// from the provider (stub returns 128000).
	m.RefreshAfterModelChange(t.Context())
	if got := m.ContextLimit(); got != 128000 {
		t.Fatalf("limit after model change = %d, want re-resolved 128000 (not pinned at 10000)", got)
	}
}

// TestUpdateSettingsPreservesManualPin covers the other half of the
// distinction: a user-configured (manual) limit survives a push that does
// not touch ContextLimit and stays manual, so a model change keeps it.
func TestUpdateSettingsPreservesManualPin(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{ContextLimit: 200000})
	cur := m.SettingsSnapshot()
	cur.CompactKeepRecentMessages = 6
	m.UpdateSettings(cur)
	if got := m.ContextLimit(); got != 200000 {
		t.Fatalf("manual limit = %d, want 200000 preserved", got)
	}
	m.RefreshAfterModelChange(t.Context())
	if got := m.ContextLimit(); got != 200000 {
		t.Fatalf("manual limit after model change = %d, want 200000 kept", got)
	}
}

// TestUpdateSettingsFuncMergesAgainstStoredSettings pins the atomic
// read-modify-write contract: fn is handed the CURRENTLY STORED settings
// (not a caller-side copy) and the merged result is stored under the same
// lock. This is what makes the server's per-field push safe against
// interleaving — with the old separate SettingsSnapshot + UpdateSettings
// pair, a push whose snapshot predated another push's store silently
// reverted that push's field.
func TestUpdateSettingsFuncMergesAgainstStoredSettings(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{ContextLimit: 5000})

	// Push 1 (field A) lands first.
	m.UpdateSettingsFunc(func(cur Settings) Settings {
		cur.CompactReserveTokens = 2222
		return cur
	})
	// Push 2 carries a settings copy snapshotted BEFORE push 1 stored (the
	// concurrent-interleave shape): its copy fn sets only its own field and
	// must observe push 1's value in the cur it is handed.
	m.UpdateSettingsFunc(func(cur Settings) Settings {
		if cur.CompactReserveTokens != 2222 {
			t.Errorf("fn saw CompactReserveTokens = %d, want 2222 (must merge against stored settings, not a stale copy)", cur.CompactReserveTokens)
		}
		cur.MaxToolResultBytes = 1111
		return cur
	})
	snap := m.SettingsSnapshot()
	if snap.CompactReserveTokens != 2222 || snap.MaxToolResultBytes != 1111 {
		t.Fatalf("after two single-field pushes: reserve=%d max=%d, want 2222/1111 (a field was reverted)", snap.CompactReserveTokens, snap.MaxToolResultBytes)
	}
	if snap.ContextLimit != 5000 {
		t.Fatalf("ContextLimit = %d, want 5000 untouched", snap.ContextLimit)
	}
}

// TestUpdateSettingsFuncLimitSemantics verifies UpdateSettingsFunc applies
// the same ContextLimit manual/resolved rules as UpdateSettings: an
// unchanged limit preserves the resolved state (a model change re-resolves
// it), an explicitly changed value pins it as manual, and normalizeSettings
// clamps the merged result.
func TestUpdateSettingsFuncLimitSemantics(t *testing.T) {
	m := NewManager(&stubProvider{}, Settings{})
	m.SetContextLimit(10000) // provider-resolved, NOT manual

	m.UpdateSettingsFunc(func(cur Settings) Settings {
		cur.CompactThreshold = 0.4
		return cur
	})
	m.RefreshAfterModelChange(t.Context())
	if got := m.ContextLimit(); got != 128000 {
		t.Fatalf("limit after model change = %d, want re-resolved 128000 (not pinned)", got)
	}

	m.UpdateSettingsFunc(func(cur Settings) Settings {
		cur.ContextLimit = 7777
		return cur
	})
	m.RefreshAfterModelChange(t.Context())
	if got := m.ContextLimit(); got != 7777 {
		t.Fatalf("limit after model change = %d, want manual 7777 kept", got)
	}

	m.UpdateSettingsFunc(func(cur Settings) Settings {
		cur.CompactThreshold = -1
		return cur
	})
	if got := m.SettingsSnapshot().CompactThreshold; got != DefaultSettings().CompactThreshold {
		t.Fatalf("threshold after invalid value = %v, want default", got)
	}
}

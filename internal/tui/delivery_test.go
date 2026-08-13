package tui

import (
	"context"
	"fmt"
	"testing"
)

// TestDrainDeliveriesOnlyWhenIdle pins the TUI delivery guard: queued
// system messages are only drained when no turn is streaming, and each
// drain starts exactly one delivery turn.
func TestDrainDeliveriesOnlyWhenIdle(t *testing.T) {
	m := &Model{streaming: true, ctx: context.Background()}
	m.viewport.Width = 200
	m.pendingDeliveries = []string{"notice-a", "notice-b"}

	if cmd := m.drainDeliveries(); cmd != nil {
		t.Fatal("must not drain while streaming")
	}
	if len(m.pendingDeliveries) != 2 {
		t.Fatalf("queue mutated while streaming: %v", m.pendingDeliveries)
	}

	m.streaming = false
	cmd := m.drainDeliveries()
	if cmd == nil {
		t.Fatal("must drain when idle")
	}
	if len(m.pendingDeliveries) != 1 || m.pendingDeliveries[0] != "notice-b" {
		t.Fatalf("queue = %v, want [notice-b]", m.pendingDeliveries)
	}
	if !m.streaming {
		t.Fatal("drain must mark the model as streaming")
	}

	// Still streaming (the just-started turn) → the second item waits.
	if cmd := m.drainDeliveries(); cmd != nil {
		t.Fatal("second item must wait for the first turn to end")
	}
	// An empty queue yields no command even when idle.
	m.streaming = false
	m.pendingDeliveries = nil
	if cmd := m.drainDeliveries(); cmd != nil {
		t.Fatal("empty queue must not drain")
	}
}

// TestDeliveryRequestMsgQueues pins the Update-thread entry: deliveryRequestMsg
// appends to the queue and drains immediately when idle.
func TestDeliveryRequestMsgQueues(t *testing.T) {
	m := &Model{streaming: true, ctx: context.Background()}
	m.viewport.Width = 200

	model, cmd := m.handleMsg(deliveryRequestMsg{text: "incoming notice"})
	m2 := model.(*Model)
	if len(m2.pendingDeliveries) != 1 || m2.pendingDeliveries[0] != "incoming notice" {
		t.Fatalf("queue = %v, want [incoming notice]", m2.pendingDeliveries)
	}
	if cmd != nil {
		t.Fatal("must not start a turn while streaming")
	}
}

// TestDeliveryQueueOverflowDropsOldest pins the TUI queue cap: overflow
// drops the OLDEST queued delivery (freshness wins, mirroring the web
// queue) and the drop is reported as a system line at the next drain.
func TestDeliveryQueueOverflowDropsOldest(t *testing.T) {
	m := &Model{streaming: true, ctx: context.Background()}
	m.viewport.Width = 200
	for i := 1; i <= maxPendingDeliveries; i++ {
		m.pendingDeliveries = append(m.pendingDeliveries, fmt.Sprintf("notice-%d", i))
	}

	// Overflow while streaming: the oldest is dropped, the queue stays
	// capped, and nothing drains.
	model, cmd := m.handleMsg(deliveryRequestMsg{text: "notice-9"})
	m2 := model.(*Model)
	if cmd != nil {
		t.Fatal("must not drain while streaming")
	}
	if len(m2.pendingDeliveries) != maxPendingDeliveries || m2.pendingDeliveries[0] != "notice-2" {
		t.Fatalf("queue = %v, want %d items starting at notice-2", m2.pendingDeliveries, maxPendingDeliveries)
	}
	if m2.deliveryDrops != 1 {
		t.Fatalf("deliveryDrops = %d, want 1", m2.deliveryDrops)
	}

	// Idle drain: the drop is reported first, then the next delivery
	// starts (and the queue shrinks by one).
	m2.streaming = false
	if cmd := m2.drainDeliveries(); cmd == nil {
		t.Fatal("must drain when idle")
	}
	if m2.deliveryDrops != 0 {
		t.Fatal("drop counter must reset after rendering")
	}
	if len(m2.pendingDeliveries) != maxPendingDeliveries-1 || m2.pendingDeliveries[0] != "notice-3" {
		t.Fatalf("queue = %v, want %d items starting at notice-3", m2.pendingDeliveries, maxPendingDeliveries-1)
	}
}

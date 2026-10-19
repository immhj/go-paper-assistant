package service

import (
	"sync/atomic"
	"time"
)

// EmergencyStop is the global emergency stop defined in CONTEXT.md. While it is engaged the
// system stops *accepting* inbound messages; it never interrupts tasks that are already
// running. The state is deliberately kept in process memory only, so a restart always
// releases it. See docs/adr/0011-global-emergency-stop-in-process-memory.md.
type EmergencyStop struct {
	// A nil pointer means the system is accepting messages, which keeps the flag and its
	// metadata a single source of truth instead of two fields that can disagree.
	state atomic.Pointer[EmergencyStopState]
}

// EmergencyStopState records when the stop was engaged and why.
type EmergencyStopState struct {
	EngagedAt time.Time `json:"engagedAt"`
	Reason    string    `json:"reason,omitempty"`
}

func NewEmergencyStop() *EmergencyStop { return &EmergencyStop{} }

// Engaged reports whether inbound messages must be rejected.
func (e *EmergencyStop) Engaged() bool { return e.state.Load() != nil }

// State returns the current stop metadata, or nil when the system is accepting messages.
func (e *EmergencyStop) State() *EmergencyStopState { return e.state.Load() }

// Engage stops accepting inbound messages. Engaging an already engaged stop keeps the
// original EngagedAt, so the admin view reports how long the system has really been stopped
// rather than when the button was last pressed.
func (e *EmergencyStop) Engage(reason string) EmergencyStopState {
	next := &EmergencyStopState{EngagedAt: time.Now(), Reason: reason}
	for {
		current := e.state.Load()
		if current != nil {
			return *current
		}
		if e.state.CompareAndSwap(current, next) {
			return *next
		}
	}
}

// Release resumes accepting inbound messages.
func (e *EmergencyStop) Release() { e.state.Store(nil) }

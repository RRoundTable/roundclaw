package activity

import (
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/sdk/temporal"

	"github.com/roundtable/roundclaw/internal/core"
)

// Temporal decides retries from the failure's *type*, not its message, and an
// ordinary error's type is its Go type name. Marking with a sentinel in the
// message therefore looked right and retried anyway — every "no retry will fix
// this" error burned its full policy. This pins the behaviour that matters:
// the flag Temporal actually reads.
func TestNonRetryableIsHonouredByTemporal(t *testing.T) {
	cause := errors.New("unknown agent \"ghost\"")
	err := newNonRetryable(cause)

	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("newNonRetryable returned %T, want a *temporal.ApplicationError", err)
	}
	if !appErr.NonRetryable() {
		t.Error("the error is not marked non-retryable, so Temporal will retry it")
	}
	if appErr.Type() != NonRetryableType {
		t.Errorf("type = %q, want %q — a policy naming this type would not match", appErr.Type(), NonRetryableType)
	}
	// The cause must survive: callers match on it and logs quote it.
	if !errors.Is(err, cause) {
		t.Errorf("the cause was dropped: %v", err)
	}
}

// The wrapping callers use must not hide the marking.
func TestNonRetryableSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("deliver: %w", newNonRetryable(errors.New("boom")))

	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !appErr.NonRetryable() {
		t.Error("wrapping a non-retryable error lost the marking")
	}
}

// The unknown-origin case is the one a rolled-back binary hits: retrying cannot
// teach it a type it does not implement.
func TestDeliverUnknownOriginIsNonRetryable(t *testing.T) {
	a, _, _ := notifyHarness(t)

	err := runDeliver(t, a, DeliverInput{
		AgentID: "dev",
		Origin:  core.Origin{Type: "from-the-future"},
		Result:  core.TurnResult{TurnID: 1, Status: core.TurnDone, Text: "x"},
	})
	if err == nil {
		t.Fatal("an unknown origin was accepted")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !appErr.NonRetryable() {
		t.Errorf("unknown origin error is retryable: %v (%T)", err, err)
	}
}

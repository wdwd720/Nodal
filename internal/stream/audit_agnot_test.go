package stream

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nodal/controlplane/internal/security"
)

// F-185 (was TestAuditAgnot_PublishPanicsWhenASubscriberDisconnectsMidSend on
// audit/agents-notifications @ 8a69aaa, which asserted the panic below).
//
// Hub.Publish sent on a subscriber's channel OUTSIDE h.mu while Hub.Unsubscribe
// closed that same channel UNDER h.mu. A disconnecting client
// (Handler.ServeHTTP's `defer h.hub.Unsubscribe(sub)`) therefore raced a
// concurrent Publish, and a send on a closed channel panics -- in the
// PUBLISHING goroutine, which in cmd/api is the notification follower's ticker.
// An unrecovered panic there takes the whole API process down, and any
// authenticated person could cause it by closing a stream at the right moment.
//
// The window is made deterministic here by filling the subscriber's channel so
// Publish is parked in its grace-period select; in production the same
// collision happens whenever a publish lands while a browser is going away.
// Publish must now return, having delivered nothing, and must not panic.
func TestAuditAgnot_PublishSurvivesASubscriberDisconnectingMidSend(t *testing.T) {
	h := NewHub(16, nil)
	p := security.Principal{SubjectID: "u1", ActorType: security.ActorUser, AccountIDs: []string{"a1"}}

	// bufferSize 1, so the second publish parks in the select.
	sub, _ := h.Subscribe(p, 0, 1)

	h.Publish(Event{Type: TypeNotification, UserID: "u1"}) // fills the channel

	var (
		wg        sync.WaitGroup
		recovered any
		returned  = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(returned)
		defer func() { recovered = recover() }()
		// Parks in `select { case s.ch <- e: case <-s.done: case <-timer.C: }`.
		h.Publish(Event{Type: TypeNotification, UserID: "u1"})
	}()

	// Give the publisher time to park on the send, then do what an SSE handler
	// does when its client goes away.
	time.Sleep(20 * time.Millisecond)
	h.Unsubscribe(sub)

	wg.Wait()
	if recovered != nil {
		t.Fatalf("Hub.Publish panicked when a subscriber disconnected mid-send: %v", recovered)
	}
	select {
	case <-returned:
	default:
		t.Fatal("Publish had not returned after the subscriber left")
	}
	// The departure is a closed `done`, not a closed event channel: the channel
	// a publisher sends on is never closed by anybody.
	select {
	case <-sub.Done():
	default:
		t.Fatal("Unsubscribe did not signal the subscriber's departure")
	}
	assert.False(t, sub.Dropped(), "an ordinary disconnection is not the slow-consumer drop")
}

// The same defect without a full channel: an ordinary connect/disconnect churn
// against a live publisher. Run with -race; the detector used to report the
// close/send pair on Subscriber.ch, and the process panicked when the send won.
// Nothing closes that channel any more, so both are gone.
func TestAuditAgnot_PublishRacesAnOrdinaryDisconnect(t *testing.T) {
	h := NewHub(64, nil)
	p := security.Principal{SubjectID: "u1", ActorType: security.ActorUser}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Hub.Publish panicked: %v", r)
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.Publish(Event{Type: TypeNotification, UserID: "u1"})
		}
	}()

	for i := 0; i < 2000; i++ {
		sub, _ := h.Subscribe(p, 0, 1)
		h.Unsubscribe(sub) // what Handler.ServeHTTP's defer does
	}
	close(stop)
	wg.Wait()
}

// Controls: what the hub must not leak. (Expected to pass.)
func TestAuditAgnot_TheHubDoesNotLeakAcrossUsers(t *testing.T) {
	h := NewHub(64, nil)
	alice := security.Principal{SubjectID: "alice", ActorType: security.ActorUser, AccountIDs: []string{"acct-a"}}
	bob := security.Principal{SubjectID: "bob", ActorType: security.ActorUser, AccountIDs: []string{"acct-b"}}
	operator := security.Principal{
		SubjectID: "op", ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleOperations}, AuthTime: time.Now(),
	}

	for _, tc := range []struct {
		name string
		e    Event
		who  security.Principal
		want bool
	}{
		{"alice's notification to alice", Event{Type: TypeNotification, UserID: "alice"}, alice, true},
		{"alice's notification to bob", Event{Type: TypeNotification, UserID: "alice"}, bob, false},
		{"alice's notification to an operator", Event{Type: TypeNotification, UserID: "alice"}, operator, false},
		{"alice's data.changed to bob", Event{Type: TypeDataChanged, UserID: "alice", AccountID: "acct-a"}, bob, false},
		{"alice's data.changed to an operator", Event{Type: TypeDataChanged, UserID: "alice"}, operator, false},
		{"a broadcast market tick to bob", Event{Type: TypeDataChanged, Broadcast: true}, bob, true},
		{"another account's event to bob", Event{Type: TypeOrderTransitioned, AccountID: "acct-a"}, bob, false},
	} {
		if got := tc.e.visibleTo(tc.who); got != tc.want {
			t.Errorf("%s: visibleTo = %v, want %v", tc.name, got, tc.want)
		}
	}

	// And the replay path filters too.
	subA, _ := h.Subscribe(alice, 0, 8)
	defer h.Unsubscribe(subA)
	h.Publish(Event{Type: TypeNotification, UserID: "bob"})
	e := h.Publish(Event{Type: TypeNotification, UserID: "alice"})
	subB, replay := h.Subscribe(bob, 1, 8)
	defer h.Unsubscribe(subB)
	for _, r := range replay {
		if r.Type == TypeNotification && r.UserID == "alice" {
			t.Fatalf("bob's replay carried alice's notification %d", e.ID)
		}
	}
}

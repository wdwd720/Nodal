package stream

import (
	"sync"
	"testing"
	"time"

	"github.com/nodal/controlplane/internal/security"
)

// F-agnot: Hub.Publish sends on a subscriber's channel OUTSIDE h.mu, while
// Hub.Unsubscribe closes that same channel UNDER h.mu. A disconnecting client
// (Handler.ServeHTTP's `defer h.hub.Unsubscribe(sub)`) therefore races a
// concurrent Publish, and a send on a closed channel panics.
//
// The window is made deterministic here by filling the subscriber's channel so
// Publish is parked in its 50ms select; in production the same collision
// happens whenever a publish lands while a browser is going away.
//
// The panic occurs in the PUBLISHING goroutine, which in cmd/api is the
// notification follower's ticker goroutine. An unrecovered panic there takes
// the whole API process down.
func TestAuditAgnot_PublishPanicsWhenASubscriberDisconnectsMidSend(t *testing.T) {
	h := NewHub(16, nil)
	p := security.Principal{SubjectID: "u1", ActorType: security.ActorUser, AccountIDs: []string{"a1"}}

	// bufferSize 1, so the second publish parks in the select.
	sub, _ := h.Subscribe(p, 0, 1)

	h.Publish(Event{Type: TypeNotification, UserID: "u1"}) // fills the channel

	var (
		wg        sync.WaitGroup
		recovered any
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { recovered = recover() }()
		// Parks in `select { case s.ch <- e: case <-time.After(50ms): }`.
		h.Publish(Event{Type: TypeNotification, UserID: "u1"})
	}()

	// Give the publisher time to park on the send, then do what an SSE handler
	// does when its client goes away.
	time.Sleep(20 * time.Millisecond)
	h.Unsubscribe(sub)

	wg.Wait()
	if recovered == nil {
		t.Fatalf("expected Publish to panic on a closed subscriber channel; it did not")
	}
	t.Logf("Hub.Publish panicked: %v", recovered)
}

// The same defect without a full channel: an ordinary connect/disconnect churn
// against a live publisher. Run with -race; the detector reports the
// close/send pair on Subscriber.ch, and the process panics when the send wins.
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

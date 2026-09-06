// Package fundingtest holds test doubles for internal/funding: an in-memory
// FundingProvider, a scripted ChainReceiptObserver and pass/fail gate and
// kill-switch checkers. It is never imported by production wiring.
package fundingtest

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/webhook"
)

// Provider is an in-memory FundingProvider. Sessions start INITIALIZED;
// tests move them with SetStatus and produce signed webhook bodies with
// Webhook. CreateErr, when set, is returned by CreateSession.
type Provider struct {
	ProviderName string
	Secret       string
	Clock        clock.Clock
	Tolerance    time.Duration
	CreateErr    error

	mu       sync.Mutex
	seq      int
	nonce    string
	sessions map[string]funding.Session
	created  []funding.CreateSessionRequest
}

// NewProvider returns a fake named "fake" signing webhooks with secret.
// Session ids carry a per-instance nonce so fixtures sharing a database
// never collide on UNIQUE(provider, provider_session_id).
func NewProvider(clk clock.Clock, secret string) *Provider {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return &Provider{ProviderName: "fake", Secret: secret, Clock: clk, Tolerance: 5 * time.Minute, nonce: hex.EncodeToString(b[:]), sessions: map[string]funding.Session{}}
}

// Name implements funding.FundingProvider.
func (p *Provider) Name() string { return p.ProviderName }

// Verification implements funding.FundingProvider.
func (p *Provider) Verification() provider.VerificationLabel { return provider.CodeComplete }

// Created returns every CreateSession request received.
func (p *Provider) Created() []funding.CreateSessionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]funding.CreateSessionRequest(nil), p.created...)
}

// CreateSession mints a session (idempotent on IdempotencyKey).
func (p *Provider) CreateSession(_ context.Context, req funding.CreateSessionRequest) (funding.Session, error) {
	if p.CreateErr != nil {
		return funding.Session{}, p.CreateErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, req)
	for _, s := range p.sessions {
		if s.Metadata["idempotency_key"] == req.IdempotencyKey {
			return s, nil
		}
	}
	p.seq++
	meta := map[string]string{"idempotency_key": req.IdempotencyKey}
	for k, v := range req.Metadata {
		meta[k] = v
	}
	s := funding.Session{
		ID: fmt.Sprintf("cos_fake_%s_%d", p.nonce, p.seq), Status: funding.ProviderStatusInitialized, RawStatus: "initialized",
		ClientSecret: "cos_fake_secret_" + strconv.Itoa(p.seq), RedirectURL: "https://fake.invalid/onramp/" + strconv.Itoa(p.seq),
		CreatedAt: p.Clock.Now().UTC(), DestinationNetwork: req.DestinationNetwork, DestinationCurrency: req.DestinationCurrency,
		SourceCurrency: req.SourceCurrency, SourceAmount: req.SourceAmount, WalletAddress: req.WalletAddress, Metadata: meta,
	}
	p.sessions[s.ID] = s
	return s, nil
}

// GetSession returns a session (NOT_FOUND when unknown).
func (p *Provider) GetSession(_ context.Context, sessionID string) (funding.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	if !ok {
		return funding.Session{}, errs.New(errs.CodeNotFound, "fake provider: unknown session").WithField("session_id", sessionID)
	}
	return s, nil
}

// SetStatus moves a session to a status; opts mutate the session first
// (amounts, wallet, transaction id).
func (p *Provider) SetStatus(sessionID string, status funding.ProviderStatus, raw string, opts ...func(*funding.Session)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[sessionID]
	for _, o := range opts {
		o(&s)
	}
	s.Status, s.RawStatus = status, raw
	p.sessions[sessionID] = s
}

// webhookBody is the fake's wire format.
type webhookBody struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Created int64            `json:"created"`
	Session *funding.Session `json:"session,omitempty"`
}

// Webhook renders a signed webhook delivery for a session: the raw body and
// the headers. eventType "session.updated" is modeled; anything else is a
// signed but unknown event.
func (p *Provider) Webhook(eventID, eventType, sessionID string) ([]byte, http.Header) {
	p.mu.Lock()
	s, ok := p.sessions[sessionID]
	p.mu.Unlock()
	body := webhookBody{ID: eventID, Type: eventType, Created: p.Clock.Now().Unix()}
	if ok {
		body.Session = &s
	}
	raw, _ := json.Marshal(body)
	return raw, p.Sign(raw, p.Clock.Now())
}

// Sign builds the signature headers for raw at signedAt.
func (p *Provider) Sign(raw []byte, signedAt time.Time) http.Header {
	ts := strconv.FormatInt(signedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(p.Secret))
	mac.Write([]byte(ts + "."))
	mac.Write(raw)
	h := http.Header{}
	h.Set("Fake-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
	return h
}

// ParseWebhook verifies Fake-Signature (t=<unix>,v1=<hex>) with the same
// scheme as Stripe's v1 and decodes the body.
func (p *Provider) ParseWebhook(_ context.Context, raw []byte, headers http.Header) (funding.WebhookEvent, error) {
	sig := headers.Get("Fake-Signature")
	if sig == "" {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrSignatureInvalid, errs.CodeWebhookSignatureInvalid, "missing signature")
	}
	var ts int64
	var v1 string
	for _, part := range strings.Split(sig, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			v1 = v
		}
	}
	mac := hmac.New(sha256.New, []byte(p.Secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(raw)
	want := hex.EncodeToString(mac.Sum(nil))
	got, err := hex.DecodeString(v1)
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) || want == "" {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrSignatureInvalid, errs.CodeWebhookSignatureInvalid, "signature mismatch")
	}
	signedAt := time.Unix(ts, 0).UTC()
	if delta := p.Clock.Now().Sub(signedAt); delta > p.Tolerance || delta < -p.Tolerance {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrTimestampOutOfTolerance, errs.CodeWebhookSignatureInvalid, "timestamp outside tolerance").
			WithField("reason", "timestamp_out_of_tolerance")
	}
	var body webhookBody
	if err := json.Unmarshal(raw, &body); err != nil || body.ID == "" {
		return funding.WebhookEvent{}, errs.Wrap(webhook.ErrMalformed, errs.CodeValidationFailed, "malformed body")
	}
	ev := funding.WebhookEvent{
		Identity: webhook.Identity{Provider: p.ProviderName, EventID: body.ID, EventType: body.Type, PublishedAt: time.Unix(body.Created, 0).UTC(), SignedAt: signedAt},
		Raw:      raw,
	}
	if body.Type == "session.updated" && body.Session != nil {
		ev.Session, ev.SessionKnown = *body.Session, true
	}
	return ev, nil
}

// Observer is a scripted ChainReceiptObserver.
type Observer struct {
	mu       sync.Mutex
	receipts map[string]funding.ChainReceipt // by address
	Err      error
	queries  []funding.ChainReceiptQuery
}

// NewObserver returns an empty observer.
func NewObserver() *Observer { return &Observer{receipts: map[string]funding.ChainReceipt{}} }

// Credit scripts a receipt for an address.
func (o *Observer) Credit(address string, r funding.ChainReceipt) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.receipts[address] = r
}

// Queries returns every query received.
func (o *Observer) Queries() []funding.ChainReceiptQuery {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]funding.ChainReceiptQuery(nil), o.queries...)
}

// ObserveCredit implements funding.ChainReceiptObserver.
func (o *Observer) ObserveCredit(_ context.Context, q funding.ChainReceiptQuery) (funding.ChainReceipt, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queries = append(o.queries, q)
	if o.Err != nil {
		return funding.ChainReceipt{}, false, o.Err
	}
	r, ok := o.receipts[q.Address]
	if !ok || r.ObservedAt.Before(q.Since) {
		return funding.ChainReceipt{}, false, nil
	}
	return r, true, nil
}

// Gates is a GateChecker whose verdict is fixed per capability. Missing
// capabilities are inactive (fail closed).
type Gates struct {
	mu     sync.Mutex
	Active map[gates.Capability]bool
	calls  []gates.Capability
}

// RequireActive implements funding.GateChecker.
func (g *Gates) RequireActive(_ context.Context, _ db.Querier, cap gates.Capability) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, cap)
	if g.Active[cap] {
		return nil
	}
	return errs.Newf(errs.CodeCapabilityNotApproved, "capability %s is not active", cap).WithField("capability", string(cap))
}

// Calls returns the capabilities consulted so far.
func (g *Gates) Calls() []gates.Capability {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gates.Capability(nil), g.calls...)
}

// KillSwitches is a KillSwitchChecker blocking when Blocked is set.
type KillSwitches struct {
	mu      sync.Mutex
	Blocked bool
	actions []killswitch.Action
}

// Check implements funding.KillSwitchChecker.
func (k *KillSwitches) Check(_ context.Context, _ db.Querier, a killswitch.Action) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.actions = append(k.actions, a)
	if err := a.Validate(); err != nil {
		return err
	}
	if k.Blocked && !a.Class.NeverBlocked() {
		return errs.New(errs.CodeKillSwitchActive, "kill switch active").WithField("switch", string(killswitch.FundingDisable))
	}
	return nil
}

// Actions returns every action checked so far.
func (k *KillSwitches) Actions() []killswitch.Action {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]killswitch.Action(nil), k.actions...)
}

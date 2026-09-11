# AUDIT FINDINGS

Every defect found while auditing the baseline and while building the migration, with what it was,
how it was found, and what was done. Ordered by when it was found.

Findings in **existing code** are marked `BASELINE`. Findings in code written during this migration
are marked `NEW` — they are recorded with the same weight, because a defect found in your own work an
hour after writing it is the same defect it would have been in six months.

Three findings are **OPEN**: F-31, a concurrency failure seen once and not reproduced; F-42, a
database control whose stated guarantee does not hold; and F-47, two statements about who may read
encrypted PII that contradict each other. All three are in the table with everything else rather
than in a footnote, because a register that only records what was fixed is a register that rewards
not looking.

**F-47 is also the one wrong call in this register.** It was raised as a privilege leak and a fix was
written and applied before the integration suite refused it. The entry keeps the whole sequence
rather than the conclusion, because "an auditor's framing was taken without checking whether the
repository stated a different contract" is the more useful thing to have written down.

The audit narrative is in `INDEPENDENT_AUDIT.md`; this is the register.

---

| # | Severity | Origin | Status | Summary |
|---|---|---|---|---|
| F-01 | P2 | BASELINE | fixed | `make integration` ran 1 package while claiming to run 42 |
| F-02 | P2 | BASELINE | fixed | The cross-tenant cursor security test could not reach its own assertion |
| F-03 | P1 | NEW | fixed | Value-domain isolation was direction-blind, defeating `PAYOUT_RESERVE` |
| F-04 | P1 | NEW | fixed | `credit.Issue` double-credited balances when used inside a larger posting |
| F-05 | P2 | BASELINE | fixed | The 00603 state-binding could not guard a table keyed by anything but `id` |
| F-06 | P2 | BASELINE | fixed | The 00603 state-binding could not express two transitions in one transaction |
| F-07 | P2 | NEW | fixed | Two SQL statements were not provably constant, breaking a security control |
| F-08 | P2 | NEW | fixed | A test created a Credit asset per call where the schema permits one |
| F-09 | P3 | NEW | fixed | `internal/valuation`'s fixture predated required value domains |
| F-10 | P2 | NEW | fixed | Payout shortfalls were explained from the wrong set and said nothing useful |
| F-11 | P2 | NEW | fixed | Two states in the payout machine were unreachable, one of them needed |
| F-12 | P2 | NEW | fixed | `RailHostedPartner` reported itself implemented with no adapter behind it |
| F-13 | P3 | NEW | fixed | The market fill test asserted absolute balances on a shared account |
| F-14 | P1 | NEW | fixed | The readiness report claimed a `-race` run that cannot happen on this host |
| F-15 | P1 | NEW | fixed | Internal commerce shipped with no capability gate of its own |
| F-16 | P2 | BASELINE | fixed | `MARKETPLACE` was low risk while gating the minting of withdrawable provenance |
| F-17 | P2 | NEW | fixed | A gate being off was reported as a policy refusal, sending operators to the wrong fix |
| F-18 | P2 | NEW | fixed | The adversarial-coverage summary contradicted the evidence tables in its own document |
| F-19 | P3 | NEW | fixed | A Route field called `MaxAgentAuthority` held a minimum, inviting a backwards gate |
| F-20 | P2 | BASELINE | fixed | Any failure to read `/v1/me` told the customer they were signed out, including a rate limit |
| F-21 | P2 | NEW | fixed | A chaos negative control could not fail, so the guard it defended was unproven |
| F-22 | P3 | NEW | fixed | A load script reported idempotency conflicts as price changes |
| F-23 | P2 | NEW | fixed | Reconciling a finished payout asked the provider nothing and reported agreement it never obtained |
| F-24 | P2 | NEW | fixed | The deployment's legal policy never reached the trade-intent compiler, and no test could tell |
| F-25 | P3 | BASELINE | fixed | The readiness report named a test that does not exist, and 609 such names were hand-checked once |
| F-26 | P1 | BASELINE | fixed | Two required inputs were never supplied, so the internal economy was unreachable in every deployment |
| F-27 | P1 | BASELINE | fixed | A capability check inside a financial transaction read through the pool, deadlocking it under concurrency |
| F-28 | P1 | BASELINE | fixed | No deployment could launch a native market: the act every document called separate was never built |
| F-29 | P2 | BASELINE | fixed | A user could reserve their Credits in a payout request and had no way to release them |
| F-30 | P2 | BASELINE | fixed | A property test could fail on a no-op mutation, and its shrinker would mis-explain any real failure |
| F-31 | P3 | NEW | fixed | Reproduced: a hundred buyers contend for one row and the 5s lock timeout refuses some of them, which is the system working |
| F-32 | P2 | BASELINE | fixed | The browser test for the internal-economy pages could not tell a working page from a broken one |
| F-33 | P2 | BASELINE | fixed | No browser test completed a transaction, so nothing proved a customer could finish anything |
| F-34 | P1 | BASELINE | fixed | The whole risk kernel was unreachable: no deployment wrote a policy and nothing evaluated one |
| F-35 | P2 | BASELINE | fixed | `make lint` had never passed either: 70 findings, 56 of them a linter arbitrating British English |
| F-36 | P1 | BASELINE | fixed | A read permission authorized writes: one ADMIN session could trade, buy and reserve payouts out of any customer's balance |
| F-37 | P1 | BASELINE | fixed | `POST /v1/native-markets/{id}/orders` had never worked in any deployment — no caller set `effective_at` |
| F-38 | P1 | BASELINE | fixed | A policy DENY carrying the gate's reason code became a PERMITTED route |
| F-39 | P2 | BASELINE | fixed | The no-float linter never looked at the internal economy |
| F-40 | P2 | NEW | fixed | This register claimed a database guarantee that the trigger it named does not make |
| F-41 | P2 | BASELINE | fixed | Three by-id reads answered more than they should: a membership oracle and two unpublished records |
| F-42 | P2 | BASELINE | fixed | The AU001 audit binding trusts a session variable any caller can set |
| F-43 | P1 | BASELINE | fixed | MARKETPLACE is high-risk in Go and was not in SQL, so the database evidence check never fired for it |
| F-44 | P1 | BASELINE | fixed | Every native-market price was displayed at the wrong scale, as a sixteen-digit number of Credits |
| F-45 | P2 | NEW | fixed | The web unit suite had been red on the purchase spec, which parsed money into doubles |
| F-46 | P2 | BASELINE | fixed | The nine-page browser check asserted absences before the page had loaded |
| F-47 | P2 | BASELINE | fixed | Two deliberate statements about who may read encrypted PII contradict each other |
| F-48 | P2 | BASELINE | fixed | Five SECURITY DEFINER functions did not pin `pg_temp` |
| F-49 | P2 | BASELINE | fixed | An account chose its own exemption from the negative-balance guard |
| F-50 | P2 | BASELINE | fixed | An asset could be stored with no value domain: a CHECK that evaluates to NULL accepts |
| F-51 | P2 | BASELINE | fixed | Three operations documents described controls and wiring that do not exist, in both directions |
| F-52 | P1 | BASELINE | fixed | A deployment could block an account's new risk and had no wired path to unblock it |
| F-53 | P2 | BASELINE | fixed | Tradability and the economics freeze keyed on different columns, so a market could trade while editable |
| F-54 | P3 | BASELINE | fixed | Every derivable count in the readiness documents had gone stale, including a restore drill reported at 111 migrations old |
| F-55 | P1 | BASELINE | fixed | Every runbook described a system that predates the build: 140 PENDING markers, and every package, binary and route they named exists |
| F-56 | P2 | BASELINE | fixed | Provider webhook evidence was mutable by the application role, including the payload hash and the signature flag |
| F-57 | P1 | BASELINE | fixed | Five subsystems' integration tests skipped silently in CI, and fifteen VERIFIED rows cited them as evidence |
| F-58 | P2 | BASELINE | fixed | Four documented SQLSTATEs were never raised and five raised families were never handled in Go |
| F-59 | P1 | BASELINE | fixed | The prediction resolver applied no maximum price age, so a dead feed scored every prediction FLAT |
| F-60 | P2 | BASELINE | fixed | Calibration dropped unresolvable predictions silently and reported a denominator that hid it |
| F-61 | P2 | BASELINE | fixed | `position_lots` had no database invariant tying status to quantity, and open quantity could be raised |
| F-62 | P3 | BASELINE | fixed | `normalized_events` replaced a column that varied between copies of the same event |
| F-63 | P2 | NEW | fixed | F-55's own correction pass replaced three true PENDING markers with false claims that things were wired |
| F-64 | P1 | NEW | fixed | An approval could be repointed at another target after both signatures, and the dual-control gate trusted a column instead of the code |
| F-65 | P2 | NEW | part | Two kill-switch kinds an operator would reach for in an incident reach nothing, and four documents said they work |
| F-66 | P1 | NEW | fixed | An authentication time in the future satisfied every step-up window for the life of the session |
| F-67 | P1 | NEW | fixed | The signing recovery path signed the bytes it was handed rather than the bytes that were approved |
| F-68 | P2 | NEW | fixed | The production withdrawal velocity policy bounds nothing, under a comment saying it bounds everything |
| F-69 | P2 | NEW | open | Inventory: what six independent audits found and what has not been fixed, with the reason for each |
| F-70 | P2 | NEW | fixed | A kill switch's first activation needed no transition row, because the binding fired only on UPDATE |
| F-71 | P2 | NEW | fixed | The console offered the grantee of an elevation a live Approve button, because the rule that refuses them was unreachable in every test |
| F-72 | P1 | NEW | fixed | An expired LIVE_AGENT_TRADING gate still read as live to the agent worker, because its reader decided on `state` alone |
| F-73 | P2 | NEW | fixed | Revoking an agent stopped new runs and let every run already open finish, including its intent |
| F-74 | P2 | NEW | fixed | Three tests named for a comparison against the database opened no database; 163 enum CHECKs had nothing checking them |
| F-75 | P3 | NEW | fixed | The intent emitter treated its envelope reader as optional, and a nil one dropped the instrument allow-list and the per-trade cap in silence |
| F-76 | P3 | NEW | fixed | Six exported route guards look like the API's authorization and the API mounts none of them |
| F-77 | P3 | NEW | fixed | The Redpanda suite left every topic it created behind, and eventually failed the next run with an error about the partition count |
| F-78 | P1 | NEW | fixed | The application role could promote an agent to LIVE with no approval and no evidence, by writing a transition row that denied the change it licensed |
| F-79 | P2 | NEW | fixed | login_attempts kept a plaintext OIDC nonce and PKCE verifier forever, under a migration saying the ops role purged them |
| F-80 | P2 | NEW | fixed | Two admin action kinds with live executors had never been run by anything, including the one that decides what happens to a payout |
| F-81 | P2 | NEW | fixed | An execution attempt could carry two signing decisions, and the reader silently preferred whichever was written last |
| F-82 | P2 | NEW | fixed | Nothing constructed a Redis client, so the API's rate limits were counted per replica: a configured 600/min admitted 1800 at three tasks and 7200 at twelve |
| F-83 | P3 | NEW | fixed | `make lint` was red at the provider workstream's HEAD, and two helpers that read configuration straight from the environment survived the fix that removed their callers |
| F-84 | P3 | NEW | part | Request validation precedes authentication, so two endpoints the checkpoint records as answering 401 answer 400 |
| F-85 | P2 | NEW | fixed | The request body is read into memory before the rate limiter runs, on a single 512 MB instance |
| F-86 | P2 | NEW | fixed | The payload-hash binding covered a PROCESSED message and not a FAILED one, so a retry could be processed from different bytes than the evidence records |
| F-87 | P1 | NEW | fixed | A planted OIDC callback signed the victim's browser in as the attacker, because `state` was a lookup key and never bound to a browser |
| F-88 | P1 | NEW | fixed | Every unauthenticated rate-limit bucket collapsed into one, because the limiter keyed on the load balancer's address |
| F-89 | P2 | NEW | fixed | `CP_AUTH_STEP_UP_MAX_AGE` was loaded, validated and read by nothing; the enforced window was three times the configured one |
| F-90 | P1 | NEW | fixed | Nothing settled on the deployed tier, so the money-at-risk ceiling was a lifetime cumulative cap that would have refused every Credit purchase forever |
| F-91 | P1 | NEW | fixed | The launch-cohort ceiling was configured, logged as in force, and enforced nowhere |
| F-92 | P2 | NEW | fixed | Two of the seven capacity tests the checkpoint cites as VERIFIED PASS pass only against a database that already contains rows |
| F-93 | P1 | NEW | open | Inventory: what the six provider audits found, verified against the source, and what has not been fixed — with the reason for each |
| F-94 | P2 | NEW | fixed | Nothing in the schema read a transition row's ORIGIN, so a row recording an origin the entity was never in licensed a change it did not describe |
| F-95 | P3 | NEW | part | Nine more enum CHECKs are compared against the Go list that declares them; 121 remain, and three were deliberately left unpaired |
| F-96 | P1 | NEW | fixed | The Stripe call ran inside the purchase transaction, so the funding row was not persisted before it and eight concurrent purchases could each pass one ceiling |
| F-97 | P2 | NEW | fixed | A money ceiling set to zero loaded, validated, and logged as in force with the cap silently off |
| F-98 | P2 | NEW | fixed | A key withdrawn from the JWKS kept verifying tokens for the life of the process, because only an unknown kid triggered a refresh |
| F-99 | P2 | NEW | fixed | Every step of the gate ceremony demanded a recent step-up except the first one |
| F-100 | P1 | NEW | fixed | A funding parked for a human is un-parked by the next provider event, and a refund followed by a late success mints Credits for money that was returned |
| F-101 | P1 | NEW | fixed | One transition row licenses a second, unrelated edge, because the edge encoding's delimiters are in band and a state name is unconstrained text |
| F-102 | P1 | NEW | fixed | Two write routes scoped through the read-grade helper, so one ADMIN session could cancel any customer's intent and move any seller's product; the guard that exists to prevent it matched one helper name of four |
| F-103 | P1 | NEW | fixed | Five configuration rules permitted what the deployment cannot survive: live provider credentials outside PROD, a trusted-proxy list that trusts everyone, a retention class with no floor, a legal policy the binary refuses to boot on, and a TLS flag nothing read |
| F-104 | P2 | NEW | fixed | Three controls reported something other than what they enforced: a secret redactor that never satisfied the interface it named, a step-up window three times the one applied, and a ceiling test that passed because a different guard fired |
| F-105 | P1 | NEW | fixed | An unauthenticated caller chose how many permanent, undeletable rows the service wrote, and the deployment's database ceiling halts every financial action when it is reached |
| F-106 | P1 | NEW | fixed | Three money tables handed one account's record to another on a reused idempotency key, and discarded the caller's own request; four sibling tables already compared the account |
| F-107 | P1 | NEW | fixed | The seller set the platform's own commission on their own sales, with a ceiling, no floor and a zero default; and a payout the provider may already have paid could be cancelled by its owner, releasing the reservation |
| F-108 | P1 | NEW | fixed | Two failed RPCs were read as proof a transaction never happened, closing the record as MATCHED and terminal while the user's tokens were spent |
| F-109 | P1 | NEW | fixed | The transition binding covers the state column and nothing else, so the application role could rewrite an amount, a destination or the definition of what counts as money, with no transition row |
| F-110 | P2 | NEW | fixed | An automatic reconciliation closed a record of any size with no amount test, under a doc comment naming two amount conditions that were not in the function |
| F-111 | P2 | NEW | fixed | The stopping criterion that certifies "documentation reflects reality" cited five invented numbers; the capability table listed ten of twenty; and the two documents a reviewer scores security posture from were checked by nothing |
| F-112 | P1 | NEW | fixed | A supported configuration removes the `__Host-` prefix from the session and login-state cookies, re-opening the planted-callback takeover F-87 closed; the terraform PROD example set it and the test suite's own valid-production fixture set it |
| F-113 | P1 | NEW | fixed | The amount a provider says it refunded was computed in two places and read in none, so a succeeded payment carrying a full refund minted Credits |
| F-114 | P1 | NEW | fixed | A revoked agent could return to live capital with no approval and no evidence, because it keeps its stage and both promotion CHECKs short-circuit when the stage does not move |
| F-115 | P1 | NEW | fixed | The branch whose comment says "do not resubmit" was the one that resubmitted, and the provider a payout goes to was a caller argument compared to nothing |
| F-116 | P2 | NEW | fixed | An open event stream outlived logout, operator revocation and the session's own absolute expiry, because revocation is per request and a stream is one request that never ends |
| F-117 | P2 | NEW | fixed | Two metric label sets took values the caller chose, so arming metrics on a 512 MB instance would have made observability the memory leak |
| F-118 | P2 | NEW | fixed | Every reconciliation alert was silent: an in-process counter no exporter read, an OTel instrument nobody constructed, and an observer callback with no caller |
| F-119 | P3 | NEW | fixed | An as-of price read was bounded by event time and not by knowledge time, so it could answer with something the platform had not yet received |
| F-120 | P3 | NEW | fixed | A calibration snapshot folded in every outcome resolved since, so the evidence a promotion decision reads changed when you looked at it again |
| F-121 | P1 | NEW | fixed | Every control on the dual-control table guarded UPDATE, so the application role could INSERT a row born APPROVED and forge two-person control outright |
| F-122 | P1 | NEW | fixed | An agent could be born LIVE, a funding born minted and a payout born settled, because every binding in the schema is about changes and a row inserted in a privileged state never changed |
| F-123 | P3 | NEW | fixed | Six rapid property-failure seeds were committed by accident, and rapid replays them on every run, pinning the property tier to cases that no longer fail |
| F-124 | P1 | NEW | fixed | The webhook path the published contract documents is not the one the service registers, so a delivery to it answers 404 and the provider eventually gives up |
| F-125 | P3 | NEW | fixed | The race detector cannot link on this host, so every race claim in this repository rests on CI |
| F-126 | P2 | NEW | fixed | Five spellings of one archive key parse to the same object, so a dedup check, a retention sweep and an audit reconstruction each miss what the other wrote |
| F-127 | P2 | NEW | fixed | The ADR deciding how an unprunable table must be pruned records that the table does not refuse DELETE; it does, and that was the fact the choice of remedy rested on |
| F-128 | P1 | NEW | fixed | The application role could mint a transition flag by attaching the real setter to a temp table of its own, which F-42 does not record and which defeats any fix that only hardens the flag's value |
| F-129 | P2 | NEW | fixed | A restore that lost one table would pass every comparison the restore drill makes and then refuse every state change in the system, with an error blaming the caller |
| F-130 | P3 | NEW | fixed | This register states every finding's status twice and nothing checked the two agreed; four disagreed, and five findings have no evidence section at all |
| F-131 | P3 | NEW | fixed | A fixture wrote agent rows with v4 UUIDs into a column the application reads as a v7 typed id, so it created rows this system can write and cannot read |
| F-132 | P2 | NEW | fixed | The webhook route's "declared public" assertion has never been measured: the probe was answered 404 by a provider lookup, which satisfies "not 401" while proving nothing |
| F-133 | P2 | BASELINE | fixed | Expired sessions were never purged: the job existed, was documented as the ops role's, and nothing on any tier ran it |
| F-134 | P2 | PRODUCTIZATION | fixed | The Go e2e suite could not sign in since F-87; three stale expectations behind it |
| F-135 | P3 | PRODUCTIZATION | fixed | The chaos purchase world set a platform fee the service overwrites |
| F-201 | P1 | PRODUCTIZATION | fixed | The withdrawal bar drew the frozen bucket beside the ineligible one it is already inside, so the picture claimed more than the total |
| F-202 | P1 | PRODUCTIZATION | fixed | A fill left the portfolio, the holdings and the activity feed stale: the stream's scope map named five dead prefixes and neither key a fill changes |
| F-203 | P1 | PRODUCTIZATION | fixed | After any refused order the ticket reused its idempotency key with a new quote's body, so the customer was told about the key instead of about the trade |
| F-204 | P2 | PRODUCTIZATION | fixed | Signing out left the previous customer's typed form state, including a payout destination id, in the tab for the next person |
| F-205 | P2 | PRODUCTIZATION | fixed | The Buy Credits idempotency key was kept across a change of amount, so a changed amount answered 409 with no recovery |
| F-206 | P2 | PRODUCTIZATION | fixed | The public site never said the deployment is a sandbox tier, though it read the response that says so for its build tag |
| F-207 | P2 | PRODUCTIZATION | fixed | The same market figures were `economy` on Home and `simulated` on `/markets`, so the temperature described the page rather than the tier |
| F-208 | P2 | PRODUCTIZATION | fixed | Every press of a command button minted a new idempotency key, so a lost reply and a second press were a second purchase, decision, agent or asset |
| F-209 | P2 | PRODUCTIZATION | fixed | The application sweeps asserted nothing that identifies the page, so `/this-route-does-not-exist` satisfied all of them |
| F-210 | P2 | PRODUCTIZATION | fixed | `g-refund.spec.ts`'s reversal test asserted an unconditional label, so it passed on exactly the account that disproves its name |
| F-211 | P3 | PRODUCTIZATION | fixed | No focus management on a route change: a keyboard or screen-reader user was left in the rail beside a page nobody announced |
| F-212 | P3 | PRODUCTIZATION | fixed | Every unmapped failure rendered the one generic apology `UI_UX_SYSTEM.md` §4 forbids by name, while discarding the title the backend had sent |
| F-213 | P3 | PRODUCTIZATION | fixed | `/agents/:agentId` was in no route list, so its only coverage was the not-found state — the one shape of it that renders no figure |
| F-214 | P3 | PRODUCTIZATION | fixed | Three components nothing imports, one of them a second money formatter |
| F-215 | P3 | PRODUCTIZATION | fixed | `Figure` silently dropped `symbol` on counts and `stablecoin` on base-unit values, and read a percent's base units as a decimal |
| F-216 | P3 | PRODUCTIZATION | fixed | The paused-market refusal test skipped on every run there has ever been, so its assertions had executed zero times |
| F-217 | P3 | PRODUCTIZATION | fixed | `AgentNew.tsx` and `AgentDetail.tsx` minted a key per press under comments promising the opposite |
| F-218 | P3 | PRODUCTIZATION | fixed | No error boundary anywhere: a malformed stash reaching the BigInt constructor blanked the whole application on every load until the tab was closed |
| F-219 | P3 | PRODUCTIZATION | fixed | `openapi.yaml` said quote fees are Credits at price_scale; they are at the Credit asset's scale, twelve orders of magnitude apart |
| F-220 | P3 | PRODUCTIZATION | fixed | Two honesty checks settled the network after reading the page, so they read it mid-fetch |
| F-221 | P3 | PRODUCTIZATION | fixed | Adding a payout destination minted an idempotency key per press, so a retry registered it twice |
| F-222 | P2 | PRODUCTIZATION | fixed | Finishing onboarding raced the profile refetch, so a slow connection sent a new customer back to a terms page with nothing left to accept |
| F-223 | P3 | PRODUCTIZATION | fixed | The demodata command gave its ledger no capability resolver, so it refused every demo trade on a database whose gates were sandbox-active |
| F-224 | P1 | PRODUCTIZATION | fixed | A payout below the provider's published minimum was created, reserved, submitted and settled because quote_id was optional, and the fee was never taken |
| F-225 | P1 | PRODUCTIZATION | fixed | The provider was instructed to pay no amount, to nobody, in no currency, and the request still reached SETTLED |
| F-226 | P1 | PRODUCTIZATION | fixed | An open sanctions review, an account restriction and an unsupported jurisdiction stopped the eligibility page and stopped nothing on the conversion path |
| F-227 | P1 | PRODUCTIZATION | fixed | The compliance and verification transition tables recorded edges and constrained none, so one INSERT reached VERIFIED and four more reached PAYOUT_KYC |
| F-228 | P2 | PRODUCTIZATION | fixed | A payout destination the provider had said it cannot pay was accepted and marked VERIFIED whenever the client omitted country |
| F-229 | P2 | PRODUCTIZATION | fixed | payout_requests.state and its money columns were freely writable by cp_app, so a REJECTED request was moved to SETTLED with a forged provider reference |
| F-230 | P2 | PRODUCTIZATION | fixed | No earned Credit could ever reach a payout-eligible funding finality, and the explanation said waiting would fix it |
| F-231 | P2 | PRODUCTIZATION | fixed | The single-use hosted verification link was written to idempotency_keys.response_body, which two read-only roles may SELECT |
| F-232 | P3 | PRODUCTIZATION | fixed | The conversion request recorded no sandbox fact, so one payout was a rehearsal on one screen and unlabelled on two others |
| F-233 | P3 | PRODUCTIZATION | fixed | POST /v1/payouts answered 403 for another account's destination where every sibling answers 404 |
| F-234 | P3 | PRODUCTIZATION | fixed | PROVIDER_BOUNDARY §3 described nine methods and an error taxonomy the shipped interfaces do not have, and §2 named a route that does not exist |
| F-249 | P2 | PRODUCTIZATION | fixed | The closure decision read its financial blockers as an aggregate, so a conversion request committing beside it left a reservation on a CLOSED account |
| F-250 | P3 | PRODUCTIZATION | fixed | A browser scenario asserted that a reserved payout stops at VERIFIED, which the sweep had made false; it passed by reading the state before the first tick |
| F-235 | P2 | PRODUCTIZATION | fixed | POLICY_AUTHORITY scoped the four evidence references to LIVE_* and WITHDRAWALS; the predicate the gate code consults is true for eighteen of twenty, and §1's state machine had no SANDBOX |
| F-236 | P2 | PRODUCTIZATION | fixed | VERIFICATION_AND_WITHDRAWAL sent a caller to POST /v1/me/terms/accept; no route matches that path |
| F-237 | P2 | PRODUCTIZATION | fixed | USER_JOURNEY's onboarding row said PUT /v1/me/profile; the path is served, but only with POST |
| F-238 | P2 | PRODUCTIZATION | fixed | SECURITY.md reported nine present controls as missing and called eight written, passing adversarial suites "Planned", behind a path check that exempted the whole table |
| F-239 | P2 | PRODUCTIZATION | fixed | Two of THREAT_MODEL's top-ten residual risks were closed by the tree, and the §2 boundary every STRIDE row is scored against listed twelve live packages as absent |
| F-240 | P2 | PRODUCTIZATION | fixed | .env.example set the settlement pair to mainnet USDC while the seed registers devnet, so a developer following the README got a service that refuses to start |
| F-241 | P3 | PRODUCTIZATION | fixed | PROVIDER_BOUNDARY called PAYOUT_KYC a capability; it is a verification level, and no such gate exists for an operator to find |
| F-242 | P3 | PRODUCTIZATION | fixed | Three documents described an activity feed of seven kinds that has carried eighteen since D-081, one of them holding a traceability row open on it |
| F-243 | P3 | PRODUCTIZATION | fixed | UI_UX_SYSTEM described a shell the app does not ship and told a copywriter to use "Add funds", the one phrase USER_JOURNEY forbids by name |
| F-244 | P3 | PRODUCTIZATION | fixed | PRODUCT_ARCHITECTURE's domain table and ADR-0027 sent a reader to internal/credits and internal/payments; neither package exists |
| F-245 | P3 | PRODUCTIZATION | fixed | The README called apps/web a Next.js app, reported three running test tiers as pending, and described a V1 that predates the product the tree builds |
| F-246 | P3 | PRODUCTIZATION | fixed | BACKUP_RESTORE marked DISASTER_RECOVERY.md and the database-corruption runbook "(pending)"; both exist, and the check read only the uppercase marker |
| F-247 | P3 | PRODUCTIZATION | fixed | CURRENT_SYSTEM_INVENTORY said the agent wave added nine routes over a table of ten rows |
| F-248 | P3 | PRODUCTIZATION | fixed | The decision register cited a test that has never existed under that name, one document outside the citation check's scope |
| F-253 | P3 | PRODUCTIZATION | fixed | USER_JOURNEY told a caller to read onboarding.state on GET /v1/me; the contract declares timestamps and no such property |
| F-251 | P3 | PRODUCTIZATION | fixed | The agent-detail sweep skipped with a reason that was not the reason, on the route its own finding was about |
| F-252 | P3 | PRODUCTIZATION | fixed | Two webhook-driven scenarios counted as passes while proving only that an unsigned delivery is refused, and four purchase tests were never registered |
| F-185 | P1 | PRODUCTIZATION | fixed | Any authenticated person could end the API process by closing a stream while an event was published |
| F-186 | P2 | PRODUCTIZATION | fixed | Three clocks for one notification, and Last-Event-ID compared two of them, so a resume skipped what the lap wrote |
| F-187 | P2 | PRODUCTIZATION | fixed | An agent was granted authority over a strategy version its owner never owned, never named and never accepted |
| F-188 | P2 | PRODUCTIZATION | fixed | A sign-in's IP address and User-Agent were copied into the one table nothing can purge, readable by both analytics roles |
| F-189 | P3 | PRODUCTIZATION | fixed | A compiler backend's answer was written verbatim, including whose strategy it was under and whether anybody had accepted it |
| F-190 | P3 | PRODUCTIZATION | fixed | Every data.changed signal in the lap was republished on every pass, a broadcast one to every connected client |
| F-191 | P3 | PRODUCTIZATION | fixed | Two documented mechanisms the code does not implement: an in-flight marker nothing can observe, and a comment outlived by its subject |
| F-192 | P3 | PRODUCTIZATION | fixed | The market-pause fan-out was bounded only by a capacity variable that may be zero |
| F-193 | P1 | PRODUCTIZATION | fixed | A market whose marginal price truncates to zero can be opened, and the impact, slippage and breaker controls then fail open |
| F-194 | P2 | PRODUCTIZATION | fixed | Four SECURITY DEFINER functions written after F-48 repeat F-48: an unpinned search_path lets the application forge a position through pg_temp |
| F-195 | P2 | PRODUCTIZATION | fixed | Every order response reports state_version_after 0, and the trade ticket prints it |
| F-196 | P2 | PRODUCTIZATION | fixed | The public market list carries creator_account_id and accepts it as a filter, contradicting the premise D-080 made it public on |
| F-197 | P2 | PRODUCTIZATION | fixed | The market summary hands every top holder's account id and exact holding to any signed-in customer |
| F-198 | P2 | PRODUCTIZATION | fixed | Content a moderation verdict REJECTED stays on the public list, beside the verdict that rejected it |
| F-199 | P3 | PRODUCTIZATION | fixed | A crafted discovery cursor is a 500 from a public, unauthenticated route |
| F-200 | P3 | PRODUCTIZATION | fixed | Four claims in the markets area the code does not support |
| F-166 | P1 | PRODUCTIZATION | fixed | Any caller behind the platform router chose its own rate-limit key and its own audit address by writing one header |
| F-167 | P2 | PRODUCTIZATION | fixed | The notification follower's cursor could move backwards, so one ordinary burst stalled a source for the life of the deployment, silently |
| F-168 | P2 | PRODUCTIZATION | fixed | The sanctions screen that gates a payout was writable by one UPDATE with no transition row, one statement after the migration that protected the state beside it |
| F-169 | P2 | PRODUCTIZATION | fixed | The in-process rate-limit store documented a bound nothing enforced: no caller of Sweep, and a key seen once retained for the life of the process |
| F-170 | P2 | PRODUCTIZATION | fixed | Four expiry passes existed and nothing called them; the one with a person waiting left an expired verification link holding its owner's only session shut |
| F-171 | P3 | PRODUCTIZATION | fixed | BODY_TOO_LARGE is emitted on every route and was absent from the registered code set, so nothing that enumerates the codes covered it |
| F-172 | P3 | PRODUCTIZATION | fixed | /v1/strategies published timestamps in the server's local offset, on a contract that says RFC 3339 UTC |
| F-173 | P3 | PRODUCTIZATION | fixed | The body limit was justified by the size of the largest schema field, and that number had been wrong since the strategy schemas landed |
| F-151 | P0 | PRODUCTIZATION | fixed | The pricing policy minted a count of Credits into a field that means base units, so $10 bought 0.001 Credits |
| F-152 | P1 | PRODUCTIZATION | fixed | A chargeback destroyed whichever lots sorted first and stranded the units it reversed |
| F-153 | P1 | PRODUCTIZATION | fixed | A failed provider call or an abandoned checkout held the money-at-risk ceiling forever |
| F-154 | P1 | PRODUCTIZATION | fixed | The sweep that recovers a swallowed provider event had no caller in the deployed topology |
| F-155 | P2 | PRODUCTIZATION | fixed | A card-network inquiry closing settled a funding before its reversibility window |
| F-156 | P3 | PRODUCTIZATION | fixed | The reversed balance bucket was returned by the API and rendered nowhere |
| F-157 | P3 | PRODUCTIZATION | fixed | SandboxPolicy permitted an origin the document it implements says it refuses |
| F-158 | P3 | PRODUCTIZATION | fixed | The sandbox flag on a purchase was the deployment's mode today, not a recorded fact |
| F-159 | P3 | PRODUCTIZATION | fixed | The money-at-risk ceiling added in unchecked int64 |
| F-174 | P2 | PRODUCTIZATION | fixed | The closure cooling-off period was measured against a timestamp the INSERTing role chose, so a row stamped in the future effected a closure inside the wait |
| F-175 | P2 | PRODUCTIZATION | fixed | The operator directory -- the only source of operator authority -- was freely rewritable by the application role, so a revocation did not stay revoked and a role could be rewritten in place |
| F-176 | P2 | PRODUCTIZATION | fixed | Both account-lifecycle edge sets existed only in Go: a terminal closure request could be walked back and a CLOSED user reopened |
| F-177 | P2 | PRODUCTIZATION | fixed | A step-up login minted a second session and left the first live, so the credential the step-up defends against still worked; auth.Manager.Rotate had no caller |
| F-178 | P2 | PRODUCTIZATION | fixed | The /me self-scoping proof walked path patterns only, and a state-changing GET that named its account in the query string was scoped with the read-grade helper |
| F-179 | P2 | PRODUCTIZATION | fixed | Effecting a closure consulted no balance, no open payout and no open position, and the operator's decision surface carried none of the three |
| F-180 | P3 | PRODUCTIZATION | fixed | STAGING accepted an unbounded standing operator directory declared in an environment variable; only PROD was narrowed |
| F-181 | P3 | PRODUCTIZATION | fixed | CUSTOMER was a nameable operator-directory role, and declaring it recorded a person's own consent as given on their behalf by an operator |
| F-182 | P3 | PRODUCTIZATION | fixed | The login-state refusal described an attack when the likely cause is a second sign-in tab |
| F-183 | P3 | PRODUCTIZATION | fixed | The privacy notice said security events are removed on a schedule; the retention variable is 0 by default and nothing purges them |
| F-184 | P3 | PRODUCTIZATION | fixed | A test asset symbol drew four hex characters from a UUID tail, which collides on SYMBOL_TAKEN about one run in fifty |
| F-136 | P1 | PRODUCTIZATION | fixed | The blueprint handed the internet-facing API the schema-owner DSN, under a name the test that forbids it did not know |
| F-137 | P1 | PRODUCTIZATION | fixed | STAGING booted with no alert destination and no PII keyring: both rules were satisfied by the reference, and three documents said it could not |
| F-138 | P2 | PRODUCTIZATION | fixed | The blueprint's secret scan read one service of two, and nothing pinned the static site's CSP, HSTS, SPA rewrite or domain |
| F-139 | P2 | PRODUCTIZATION | fixed | `style-src 'self'` blocked the inline style attributes the app ships, so the balance bar rendered empty while its label stated the shares |
| F-140 | P2 | PRODUCTIZATION | fixed | `connect-src`, `script-src` and the Stripe publishable key were unasserted, and the key's account was never held against the API's |
| F-141 | P2 | PRODUCTIZATION | fixed | `nodal-api` declared no custom domain while every other value in the file assumed one |
| F-142 | P2 | PRODUCTIZATION | fixed | `/v1/version` reported `build_version: dev` for every image ever built from the blueprint, and the config hash carried the same constant |
| F-143 | P2 | PRODUCTIZATION | fixed | `EventSource` was constructed without `withCredentials`, so the live stream carried no session on the deployed cross-origin topology |
| F-144 | P2 | PRODUCTIZATION | fixed | `CP_SEED_ENABLED` was read by nothing and the sandbox tier seeded demo data regardless, so the deployment declared that it does not seed and seeded |
| F-145 | P3 | PRODUCTIZATION | fixed | A misspelled capability loaded clean and was dropped in silence, switching off condition 1 of the policy authority |
| F-146 | P2 | PRODUCTIZATION | fixed | `return_to` of the form `/\host` was an open redirect off the callback that sets the session cookie |
| F-147 | P3 | PRODUCTIZATION | fixed | `render.yaml` pointed operators at `docs/operations/RENDER_DEPLOY.md`, which does not exist |
| F-148 | P3 | PRODUCTIZATION | fixed | The static site published its source maps, serving the readable frontend to every visitor |
| F-149 | P3 | PRODUCTIZATION | fixed | `CP_AUTH_POST_LOGIN_URL` defaulted to one deployment's hostname and no rule required it where it matters |
| F-150 | P2 | PRODUCTIZATION | fixed | `Permissions-Policy: payment=()` denied the Payment Request API to Stripe's Payment Element, removing the wallets |
| F-160 | P2 | PRODUCTIZATION | fixed | A gate sandbox-activated out of REVOKED reported success, logged it, and could never be active |
| F-161 | P2 | PRODUCTIZATION | fixed | A SANDBOX gate reached from EXPIRED or REVOKED carried the whole approval version that reached ACTIVE, under a banner saying it carries none |
| F-162 | P2 | PRODUCTIZATION | fixed | Rehearsing the real ceremony on a sandbox-listed capability stopped the deployment booting |
| F-163 | P1 | PRODUCTIZATION | fixed | No kill switch reached the conversion-request path, so WITHDRAWALS_DISABLE, GLOBAL_NEW_RISK_KILL and ACCOUNT_FREEZE stopped none of it |
| F-164 | P3 | PRODUCTIZATION | fixed | ADR-0023 §3 said every sandbox permission reads NOT-AN-APPROVAL; one carries a standing product decision, correctly |
| F-165 | P3 | PRODUCTIZATION | fixed | ADR-0023 cited `test/integration/gates` as its proof, which has never existed |

---

## F-01 · `make integration` ran one package · BASELINE · P2 · FIXED

**Found by** running `make integration` and enumerating what it executed.

`./test/integration/...` contains exactly one package. The database-backed proof of the financial
core lives behind `//go:build integration` inside `internal/` and `cmd/` — 40 packages that the
target never ran. `make test-all` reported success having executed none of them.

CI had already grown an inline shell loop that enumerated the tag correctly, with a comment saying
the workaround would live there "until that target takes a package list". So CI was sound and the
local command was not, and two definitions of "the integration tests" were free to drift.

**Fix.** `scripts/inttest`: one program both call. It enumerates the build tag, gives each package
its own database (they cannot share one — `internal/event` truncates tables others count rows in),
and refuses to run at all if it finds fewer than two packages, so a broken enumeration fails loudly
instead of passing in a tenth of a second.

## F-02 · The cross-tenant cursor test could not reach its assertion · BASELINE · P2 · FIXED

**Found by** running `test/security` on a database other suites had already used.

`TestLedger_AForgedCursorCannotCrossTenants` selected the *first* customer-owned journal row and then
asserted it belonged to customer-a. Any other suite posting a row first broke the precondition — and
it broke **before** the cross-tenant assertion, so SEC-003 was unproven.

The API is not defective: it takes the owner as a bound parameter and the cursor only as a keyset
position. The defect is that the test could not demonstrate it. Note the asymmetry that made this
worth fixing rather than shrugging at: a security test that stops before its assertion *fails*, which
is survivable; the same test passing while proving nothing would not be.

**Fix.** Select the row by owner instead of asserting about whichever row sorted first. Verified on
three consecutive runs against the same database.

## F-03 · Isolation was direction-blind · NEW · P1 · FIXED

**Found by** `TestIsolation_PayoutPathIsGatedAtBothSteps`, written before the design was believed.

The first draft of `CheckDomains` took only the *set* of domains a transaction touched and permitted
the pair if a conversion was declared in **either** direction. `PAYOUT_PENDING → INTERNAL_CREDIT` is
deliberately ungated so an in-flight payout can always be unwound, so a direction-blind check let
that ungated return path authorise the gated `INTERNAL_CREDIT → PAYOUT_PENDING` reserve step —
defeating `PAYOUT_RESERVE` for anyone able to post a two-domain transaction.

**Fix.** `CheckConversion(from, to, caps)` is directional and is the authority. `CheckPosting`
additionally requires a cross-domain transaction to *declare* which conversion it is, and the
declaration to match the domains present. Regression: `TestIsolation_DirectionIsLoadBearing`.

## F-04 · `credit.Issue` double-credited inside a larger posting · NEW · P1 · FIXED

**Found by** `TestIntegration_BuyMovesValueExactlyWhereItShould`, which asserted the creator's
balance and got twice the fee.

`Issue` did two things: post the movement and record the provenance. A native-market trade already
credits the seller and the creator as legs of a six-entry, two-asset posting, so calling `Issue`
there posted a *second* transaction and credited them again. Provenance and balance then disagreed,
which `VerifyProvenance` caught immediately afterwards.

**Fix.** Split into `Issue` (post + record) and `RecordLot` (record against a posting the caller
already made). The database already refused provenance for units no journal transaction moved, so the
second half of the bug was contained; the first half was real.

## F-05, F-06 · The state-change binding was narrower than its purpose · BASELINE · P2 · FIXED

**Found by** applying migration 00603's binding to `native_assets`.

`cp_require_transition` read `NEW.id`, because every table it guarded had an `id` primary key. A
native asset IS its registry entry and is keyed by `asset_id`; giving it a surrogate key so one
trigger could find it would be the tail wagging the dog.

Separately, the flag was a single value, so an entity could change state only **once** per
transaction: a second flag overwrote the first, and at commit the earlier update found the later
state and was refused. Creating and launching an asset does exactly that.

**Fix.** The function takes an optional id-column argument, and the flag accumulates with membership
checking. Both are widenings — every existing trigger passes one argument and is unaffected — and
membership is exactly as strong as equality was, because writing an immutable transition row is still
the only way to set the flag.

## F-07 · Two SQL statements were not provably constant · NEW · P2 · FIXED

**Found by** `test/security`'s SQL-source analysis in the full sweep. It proves every statement in the
repository is built from constants, so no request-derived string can be concatenated into one.

`internal/credit` assembled a `WHERE` clause from literals and used an `ORDER BY` computed from a Go
map at init. Both were safe and neither was *provably* safe.

**Fix.** Not weakening the check — weakening it for one package weakens it for every package. The
`ORDER BY` is a constant, both lot filters are parameters of one constant statement, and
`TestConsumptionOrderSQL_MatchesTheMap` asserts the constant still implements the Go rank map exactly,
so the map remains the authority and the constant cannot drift.

## F-08, F-09 · Two fixtures broke under the second run · NEW · P2/P3 · FIXED

`internal/ledger`'s value-domain tests created a Credit asset per call, and migration 00711 permits
exactly one — green on a fresh database, red on the second run of the same one. `internal/valuation`'s
asset fixture predated required value domains and had not been caught by the mechanical sweep because
it constructs its asset differently from the others.

Neither was reachable from any test run in isolation. They are the argument for `scripts/inttest`
existing, and for the standing rule that every suite is run twice.

## F-10 · Payout shortfalls were explained from the wrong set · NEW · P2 · FIXED

**Found by** `TestIntegration_ReversibleFundingIsNeverPayable`, which expected `FUNDING_NOT_FINAL`
among the reasons and got only `INSUFFICIENT_ELIGIBLE_VALUE`.

The shortfall explanation was computed from `EligibleLots`, which filters **out** exactly the lots
whose refusal is the answer. A user whose entire balance was ineligible was told "insufficient" and
never why — which is the least useful possible answer to "why can I not withdraw".

**Fix.** Explaining a refusal starts from the refused set. The engine now enumerates every lot the
account holds and reports the distinct reasons.

## F-11 · Two payout states were unreachable · NEW · P2 · FIXED

`VERIFICATION_REQUIRED` could not reach `VERIFIED` (a user may complete identity verification out of
band, with no pending provider flow to sit in), and `SUBMITTED` could not reach `MANUAL_REVIEW`,
which is where a settlement that cannot be recorded has to go.

Both were gaps in the transition table rather than deliberate restrictions.
`TestState_EveryTransitionTargetIsDeclared` now asserts every declared state is reachable, so a
decorative state cannot be added again without noticing.

## F-12 · A rail reported itself implemented with nothing behind it · NEW · P2 · FIXED

**Found by** writing the BLOCKERS entry for the hosted-partner rail and noticing that the sentence
"no adapter exists" contradicted `Implemented() == true`.

The Settlement Compiler refuses to route to an unimplemented rail; reporting `true` would have let it
route to nothing. PART XXII names this failure mode exactly: "do not implement live unsupported
products merely because an interface exists".

**Fix.** `RailHostedPartner.Implemented()` returns `false` until an adapter exists, with the test
asserting it.

## F-13 · A market test asserted absolute balances on a shared account · NEW · P3 · FIXED

Every market quotes against the same Credit asset, so the platform's `MARKET_RESERVE` and fee
accounts are shared by every market — and, in a test, by every test that ran before. Asserting an
absolute balance passed only on a virgin database. The test now asserts deltas, and the shared-account
property became the `VerifyReserves` control-account invariant rather than being worked around.

## F-14 · The readiness report claimed a race-detector run that cannot happen on this host · NEW · P1 · FIXED

**Found by** trying to run `go test -race` over `internal/commerce` and getting
`cgo: C compiler "gcc" not found`. There is no C compiler on this machine — `gcc`, `clang` and `cc`
are all absent — so `go test -race` cannot have succeeded here at any point.

`docs/release/PRODUCTION_READINESS_REPORT.md` nonetheless carried a row reading

> | Race | `-race` over credit, nativemarket, payout | ok |

and a claim that the hundred-concurrent-spends test had run "also under `-race`". Neither was
executed. This is worse than an untested code path: the whole purpose of that document is that every
row has an executable command behind it, and a row that does not is a licence for the reader to stop
checking the others.

**Why it happened.** `-race` is in `make race` and in CI, both of which run on Linux where cgo works.
I recorded the *intent* of that pipeline as a *result* of this session.

**Fix, in four parts.**

1. The report was corrected to state that the race detector had not run here, name the reason, and
   mark the row as delegated to CI rather than as passed.
2. `internal/{credit,nativemarket,payout,commerce}` were added to the CI integration-race loop and to
   `make integration-race`. They were absent, so even in CI those four packages — the ones with the
   database-backed concurrency properties of the internal economy — had never been raced. Fixing the
   false claim without fixing that would have been cosmetic.
3. The concurrency properties that motivated the claim are also asserted without the race detector,
   by outcome rather than by instrumentation: 100 concurrent spends against a balance that funds 10
   yield exactly 10 successes, and 10 concurrent purchases against a balance that funds 2 yield
   exactly 2.
4. **A C compiler was then installed on the host and the run was actually executed.** It did not
   work immediately: the toolchain had been installed under a path containing a space
   (`C:\Users\Mihir Modi\...`), and the MinGW driver embeds its own unquoted path in a linker-script
   argument, so `ld` received `C:/Users/Mihir` and refused with "linker script file ... appears
   multiple times". A directory junction does not fix it — gcc canonicalises through it — so the
   toolchain was copied to a space-free path. `internal/{credit,nativemarket,payout,commerce}` and
   the financial core then passed under `-race` on this host, and the report's row says so with the
   command that produced it.

**Two lessons, not one.** The first is the obvious one: never record an intended command as an
executed one. The second is the reason the first is easy to get wrong — the fix looked impossible
("no compiler on this host"), and an obstacle that looks environmental is exactly the kind a report
quietly routes around instead of removing.

**The general lesson, recorded because it will recur.** A readiness document must distinguish "this
command was run and passed here" from "this command runs in CI" from "this command is intended".
Collapsing the three is how a launch package becomes decorative. Every row in
`PRODUCTION_READINESS_REPORT.md` §2 now carries which of the three it is.

## F-15 · Internal commerce shipped with no capability gate of its own · NEW · P1 · FIXED

**Found by** writing the Settlement Compiler's routing table and having to answer "which capability
gates PURCHASE_INTERNAL_SERVICE?" -- and finding that the honest answer was none.

Stage 8 reasoned that a purchase is a single-domain movement inside `INTERNAL_CREDIT`, so the
ledger's isolation check requires no capability for it. That reasoning is correct and it answered the
wrong question. The isolation check asks "may value cross this boundary"; nobody had asked "may this
deployment run a user-to-user marketplace at all". The result was that internal commerce -- the only
legitimate way `CREATOR_EARNING` / `DATA_SALE_EARNING` / `AGENT_SERVICE_EARNING` provenance comes
into existence -- would have been live on a fresh deployment with no gate, no policy and no decision.

Every other Domain A action had a gate. Commerce did not, precisely because its *isolation* story was
the simplest.

**Fix.** `internal/commerce` now takes a `CapabilityResolver` and refuses a purchase unless
`MARKETPLACE` is ACTIVE. It is checked in the DOMAIN SERVICE, not only in the compiler at the edge: a
gate that lives at the HTTP boundary is one a worker or a script walks around.
`TestIntegration_WithoutTheMarketplaceCapabilityNothingSells` proves the refusal for an unconfigured
resolver and for an explicitly inactive gate, and proves the same purchase commits once it is on --
so the refusal is about the gate and nothing else.
`TestIntegration_ARepeatedPurchaseStopsWorkingWhenTheGateIsPulled` proves the gate is checked before
the idempotency lookup: a replay is not a different act, and it must not keep working after the
marketplace is switched off.

**The general lesson.** "This movement needs no capability" and "this product needs no approval" are
different sentences. The first is about the ledger; the second is about the business. Answering the
first does not answer the second, and Domain A had exactly one place where the two came apart.

## F-16 · `MARKETPLACE` was low risk while gating the minting of withdrawable provenance · BASELINE · P2 · FIXED

`gates.IsHighRisk` classified `MARKETPLACE` as low risk. That was defensible when the capability was
a placeholder meaning "users can sell things some day". It is not defensible now that it gates
internal commerce, because exercising it does two things the function's own stated criterion covers:
it moves Credits between users, and it mints the creator-earning provenance a payout policy may one
day permit to be withdrawn.

Low risk means no legal-review, provider-contract, risk-approval or security-approval reference is
required to propose the gate. It still requires dual authorization -- so this was never a
one-person switch -- but it was a switch two people could throw with no evidence attached to the
only path by which withdrawable provenance is created.

**Fix.** `MARKETPLACE` is high risk, with the reason recorded next to it. The three tests that used
it as their low-risk exemplar now use `SOCIAL_DATA_PERSISTENCE`, which is genuinely low risk and
stays a real test of the low-risk path rather than an assertion that happens to pass.

## F-17 · A gate being off was reported as a policy refusal · NEW · P2 · FIXED

**Found by** an HTTP integration test asserting the status code of a refused purchase and getting
403 where 422 was expected.

The legal router correctly turns an ALLOW whose required gate is inactive into a DENY -- policy and
gate must agree. The first version of the compiler read that DENY as `LEGAL_ROUTER_DENIED`, so a
deployment whose policy already permitted internal commerce and had simply not activated the gate was
told "no approval on record permits this action". An operator reading that would go and edit a policy
that was already correct.

**Fix.** The compiler now distinguishes them: a router DENY whose reason code is
`CAPABILITY_NOT_ACTIVE` produces the capability reason only, with the capability named, because
activating it is the actual next step. A policy that denies on the merits still produces
`LEGAL_ROUTER_DENIED`, and a fresh deployment produces both. `TestCompile_AGateBeingOffIsNotAPolicyRefusal`
pins all three cases.

The refusal precedence at the HTTP edge follows the same principle and is documented where it is
written: rail-not-implemented, then policy, then gate, then verification, then provider -- the order
in which the obstacles would actually have to be removed. Every reason is still in the response; the
ordering only decides which one leads.

## F-18 · The adversarial-coverage summary contradicted its own document · NEW · P2 · FIXED

**Found by** trying to write the per-item mapping and checking each claim instead of copying the
summary sentence.

`PRODUCTION_READINESS_REPORT.md` summarised PART LXXII's thirty required adversarial tests in one
sentence: "items 1–4, 7–11, 15–17, 20–22, 28 have named tests; items 5, 6, 12–14, 18, 19, 23–27,
29, 30 do not yet". Four of those placements were wrong, in both directions:

- Item 6 (provider timeout after success) was listed as missing. `TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout`
  is cited by name **in the payout evidence table of the same document, sixty lines above**.
- Item 12 (hidden supply increase) was listed as missing.
  `TestIntegration_ACreatorCannotChangeEconomicsAfterLaunch` covers it, and is also cited above.
- Item 13 (self-trade) was listed as missing. The surveillance test raises `CREATOR_SELF_DEALING`.
- Items 26 and 27 were listed as missing and were covered by tests that predate this migration.

Nothing was untested that the summary said was tested — the error ran the safe way — but that is
luck, not a property of the process. The sentence was written by recalling what had been built rather
than by checking, which is the same failure mode as F-14 wearing different clothes.

**Fix.** The sentence is replaced by a thirty-row table in `PRODUCTION_READINESS_REPORT.md` §3a, one
row per required scenario, each naming the test. Three items (5, 14, 23) genuinely had no test and
were named, with what each would take. A summary a reader cannot check is worse than a table they
can. All three were closed afterwards — item 23 by way of F-23 — and the table is now thirty of
thirty.

**And then applied to the rest of the document.** Every "not built" claim was re-checked the same
way, which immediately caught another: the report said `apps/admin` had no pages driving the nine new
administrative actions. It has one. The console's propose form is generated from `authority.json`,
which now lists all nine, and carries the free-form params field the two parameterised kinds need, so
they are operable today. The corrected claim is narrower and true: there is no kind-SPECIFIC UI, so
an operator pastes a uuid where a picker belongs.

## F-19 · A `Route` field called `MaxAgentAuthority` held a minimum · NEW · P3 · FIXED

**Found by** re-reading `internal/settlement/compiler.go` looking for exactly this: a name that says
one thing while the value says another.

`Route.MaxAgentAuthority` was populated from `agentauthority.MinimumLevel(...)` — the LOWEST level at
which an action becomes available — and documented as "the highest authority level that may execute
this action". Nothing consumed it yet, which is the only reason it was harmless. The obvious way to
consume it would have been `if agent.Level <= route.MaxAgentAuthority`, which is the exact inversion
of the intended check and would have permitted only the agents that should have been refused.

**Fix.** Renamed to `MinAgentAuthority`, documented as a floor, and pinned by
`TestCompile_MinAgentAuthorityIsAFloorNotACeiling`: an agent at exactly that level may act, one below
may not, and a permanently forbidden action has no minimum at all because no level permits it.

**Why P3 and not P2.** No caller existed. Recorded anyway, because "nothing uses it yet" is a
statement about today and the name would have outlived the memory of what it meant.

## F-20 · A failure to ASK was reported as an answer about the session · BASELINE · P2 · FIXED

**Found by** the browser suite failing intermittently while building the Domain A pages — two or
three tests per run, never the same ones, always with the page rendering the sign-in screen. The
tests that failed were about pages I had not touched, which is what made it worth chasing rather
than working around.

`SessionProvider` computed `signedIn = me.isSuccess`, and `App` rendered `<SignIn />` whenever that
was false. So EVERY way of failing to read `/v1/me` — a dropped connection, a timeout, a 500, a 429 —
produced the sentence *"This session is not signed in."*

That sentence is a claim about the customer's session, and the application did not know it. The
session was fine. What had actually happened was:

```
code RATE_LIMITED · HTTP 429
```

The suite makes enough requests to trip the transport rate limiter, the limiter did exactly its job,
and the interface translated "I was told to slow down" into "you are logged out". A customer hitting
a rate limit would have been shown a sign-in screen and would reasonably have concluded their session
had been terminated.

It was also self-concealing: the same code path discarded the real error, so the 429 never reached
the screen. It took a fix to make the cause visible — the first run after the change failed with the
rate-limit message rendered in full, which is how the root cause was finally identified.

**Fix.**

- `SessionValue` gains `signedOut`, true only when the backend actually answered 401. `App` shows the
  sign-in screen for that and only that; every other failure renders the real problem with a retry
  and the line *"This is a failure to reach the backend, not a statement about your session."*
- `useMe` no longer refuses to retry everything. A 401 is an answer and is never retried; anything
  else gets one more attempt before the application concludes anything about the session.

**Verification.** Three consecutive full browser runs, 66/66, stable at ~52 s. Before the fix the
same suite failed 1–2 tests in three runs out of four.

**The harness half.** The e2e run also needs the transport limits raised, because a 66-test browser
suite is not a human being:

```
CP_API_RATE_LIMIT_GENERAL=100000/1m CP_API_RATE_LIMIT_COMMAND=100000/1m \
CP_API_RATE_LIMIT_QUOTE=100000/1m   CP_API_RATE_LIMIT_AUTH=100000/1m
```

That is a fact about the harness, not a defect: the limiter refuses to be switched off in a
production-like environment, which is correct, and the default budget is the right one for a person.

**The general lesson.** "The answer was no" and "I could not ask" are different facts, and an
interface that collapses them tells the customer something false at the moment they are least able
to check it. The same distinction is why the Settlement Compiler separates a policy denial from a
gate being off (F-17) — this is that principle at the other end of the system.

## F-21 · A chaos negative control could not fail · NEW · P2 · FIXED

**Found by** running the negative control and watching the test PASS.

`test/chaos` holds itself to a rule: every guard carries a named control that
makes it violate the property it defends, so the assertion can be observed
firing. "A guard that has never been seen to fail is recorded as unproven."

The first `commerce_partial_write` control consumed the buyer's Credit lots in a
separate transaction with no posting behind them, on the theory that provenance
would then claim fewer units than the ledger held. It could not construct the
fault it was named after, the test passed with it active, and the guard was
therefore proving nothing.

**CORRECTION (F-40).** This paragraph used to say the reason was migration
00711's SQLSTATE CR004, "which refuses a lot EVENT whose journal transaction
never touched the account, so that drift is unrepresentable". That is false, and
it is exactly the kind of false statement this register exists to catch. CR004
is raised only by `cp_credit_lot_open`, the trigger on `credit_lots` INSERT.
`cp_credit_lot_apply_event` never reads `NEW.journal_transaction_id`, and the
column is nullable on `credit_lot_events` — a CONSUME event with no posting
behind it IS accepted by the database. Why the original control did not
construct the fault is recorded again below, correctly; the sentence claiming a
database guarantee was written from the wrong trigger and stood for a session.

This is the more dangerous shape of the problem the file's own header warns
about. A guard nobody exercised is an unknown. A guard whose control silently
passes is false confidence with a certificate attached.

**Fix.** The control is the mirror image: post the Credit movement and skip the
consumption, so the ledger says the buyer paid while provenance still claims
every unit. It now fires, with `VerifyProvenance` producing "buyer provenance
and ledger diverged after the fault". `native_trade_partial_write` was written
the same way and was checked the same way. Both are recorded in `breakNames`
with the reason the mirror image is not usable, so the next person does not
retry the dead end.

## F-22 · A load script reported idempotency conflicts as price changes · NEW · P3 · FIXED

The first run of `test/load/internal_economy.js` reported 16
`internal_purchases_refused_price_changed`. No price had changed. The script
counted refusals by HTTP status, and 409 is three different things on that
endpoint: the price moved (`CONFLICT`), the same key is still in flight
(`IDEMPOTENCY_IN_PROGRESS`), or the same key came back with a different body
(`INVALID_IDEMPOTENCY_REUSE`). What it had actually measured — the shared-key
iterations racing, and the idempotency store correctly refusing the second —
was the more interesting result and was the one it threw away.

**Fix.** Refusals are counted by the CODE the backend gave, with a separate
counter for each. The threshold moved too: it was `http_req_failed < 1%`, which
counts every expected refusal as a failure and would fail a run in which the
system behaved perfectly. It is now "never a 5xx" and "always problem+json",
which are the things that must hold whatever the business answer is. An operator
who has learned to ignore a red threshold has lost the threshold.

---

## F-23 · Reconciling a finished payout reported agreement it never obtained · NEW · P2 · FIXED

**Found by** writing PART LXXII item 23 — a compromised provider sending a
contradictory status — and discovering the scenario was not reachable, which is
a different thing from being handled.

`Service.Reconcile` opened with:

```go
if req.State.Terminal() {
    return req, nil
}
```

That guard is right about the state machine and wrong about the operator. Not
moving a settled or failed payout on the word of a provider is the whole point:
`FAILED` and `REJECTED` have no outgoing transitions and `SETTLED` leads only to
`REVERSED`, so a provider that changes its story cannot move Nodal's money by
lying twice. But the early return also meant the provider was never ASKED. An
operator who reconciled a settled payout got `err == nil` and the request back,
which reads as "the provider confirms it", and the provider had not been
contacted at all. A contradictory answer left no trace anywhere.

That is the same shape as F-14 and F-20: not a wrong action, a **false report**
of a check that did not happen.

**Fix.** A terminal request with a provider key is now looked up, the answer is
recorded as a `payout_provider_events` row whatever it says, and a definite
disagreement — settled-now-failed, failed-now-settled, or settled under a
different provider reference — is returned as `CodeReconciliationRequired` and
logged at ERROR. **No state moves**, because that is the control; what changed
is that the contradiction is now visible instead of discarded.

A provider that has gone back to `ACCEPTED` after settling is deliberately NOT
called a contradiction: read-after-write lag on the provider's side produces
exactly that, and an alarm a coincidence can trigger is one operators learn to
ignore. The same reasoning governs the wash-trade detector added for item 14.

Four tests, including the control that fails if every terminal reconcile is
called a contradiction:

- `TestIntegration_ACompromisedProviderCannotRewriteAFinishedPayout`
- `TestIntegration_AProviderClaimingItPaidAFailedPayoutIsNotBelieved`
- `TestIntegration_AProviderSwappingTheReferenceOfASettledPayoutContradictsItself`
- `TestIntegration_AProviderThatAgreesWithASettledPayoutIsNotAContradiction`

Reaching the scenario at all needed a provider double that can contradict
itself. `payouttest.Sandbox` is deliberately incapable of it, because a correct
provider is; the affordance is a single `Corrupt` method, named after what it
models, in the package the production registry refuses to load.

## F-24 · A deployment's legal policy never reached the intent compiler · NEW · P2 · FIXED

**Found by** reading `git status` and noticing a file that should have been
modified was not.

Putting Domain B and C trade intents through the settlement compiler needed two
halves: a `Ports.SettlementPolicy` the handler reads, and an assignment in
`Wire` that fills it from the deployment's `WireDeps.NativeEconomy`. A script
that applied several edits at once asserted its way out on an earlier step, and
the assignment never landed.

The result: every deployment's trade intents were compiled against the ZERO
VALUE — no legal policy on record, no capability active, no verification,
unknown jurisdiction. That is the conservative policy, so nothing became
permissive; but a deployment that had configured a real policy would have been
refused by a policy it never chose, which is a different wrong answer.

**Nothing caught it.** `TestIntegration_ARealCapitalIntentIsRefusedAtTheEdge`
passed with the assignment missing and passes with it present, because a
harness that configures no policy and a compiler that cannot see the policy
produce the identical refusal. This is F-21's shape again in a different place:
not a guard that failed, a guard that could not distinguish the two cases it
existed to distinguish.

**Fix.** The assignment, plus the two tests that can tell the difference:

- `TestWire_CarriesTheDeploymentsSettlementPolicy` asserts `Wire` carries the
  configured router through, and that an unconfigured deployment still gets the
  conservative policy rather than a nil one. **Observed failing** with the
  assignment removed.
- `TestIntegration_AConfiguredPolicyActuallyReachesTheIntentCompiler` wires a
  policy that PERMITS self-custodial trading and asserts the answer changes —
  the policy version in the refusal is the deployment's own, and the leading
  reason moves from the policy to the gate.

`Ports.SettlementPolicy` is a struct rather than an interface for the same
reason the finding is only P2: its zero value is the conservative deployment,
so the failure mode of forgetting it is refusal rather than permission.

## F-25 · A readiness claim pointed at a test that does not exist · BASELINE · P3 · FIXED

**Found by** asking, mechanically, whether every test name the documents cite
resolves to a declaration.

`docs/release/PRODUCTION_READINESS_REPORT.md` offered
`TestIsolation_CrossDomainPostingMustDeclareItself` as the evidence that a
cross-domain movement must declare its conversion. No such function exists. The
test is `TestIsolation_CrossDomainPostingMustDeclareItsConversion`, and the
property IS proven — so the defect is in the citation and not in the system,
which is exactly why it survived: everything a reader could check by hand said
the right thing, and the one thing they would have had to grep for did not.

The wider problem is the habit rather than the row.
`REQUIREMENTS_TRACEABILITY.md` states that "all 609 Go test-function references
resolve to a `func Test`/`func Fuzz` that exists", hand-verified on one
afternoon. A hand-verified claim about 609 things is one that goes false
without anybody knowing which week it happened. It is F-14 and F-18's shape
again: a document asserting something about the test suite, written by
recalling rather than by checking.

**Fix.** The name is corrected, and `test/docs/references_test.go` now enforces
the property for the five documents a reviewer would actually use to decide
readiness. It is deliberately NOT applied to the whole `docs/` tree:
`THREAT_MODEL.md` and `SECURITY.md` legitimately quote the names of tests they
assert do NOT exist, and a check that could not tell an assertion from a
quotation would force those documents to lie to satisfy it.

A cited name resolves if a declaration matches it exactly, or if it is a family
prefix at an underscore boundary — prose that says "the `TestProp_` suite" is
naming a group, not claiming one function.

**Still open, and named rather than fixed:** `THREAT_MODEL.md` and
`SECURITY.md` carry a 2026-09-05 inventory that lists as absent a dozen
packages that now exist, including a `TestAgentImportBoundary` whose property
is in fact covered three times over (`TestAgentTreesNeverImportAuthority`,
`TestAgentBoundaryNeverImportsSigning`, `TestNoAgentPathImportsWithdrawal`).
That staleness is in the pessimistic direction and was already recorded in the
readiness report; re-scoring those two documents is work, not a claim.

## F-26 · The internal economy was unreachable in every deployment · BASELINE · P1 · FIXED

**Found by** trying to run the Stage 21 load script against a marketplace whose
gate had actually been activated, and watching every purchase refused anyway.

Two inputs the settlement compiler requires were never supplied by `cmd/api`,
and each alone was enough to make every Domain A action impossible.

**The verification level.** `NativeEconomyDeps.Verification` was `nil`, so every
account resolved to `VerificationNone`. Every Domain A profile requires
`NODAL_IDENTITY`. The reasoning recorded in BLOCKERS B-06 — "a deployment that
cannot establish identity has not established it" — is right about a KYC
provider and wrong about this rung of the ladder. `NODAL_IDENTITY` is "a
verified email address and/or passkey: a person can be reached and can log in",
which this system establishes at login and persists as `users.email_hash`.
Reporting NONE for it was not conservatism, it was under-reporting.

**The capability.** `gateCapabilityResolver` answers only about the keys in
`conversionCapabilities`, and a key it does not answer about is reported
inactive. `MARKETPLACE` was not in that list. So the gate could be ACTIVE in the
database, three principals could have approved it with evidence, an operator
could have enabled it in configuration, and every purchase would still be
refused `CAPABILITY_NOT_ACTIVE` — indistinguishable from a gate nobody had ever
approved.

Both are fail-closed, which is why they were invisible: refusals looked exactly
like the refusals a correct fresh deployment produces. But a control that no
configuration can ever satisfy is not a control. It is dead code with a reason
attached, and the whole Domain A surface — the marketplace, native asset
creation, native market trading — sat behind it.

**Fix.**

- `internal/identity.NodalIdentityResolver` reports `NODAL_IDENTITY` when the
  owning user is ACTIVE and the identity provider asserted a verified email
  address, and NOTHING above it. `PAYOUT_KYC` and `ENHANCED` remain unreachable
  and B-06 is narrowed to what it actually blocks: payouts. A passkey-only
  account with no email still reports NONE, which is under-reporting again, in
  the same direction, and is named rather than hidden.
- `internal/identity` now fills `users.email_hash` on a later login when it is
  absent, because it used to be written only at user creation: an account
  created before its email was verified could never reach NODAL_IDENTITY
  afterwards however many times the provider asserted it. It fills an absence
  and never overwrites.
- `MARKETPLACE` and `LIVE_MANUAL_TRADING` are added to the resolver's list, and
  `settlement.AllRequiredCapabilities()` is exported so the list can be CHECKED
  rather than remembered: `TestCapabilities_ResolverAnswersEverythingTheCompilerCanRequire`
  fails on any capability a compiler profile can require that the resolver never
  answers about. **Observed failing** with MARKETPLACE removed again, naming it.

After the fix a purchase committed over HTTP for the first time.

## F-27 · A capability check inside a transaction deadlocked the connection pool · BASELINE · P1 · FIXED

**Found by** running 50 concurrent purchases once F-26 made purchases possible,
and finding p95 latency of **30.07 seconds**, five commits out of two hundred,
three 500s and 124 requests killed by the statement timeout.

`pg_stat_activity` said it plainly: nine of ten pool connections `idle in
transaction`, each waiting on `ClientRead` with `begin isolation level read
committed` as its last statement. The database was not busy. It was holding
transactions open for clients that had gone away to do something else.

The something else was the capability check. `ledger.Service.Post` runs
`checkDomainIsolation` inside the caller's transaction, and
`commerce.Service.Purchase` runs `requireMarketplace` inside its own; both asked
the capability resolver, and the production resolver held a `db.Querier` that
was **the connection pool**. So every financial write, while holding one pooled
connection for its transaction, reached for a second one — twelve times, once
per capability.

With a pool of ten and a dozen concurrent writes, every connection is held by a
transaction whose owner is waiting for a connection that will never be released.
Nothing recovers until the 30-second statement timeout, and then it all fails at
once.

**Why nothing caught it.** Every test supplies a MAP as the capability resolver,
so only the production resolver touches the database and only production could
deadlock. And the one place it would have shown — concurrent purchases through
the API — could not commit a single purchase because of F-26. Three defects
stacked so that the third was unobservable until the first two were fixed.

**Fix.** Both resolver interfaces take the caller's `db.Querier`, which is the
transaction the posting is being written in, so no financial write ever reaches
for a second connection while holding its first. `gates.Checker.ActiveSet`
answers every capability in ONE query instead of one per capability, so the
round trips inside a transaction go from twelve to one. The configuration check
still runs first and per capability, so no database read can be what enables a
capability.

**Measured, same script and host, before and after:**

| | p95 latency | committed | 5xx | timed out |
|---|---|---|---|---|
| before | 30.07 s | 5 of 200 | 3 | 124 |
| after | 469 ms (10.3 ms on successful requests) | 31 of 200, the entire seeded balance | 0 | 0 |

`TestIntegration_ConcurrentPurchasesDoNotStarveTheConnectionPool` holds the line
with a pool of TWO, which makes the failure certain rather than probable.
**Observed failing** — hanging until its own deadline — with the resolver put
back on the pool.

## F-28 · No deployment could launch a native market · BASELINE · P1 · FIXED

**Found by** continuing the re-audit the same way F-26 and F-27 were found:
driving the other half of Domain A through a live API instead of reading it.

`POST /v1/native-assets` creates an asset in DRAFT. Nothing could move it after
that. `nativeasset.SetStatus`, `nativeasset.Activate` and `nativemarket.Create`
had no caller outside tests — no HTTP route, no administrative action kind, no
executor. So a creator could publish an asset and no operator in any deployment
could ever open its market.

The moderation-verdict executor even says so in its own comment: "Recording
APPROVED does not start trading. Activating a market is a separate act behind
its own capability." The separation is right. The separate act did not exist.

Every existing test built its market by calling the services directly, which is
exactly why nobody noticed that no operator could. This is F-26's shape a second
time: a path that only tests can walk looks finished from inside the tests.

**Fix — the two halves of the chain, kept apart on purpose.**

- **The creator's half.** `POST /v1/native-assets/{assetId}/submit` moves the
  creator's own DRAFT to PENDING_REVIEW. Only the creator, and only their own:
  a stranger gets NOT_FOUND rather than FORBIDDEN, because telling somebody an
  asset exists but is not theirs is a membership oracle. Submitting is what
  freezes the economics for review, so an operator cannot launch a draft its
  creator is still editing.
- **The operator's half.** `NATIVE_MARKET_LAUNCH` is a new administrative
  action kind: **dual control**, a five-minute step-up window and a one-hour
  expiry. It is the only action in the internal economy that MINTS — every unit
  that will ever exist is created by the posting it triggers, and the economics
  lock behind it — so it is proposed by whoever may halt a market and approved
  by whoever may resume one, the same pairing that guards every other addition
  of risk. An approval to launch is an approval of THESE economics at THIS
  moment, which is what the short expiry is for.

The executor refuses an asset moderation has not approved, refuses a DRAFT with
the reason, refuses a missing or non-positive opening price (there is no default
worth guessing — a default would price somebody's asset for them), and is keyed
by the APPROVAL id, so one approval mints one supply however many times it is
executed.

Three tests drive the whole chain through the real surfaces — creator submits,
compliance approves the content, two operators launch, and then somebody buys —
because a launch that produced an unusable market would satisfy every assertion
about states.

## F-29 · A payout could be requested and never withdrawn · BASELINE · P2 · FIXED

**Found by** the same question as F-26 and F-28, asked systematically instead of
one path at a time: which methods that change money have no caller a deployment
can reach?

`payout.Cancel` was one of them. It is well tested — three service-level tests
cover it — and nothing outside the test suite called it. So a user could request
a payout, watch their Credits leave their spendable balance into
PAYOUT_RESERVED, and have no way to get them back. Only an operator could, and
only by running their own tool against the database.

Reserving somebody's money with no path to release it is not a conservative
control. It is a trap, and it is the kind that looks fine in review because the
reservation logic is correct.

**Fix.** `POST /v1/payouts/{payoutId}/cancel`. It returns the reserved Credits
to the exact lots they came from with their provenance intact, and it refuses a
SUBMITTED request — that one may already have been paid, and the only honest way
out of it is reconciliation, which `payout.Cancel` already said.

The endpoint introduces a question the service never had to answer — WHO may
cancel — so that is what
`TestIntegration_AUserCanCancelTheirOwnPayoutAndNobodyElses` tests: a stranger
gets NOT_FOUND rather than FORBIDDEN, because a distinguishable refusal is a
membership oracle, and the refused attempt changes nothing.

## The class, made mechanical

F-26, F-28 and F-29 are one defect three times: **a path only tests can walk
looks finished from inside the tests.** Every one was correct, covered, and
unreachable.

`test/reachability` is that question turned into a check. Every exported method
on a financial service that takes a transaction — every method that changes
money — must have a caller in `internal/`, `cmd/` or `scripts/`, or an entry
saying in words why not. A caller in `test/` does not count; that is the point.

Eleven methods are exempt today and every reason is external: the Credit funding
lifecycle needs a payment provider (B-04), payout destinations need a payout
provider to verify them and somewhere to send the money (B-01, B-06), and
PAYOUT_KYC is by definition a provider's determination. A second test refuses
any exemption whose reason does not name a blocker `BLOCKERS.md` actually
defines — an exemption without an external cause is a defect wearing a comment.

**Its first version could not fail.** It matched `.Method(ctx`, and `Cancel`,
`Create`, `Execute` and `SetStatus` are names four of the five packages share,
so every method looked reachable from everywhere. Two changes fixed it: a call
must pass a transaction as its second argument, and the calling file must import
the declaring package. It was then observed failing on both defects it was
written for — `payout.Cancel` with the endpoint removed, and
`nativeasset.Activate` with the launch executor removed.

## F-30 · A property test's shrinker would have mis-explained a real defect · BASELINE · P2 · FIXED

**Found by** `go test ./...` failing on a commit that had just passed the full
integration sweep. Randomised tests do that: the seed changes every run.

`TestProp_ByteFlipsNeverApprove` asserts that flipping bytes of a golden
transaction never yields an approval from the signing inspector. It drew 2
flips, both at position 0, both XOR 1 — and two flips of the same bit cancel.
The "mutated" message was byte-identical to the golden one, the inspector
approved it, correctly, and the test called that a security failure.

The false alarm is the small half. The dangerous half is what rapid does next:
**it shrinks toward small values**, so `flips=2, pos=0, xor=1` is where the
shrinker goes from ANY failure it finds. A real defect — an inspector that
approved some genuinely mutated transaction — would have been shrunk into
"identical bytes approved" and reported with an explanation that had nothing to
do with it. The engineer who chased it would have found a test bug, fixed the
test, and never seen the defect.

That is the same shape as F-21 pointing the other way. F-21 was a control that
could not fail; this is a control that fails for the wrong reason and hides the
right one.

**Fix.** The case is discarded when the mutation cancels out, with the reasoning
in the code, because the property is about MUTATED transactions and a
bitwise-identical one is not mutated. Discarding it also keeps the shrinker away
from it.

**Both controls were run.** Making the guard swallow every case leaves the test
passing while proving nothing — which is why the guard is `bytes.Equal` and not
anything broader. Making the inspector approve everything makes the test fail,
so the property still catches what it is for. The property was then run six
times at 3,000 checks each, and the whole property tier three times over.

The stale `testdata/rapid/...fail` file rapid wrote is deleted: left in place it
pins every future run to the degenerate case.

## F-31 · Four concurrent buyers failed once and nobody could say why · NEW · P3 · FIXED

**Status: reproduced and closed.** It was recorded rather than closed because
"it passed the next eight times" is not a diagnosis. The diagnosis arrived on
its own, in a full 50-package sweep run against a loaded machine — see
**Reproduced** below.

`TestIntegration_ConcurrentBuyersSerialiseWithoutBreakingAnything` runs 100
concurrent buys against one market. On one run it reported `every funded buyer
should succeed; 4 failed`, and passed on the immediately following run against
the same database.

**Nobody can say why, and that is the finding.** The test counted failures and
threw the errors away, so a failure produced a number with no cause attached.
That is now fixed — it keeps the first error and reports it — but the run that
failed is gone and the message with it.

What is known:

- The failing run took 24.2s against 16–20s for every subsequent run, on a
  machine that was simultaneously serving a k6 load test and another suite.
  A timeout under contention (`lock_timeout` is 5s, `statement_timeout` 30s)
  is the most likely explanation.
- It did not recur in eight consecutive full-suite runs or six runs of the test
  alone, against the same database.
- The run's OTHER assertions are the ones that would matter: the state version
  moved once per trade, the curve invariant held, and supply reconciled. Those
  are not reported as failing, which means the four buys that failed left
  nothing behind — consistent with a refused transaction rather than a partial
  one.

**Why it is P3 and not higher.** Every financial invariant in that test passed.
A buy that fails is a buy that did not happen, which is the correct outcome of a
timeout; the money property this test exists to defend was not violated. What is
unproven is whether the cause was environmental.

**What would close it.** The error text from a recurrence. The test now captures
it, and CI runs this suite on every commit.

**Reproduced.** In this session's third full sweep, with 50 packages each
provisioning a database and other work on the same machine:

```
--- FAIL: TestIntegration_ConcurrentBuyersSerialiseWithoutBreakingAnything (12.51s)
    expected: int(100)
    actual  : int64(87)
    every funded buyer should succeed; 13 failed, first error:
    CONFLICT: ledger operation timed out waiting for the database:
    ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)
```

The hypothesis recorded above — "a timeout under contention (`lock_timeout`
is 5s)" — was right, and the error now says so, because the earlier fix to
keep the first error is what made the failing run legible. The package passed
three times out of three immediately afterwards when run alone.

**What it means.** The system did the correct thing. A hundred buyers contend
for one market row; a lock wait longer than the deployment's five seconds is
refused with CONFLICT, the transaction rolls back, and nothing partial is
written — which is why every financial invariant in that test kept passing
through every one of these failures.

The test was the thing that was wrong. "Every funded buyer should succeed" is a
claim about throughput on whatever machine happens to be running, and it fails
on a loaded runner while looking like a correctness failure.

**Fix.** The buyer retries a CONFLICT, which is what a client does with one. The
claim the test makes is unchanged and still strong: a hundred concurrent buyers
all get their units, the version moves exactly once per trade, the curve
invariant holds, and supply reconciles against max supply. What is no longer
asserted is that all hundred succeed inside one five-second lock wait.

The retries are bounded at six and counted, and the count is logged: contention
turning into livelock would exhaust them and fail here rather than hide behind
an unbounded loop. The version assertion is what stops a retry from becoming a
double buy — a client that retried after a commit it did not see would push
the version past 100 and fail.

## F-32 · A browser test that could not tell a working page from a broken one · BASELINE · P2 · FIXED

**Found by** noticing an uncommitted change in the working tree — somebody
else's, mid-fix — that made two list queries unwrap `{items: [...]}`, and asking
why 69 browser tests had not caught what it was fixing.

Verified independently of that change: `openapi.yaml` declares
`NativeAssetPage` and `InternalProductPage` as objects with an `items` array,
and the committed client called `validatedList`, which throws
`ContractViolation("expected an array")`. So at HEAD the Native Markets asset
list and the Marketplace product list rendered "This response could not be
trusted" on every load.

**The test that should have caught it asserted only this:**

```
await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
await expect(page.locator("h1")).toHaveCount(1);
await expect(page.locator(".malformed")).toHaveCount(0);
```

The heading renders before any request is made. Every one of those passes on a
page whose every panel is an error. The test proved the route existed.

**And the first fix did not work either.** Adding
`expect(page.getByText("This response could not be trusted")).toHaveCount(0)`
still passed against the broken client — because Playwright retries an
assertion until it passes, and `toHaveCount(0)` passes the instant it is
evaluated, which was before the query had resolved. A negative assertion with
no positive signal before it proves nothing at all. It now waits for
`networkidle` and for every spinner to clear first.

**Observed failing.** With the client's paged-response handling put back to the
committed version, `Native Markets renders` fails. With it restored, all 17
render tests pass against a live API and a real Postgres.

**What is NOT asserted, deliberately.** That no refusal appeared. This
deployment refuses plenty and demanding otherwise would be demanding the
interface lie. The assertion is narrower and is exactly the thing a heading
cannot tell you: that the data loaded at all.

The client-side fix itself is somebody's uncommitted work and is left where it
was found; this finding is about the test that let it through.

**And then the type system was made to catch the class.** `validatedList` took
`unknown`, which threw away what the generated client already knew: it types
`/native-assets` as `{ items: NativeAsset[] } | undefined` and `/accounts` as
`Account[] | undefined`. Calling `validatedList` on a paged response therefore
compiled cleanly and threw on every page load.

The parameter is now `readonly unknown[] | undefined`. Putting the committed
version back produces a compile error on the exact line:

    Argument of type '{ items: {...}[] } | undefined' is not assignable to
    parameter of type 'readonly unknown[] | undefined'

So the failure moves from a customer's screen, past the browser test that could
not see it, to `tsc`. A browser test that catches this is worth having; a
compiler that makes it unwritable is worth more.

**A note on how the other endpoints were checked.** I wrote a script comparing
every client parse against the OpenAPI response shape. Its first version
reported `/assets` as a mismatch — a false positive: `/assets` answers an inline
`type: array` and the script had looked up the ITEM schema and found `items:`
inside it. Corrected, it then skipped most paths for a different regex reason.
Two wrong answers in opposite directions from the same script is a good argument
for the type change above and against the script, which is not kept.

## F-33 · No browser test finished anything · BASELINE · P2 · FIXED

**Found by** asking the F-26 question of the interface: 69 browser tests, and
does any of them prove a customer can COMPLETE something?

None did. The suite covered rendering, navigation, accessibility and the honesty
rules — every one of them a statement about a page at rest. The closest thing to
a transaction was a heading being visible.

That is the same gap that produced F-26, F-28 and F-29 on the backend, one layer
up: a path the tests never walk looks finished from inside the tests.

**Fix.** `a customer can buy something and their Credits fall by exactly the
price`. It reads the Credit balance the customer is shown, buys the cheapest
product on the Marketplace, and asserts the balance fell by exactly the price
and the purchase appears in their own list. It ran against a live API, a real
Postgres and a MARKETPLACE gate activated through the real three-principal
ceremony, and it moved 300 Credits per run.

**It found three defects in itself before it found anything else,** which is
worth recording because each is a pattern:

1. **It read one product's price and clicked another's button.** `.panel`
   matched the outer container, so `.field` with "PRICE" resolved to the FIRST
   product while `.last()` clicked the LAST. Each product is its own
   `<article class="panel-nested" aria-label={title}>`; locating the CARD and
   then its own price and its own button is what makes the assertion about one
   thing.
2. **It asserted a Purchases table was visible.** That table was already there
   from an earlier order, so the assertion passed while nothing had been
   bought. It now asserts the card's own "Bought." notice.
3. **Its refusal branch never matched, and its outcome check ran too early.**
   The branch looked for the problem TITLE and the backend renders the CODE, so
   with the gate off the test fell through to the success assertion and failed
   with "element not found" — a confusing way to be told the gate is closed.
   And `networkidle` returned before the mutation had re-rendered, so the branch
   read an empty card. It now waits for the card to show EITHER a success notice
   or an explanation before deciding which happened.

**Both branches were run against a real deployment.** With MARKETPLACE ACTIVE
the purchase completes and the balance falls. With the capability removed from
`CP_API_ENABLED_CAPABILITIES` the test takes the refusal branch, asserts the
refusal names `CAPABILITY_NOT_APPROVED` and `MARKETPLACE`, and checks the
balance did NOT move. Neither outcome is a green tick over an untested path.

**And it would have failed in CI.** The e2e job seeded users and assets but not
the internal economy, so `/marketplace` had nothing to buy. The new spec would
have found an empty catalogue on its first CI run — a test that needs data
nobody creates is another path only its author can walk. The job now runs
`seedeconomy` and activates MARKETPLACE through `gateceremony`, the same real
ceremony used locally, and starts the API with the development legal policy and
the capability enabled.

Point 3 is the third time in this session that a check ran before the thing it
was checking existed — F-32's first fix, this refusal branch, and this outcome
wait. The pattern is worth naming: **an assertion about an absence, or a branch
on a condition, needs a positive signal before it, or it is only measuring how
fast the test runs.**

## F-34 · The risk kernel was complete, tested, and unreachable · BASELINE · P1 · FIXED

**Found by** following the readiness report's own admission — *"The risk kernel
is not yet an input to the Domain A route. `Route.RequiresRiskEvaluation` is
determined and recorded; nothing consumes it for internal trades yet"* — and
asking the F-26 question of `internal/risk` rather than only of Domain A.

The answer was worse than the report said. It is not that one route fails to
consult the kernel. **Nothing does.** `risk.Store.RecordPolicy` and
`risk.Store.RecordDecision` had no caller outside a test; no migration, script
or endpoint ever wrote a `risk_policies` row; so `risk_policies` was empty in
every deployment, `EffectivePolicy` answered `ErrNoPolicy` everywhere, and
`risk_decisions` had never received a row. Nine hundred lines of limits, kill
switches, hash verification and fail-closed reasoning, none of it reachable.

The kernel's own documentation described the missing step:
`DefaultGlobalPolicyJSON` is commented as *"the compiled-in GLOBAL policy used
only to seed a fresh deployment"*, for a seeding step that did not exist.

That is F-26, F-28 and F-29 a fourth time, and it is the largest instance:
**a path only tests can walk looks finished from inside the tests.**

**Two of the limits did not exist either.** PART XXXII names what the kernel
must constrain "at minimum", and two entries are about this economy
specifically: native-market concentration and creator concentration. Neither was
in `Policy`. They cannot be expressed the way every other limit is, because
every other limit is denominated in USD and a Credit has no approved external
value (PART LIV) — so a USD limit on a native position would need an exchange
rate nobody set. Both are ratios instead, compared as `part * 10000 > whole *
limit` with no division and no float.

**Choosing the denominators was the hard part, and the first two choices were
both broken.** The obvious denominator for market concentration is the FLOAT,
which is also what `surveil` alerts on. It is unusable as a limit: the first
buyer in a new market holds all of the float, so a limit against it refuses the
opening trade of every market that will ever exist. Creator concentration
measured as one creator's share of the account's native SPEND has the identical
defect one level down — an account's first native purchase is necessarily 100%
of its native spend. Both were fixed by changing the denominator to something
that is not degenerate at zero history: total supply, which is fixed at mint and
which no participant can move, and the account's whole Credit position, spent
plus still spendable.

**A refusal that costs nothing to write is the wrong kind of refusal**, so both
denominators were chosen against the real curve rather than in the abstract.
Under the compiled-in defaults (2000 / 3000 bps) five of this package's
fifty-two integration tests began failing, and every one of them was a test
buying a genuinely concentrated position: a 10,000-Credit order that takes 22.5%
of an asset's entire supply, a hundred buyers each putting half their Credits
into one creator. The tests were changed to buy positions inside the limits, and
each carries a comment saying why — the alternative, a fixture policy that
relaxes the limits it is meant to prove, is the test equivalent of turning the
control off.

**Fix, in four parts.**

1. **The limits.** `max_native_market_concentration_bps` and
   `max_creator_concentration_bps` in `Policy`, in `Validate`, in
   `MissingLimits`, in `Compose` (so an ACCOUNT row can only tighten), and in
   `DefaultGlobalPolicyJSON`. `EvaluateNativeTrade` is a second entry point
   beside `Evaluate` that evaluates exactly these two and refuses
   `RISK_POLICY_MISSING` if either is absent, naming the absent ones.

2. **The caller.** `nativemarket.Execute` builds a snapshot — counts and
   Credits, never a price — evaluates it after pricing the fill and BEFORE
   posting anything, and refuses with `RISK_CONCENTRATION`. A SELL is never
   refused: both limits constrain holding too much, and refusing an exit would
   trap a holder in the position the limit exists to discourage.

3. **The seeding step.** `scripts/riskpolicy` records a GLOBAL policy through
   the real `Store.RecordPolicy`. With `-rules` it records an operator's
   document in any environment; with no `-rules` it records the compiled-in
   default and REFUSES outside LOCAL/DEV/TEST, because seeding production with
   starter limits nobody signed off would produce exactly what this audit is
   against: a control that looks decided and is not. `make seed-economy` and the
   CI e2e job run it.

4. **The evidence.** Every decision is persisted. An ALLOW is written inside the
   trade's own transaction, so a decision cannot commit without the trade it
   permitted. A REJECT cannot be — the transaction it refuses rolls back and
   would take the record with it — so the service hands the decision out with
   the error and `nativeMarketsAdapter` writes it in a new transaction once the
   failed one is finished. Not inside it: acquiring a second pool connection
   while holding the first is precisely F-27.

**A wrong answer the control introduced, caught before it shipped.** An order
larger than the account's Credits makes the creator-concentration denominator
smaller than its numerator, so the ratio exceeds every limit and the kernel
answered `RISK_CONCENTRATION` — for what is really "you do not have that many
Credits". The trade was correctly refused either way, which is exactly why
nobody would have noticed: a refusal that is right about the outcome and wrong
about the reason sends somebody to read a concentration policy. The limits are
now not evaluated for a trade the account cannot pay for; the ledger's own
negative-balance guard produces that refusal, and
`TestIntegration_AnOrderBiggerThanTheBalanceSaysSo` was observed reporting
`RISK_CONCENTRATION` with the guard removed.

**What the refusal says.** Both limits have an obvious remedy — a smaller order
— and a customer shown `RISK_NATIVE_MARKET_CONCENTRATION` learns that something
was refused and nothing about what to do, which reads as a fault rather than a
limit. The detail names the limit in words and says a smaller order may be
within it; the reason codes stay on the problem for anything machine-read. The
one case that does NOT say "try a smaller order" is a refusal caused by the
deployment rather than the order — a policy missing its limits — because that
would send somebody to fix something that is not theirs.

A deployment with no risk policy at all answers `UNSUPPORTED` with "this
deployment has recorded no risk policy, so no internal trade can be evaluated",
not an opaque `INTERNAL`. It is true, and the fix is `go run
./scripts/riskpolicy` rather than a bug report.

**Observed failing before being believed.** All five new
`internal/nativemarket` integration tests fail with the `checkRisk` call
removed, `TestIntegration_ARefusedTradeStillRecordsItsRiskDecision` fails with
`no rows in result set` when the adapter's `recordRiskRefusal` call is removed,
and the overspend test fails with the affordability guard removed.

**What this did NOT fix.** `internal/risk` is still unreachable for everything
except native trades: no Domain B or C route consults it, and `RecordPolicy` has
no administrative endpoint — an operator changes limits by running a script
against the database rather than by two operators through the admin plane, which
is what PART 58's risk-desk sign-off deserves.

`internal/eligibility` is in the identical position, and for the same reason:
`Store.RecordPolicy` and `Store.RecordDecision` have no caller outside their own
tests, because the settlement PLANNER that would consume an
`eligibility.Decision` is part of the Domain B and C execution path this
deployment does not run. That is checkable rather than asserted — nothing
outside a `_test.go` file and `settlementtest` constructs a `settlement.Input`
at all, so the struct field that holds the decision has never been filled by a
deployment. That is recorded here rather than raised as a separate
finding, because it is the same fact the readiness levels already state — but it
should be read as "the eligibility kernel has never run either", not as
"eligibility is covered".

`internal/risk` is now in `test/reachability`'s package list, so the answer
cannot quietly go back to no.


## F-35 · `make lint` had never passed · BASELINE · P2 · FIXED

**Found by** running it, after F-34's work touched enough packages to make it
worth checking. It is the third target in this session found red on arrival, and
the same argument applies as to `make sast`: **a check that can only fail is a
check nobody runs.**

Three separate causes, and only the third was a real defect count:

**1. `fmt-check` was red on fifteen committed files.** Not files this session
wrote — `internal/agentauthority`, `internal/commerce`, `internal/credit`,
`internal/payout`, `scripts/seedeconomy` and others had been committed
unformatted. `gofumpt -w` on all fifteen; the diff is whitespace and struct
literal bracing.

**2. `misspell` was arbitrating dialect, and losing either way.** Configured
`locale: US`, it reported 56 findings across 27 files — `cancelling`,
`behaviour`, `catalogue`, `serialised`, `honour`, and `CHEQUE`, which is a
payout instrument kind whose spelling IS the name of the thing. Switching to
`locale: UK` produced 692, because the repository genuinely mixes: `sanitizes`,
`analyze` and `authorization` sit beside the British spellings.

Neither locale can pass without rewriting correct prose, so the locale is now
unset. misspell still catches actual typos, which is what it is for, and stops
adjudicating a house style nobody wrote down. Imposing one dialect across 27
files is a real change worth making on its own; making it as a side effect of a
linter setting, in the same commit as a risk kernel, is not.

**3. Fourteen genuine findings, all fixed.** The ones worth naming:

- **Six unchecked type assertions in the commerce integration test.**
  `created.raw()["product_id"].(string)` panics when the field is absent, which
  is exactly what a handler that started returning a Problem instead of a
  product looks like — so the failure mode was a panic inside a helper rather
  than an assertion naming the field. Replaced with a helper that requires the
  field and says which one is missing.
- **Three doc comments orphaned by insertions.** `GetBySpotPair`,
  `AllRequiredCapabilities` and `StoredQuote` each carried the FIRST LINE of
  another symbol's comment, because a new function had been inserted between a
  comment and the thing it documented. In each case the original symbol was left
  undocumented and the new one was introduced by a sentence about something
  else. Moved back.
- **A bidi control character in `nativeasset_test.go`.** The same Trojan Source
  issue `make sast` found in `moderation.go` earlier this session, in the test
  that proves bidi names are refused — and invisible to gosec, which builds only
  the default tag set. Written as `\u202e` now, like the source it tests.

**Trojan Source made mechanical.** That was the third occurrence in this
project, each found by a different tool, and each in a file the other tools do
not read: gosec builds only the default tag set so it never sees a file behind
`//go:build integration`; staticcheck reads Go and nothing else; neither reads
Markdown, and a sweep found the character in TWO readiness documents — including
the paragraph describing its removal from the code.

`test/source` reads every text file in the repository, whatever the extension
and whatever build tag guards it, and refuses the nine bidi embedding, override
and isolate codepoints. U+200E and U+200F are deliberately not among them: they
are ordinary characters in multilingual text and do not reorder a line. There is
no allowlist, because the one file that legitimately needs these codepoints
already carries them as escapes, which is strictly better than the literal. It
was observed failing on a planted override, and it names the file, the line and
the codepoint — the only three things that help, given the character itself
cannot be seen.
- **A negated parenthesised pair in the isolation check.**
  `if !((declared.From == a && declared.To == b) || ...)` — correct, and exactly
  the shape a reviewer misreads. F-03 was a direction-blind isolation check, so
  the condition is now named (`declaresThisPair`) rather than read through a
  `!`.

**And the two formatters disagreed.** `scripts/fmtcheck` ran plain `gofumpt -l`
while `.golangci.yml` sets `gofumpt.extra.group-params: true`, so `make fmt`
could produce a file `make lint` rejects — which is how two of the fourteen were
found. fmtcheck now passes `-extra`, and the 25 files that difference had been
hiding are formatted.

`make lint` exits 0.

## F-36 · A read permission authorized writes, and ADMIN holds the customer surface · BASELINE · P1 · FIXED

**Found by** asking an adversarial re-audit of the HTTP authorization surface
one question: for every handler that takes an id from the request, is ownership
actually checked? It is — by `security.RequireAccount`, which was the only
tenant check on the write routes as well as the read ones.

`RequireAccount` returns nil for any non-agent principal holding
`account:read_any`. That is correct for a read and is what the permission is
named for. The write routes used the same function.

**Why that is not merely untidy.** `RoleAdmin` is defined as
`except(allPermissions, dualControl ∪ agentOnly)` — every permission that is not
an approve-side or agent-intrinsic one. So an ADMIN holds `account:read_any`
AND `native_market:trade`, `commerce:buy`, `commerce:sell`, `payout:create`,
`withdrawal:create`, `funding:create` and `credit:purchase`. Those customer
permissions are there so an operator can use their own account. Combined with
the read override, one ADMIN session could do this:

    POST /v1/native-markets/{marketId}/orders
    {"account_id": "<any customer>", "side": "BUY", "amount": "1000000000", ...}

and spend that customer's Credits. Likewise `POST /v1/internal-products/{id}/orders`
(buys with their Credits), `POST /v1/payouts` (moves their Credits into
`PAYOUT_RESERVED`), `POST /v1/internal-sellers` (registers them as a seller).
No second signature, no step-up, no `admin_actions` row, no reason recorded —
the audit stream shows an operator making a customer's request, which is
precisely what it would look like if the customer had made it.

**Fourteen write routes were scoped this way.** Every account-scoped
`POST`/`PUT`/`PATCH`/`DELETE` in the API.

**Fix.** `security.RequireAccountOwner` — ownership only, no operator override —
and `accountScopeWrite` in the HTTP layer, on all fourteen. An operator who
needs to change a customer's position uses the admin plane, where the act needs
a reason, a second principal for anything consequential, and a permanent row
naming both. Reads are untouched: `account:read_any` still reads any account,
which is what it is for.

**Made mechanical.** `TestAccountScope_EveryWriteUsesOwnershipOnly` reads this
package's own source and fails on any mutating handler that scopes through the
read helper, naming it. Its negative control requires the write helper to be in
real use, so a green run cannot mean "the regexes stopped matching". Both were
observed failing.

**And proved over HTTP.**
`TestIntegration_AnOperatorCannotTradeOutOfACustomersAccount` puts a real ADMIN
principal on a real market with a real funded customer, asserts 403, asserts the
balance did not move — and then asserts the same operator can still READ that
account and still trade out of their OWN. A fix that broke either of those would
have passed a test that only checked the refusal.

**No test could have caught it.** Every principal in
`TestIntegration_CommerceRefusesCrossTenantRequests` is a customer; the
`TestIDOR_*` family uses customers and SUPPORT_READ_ONLY. Nothing anywhere put
an operator principal on a customer write route with a foreign `account_id` —
the combination was not modelled, so the suite agreed with the code.

## F-37 · The customer trade route had never worked, in any deployment · BASELINE · P1 · FIXED

**Found by** F-36's test failing for the wrong reason. Proving an operator is
refused needed a real request through the real port, and the first honest run of
that request — by the account's own owner — returned

    400 VALIDATION_FAILED: an order needs effective_at

`nativemarket.ExecuteRequest` requires `EffectiveAt`. The handler does not set
it, because a handler must not invent time. The adapter does not set it either
— and the payout adapter and the commerce adapter both do, from the deployment
clock, three files away. So **every** request to `POST
/v1/native-markets/{marketId}/orders` had been refused since the route was
written.

**Why nothing noticed, which is the whole lesson.** The path was covered
everywhere except where it mattered: `internal/nativemarket`'s integration tests
call `Execute` directly with an `EffectiveAt`; the chaos suite and the restore
drill do the same; the browser suite buys from the Marketplace, which goes
through `commerceAdapter`; and `test/load/internal_economy.js` says in its own
comment that it deliberately does not create native-market trades. Fifty-two
integration tests, seventy browser tests, a load script and a chaos suite, and
not one of them sent this request.

That is F-26, F-28, F-29 and F-34 again — **a path only tests can walk looks
finished from inside the tests** — with a new wrinkle: here the tests walked a
path *beside* the real one. The domain service was exercised exhaustively; the
route into it was not exercised at all.

**Fix.** `nativeMarketsAdapter.Execute` stamps `EffectiveAt` from
`deps.now()`, like its siblings. `TestIntegration_ACustomerCanTradeOverHTTP`
walks the route as a customer and asserts the Credits that left the account are
the Credits the fill reports; it was observed failing with the stamp removed.

## F-38 · A policy DENY carrying the gate's reason code became a permission · BASELINE · P1 · FIXED

**Found by** an adversarial re-audit of the settlement compiler, asking which
inputs produce a permissive `Route`.

F-17 established a real distinction: a DENY the router produced only because a
capability gate is off is not a policy refusal, and reporting it as
`LEGAL_ROUTER_DENIED` sends an operator to change a policy that is already
correct. The compiler drew that distinction by comparing the decision's
`ReasonCode` against the literal `"CAPABILITY_NOT_ACTIVE"`.

A reason code is a string a policy author writes. `Policy.Validate` required one
to be non-empty and nothing more — there was no reason-code vocabulary. So a
hand-authored rule:

    {Match: {Jurisdiction: "US-NY"}, Outcome: Deny, ReasonCode: "CAPABILITY_NOT_ACTIVE"}

validated, matched, and was then read by the compiler as a gate refusal. With no
`RequiredCapability` on the rule there was nothing for the gate check to catch
either, so the route came out with **no reasons at all** and `Permitted: true`.
The deployment's own policy said no and the compiler said yes.

**Two fixes, because one of them is the control and the other is the reason
nobody can reach it.**

1. `legalrouter.Decision.GateRefusal` — a field set at exactly one place, where
   the router converts an ALLOW whose gate is off into a DENY. The compiler asks
   that instead of the string. It cannot be forged, because a `Rule` has no such
   field.
2. `ReasonCapabilityNotActive` is reserved: `Policy.Validate` refuses a rule
   that writes it by hand, naming the rule. Without this, a policy could still
   make every refusal message lie about which of the policy and the gate
   refused.

A belt-and-braces line was added with them: a router `Deny` that produced no
reason at all now adds `LEGAL_ROUTER_DENIED` unconditionally. It cannot fire
today — "cannot happen" is what the string comparison assumed too, and the cost
of being wrong is a permissive route.

**Observed failing.** `TestRoute_GateRefusalIsSetOnlyWhereTheGateRefused` fails
with the field assignment removed, and `TestPolicy_TheGatesReasonCodeIsReserved`
fails with the reservation removed. The compiler-level test is labelled in its
own comment as a regression guard rather than a proof, because the exploit can
no longer be constructed through `legalrouter.New` and it therefore passes
against the old code as well.

## F-39 · The no-float linter never looked at the internal economy · BASELINE · P2 · FIXED

**Found by** an adversarial re-audit of the money spine sweeping for floats, and
then asking what stops one being added.

`scripts/lintfin` enforces "no float in a money path" over a hard-coded
directory list: `money, ledger, capital, risk, positions, valuation, quote,
settlement`. Every package of the internal economy is absent —
`internal/credit`, `internal/nativemarket`, `internal/commerce`,
`internal/payout`, `internal/valuedomain`. Domain A was built after that list
was written and nobody extended it.

There is no float in any of them today; the sweep confirmed it. What did not
exist was the control, and "there is no float" and "a float cannot be added
without the build failing" are different claims — the second is the one the goal
document asks for.

**Fix.** The five packages are in the list. The linter was run against them and
is clean, and a deliberately-planted `float64` in a Credit calculation was
observed failing it.

## F-41 · Three by-id reads answered more than they should · BASELINE · P2 · FIXED

**Found by** an adversarial re-audit of the HTTP authorization surface, asking
of every handler that takes an id: does it verify ownership, and is a refusal
for somebody else's record distinguishable from a refusal for one that does not
exist?

Three routes fetched by id and decided afterwards, and each leaked in the gap:

- **`GET /v1/payouts/{payoutId}`** — 403 for a payout belonging to somebody
  else, 404 for one that does not exist. That difference is a membership
  oracle: anybody could enumerate which payout ids are real. Its own sibling,
  `POST /v1/payouts/{payoutId}/cancel`, has answered NOT_FOUND to both since
  F-29 and says why in a comment two files away. The same resource, two answers.
- **`GET /v1/internal-products/{productId}`** — no ownership check and no status
  filter, while `ListActive` beside it shows only what is buyable. A DRAFT
  product of another seller, with its price and its fee split, was readable by
  anyone holding `commerce:read`, which is every customer.
- **`GET /v1/native-assets/{assetId}`** — the same shape, and the response
  carries `moderation_state` and `moderation_notes`. A stranger could read
  somebody's unpublished asset AND the moderator's private commentary on it.

All three answer NOT_FOUND now. Owners and creators still read their own
unpublished work; a published product and a live asset are still public,
because a market nobody can look at is not a market.

**The tests needed the ports first.** The Domain A harness wired only the admin
plane, so a request to any customer route answered `UNSUPPORTED` — which reads
as a refusal and is not one. A test asserting "a stranger is refused" would have
passed against a deployment where the route did not exist. That is F-37's lesson
arriving twice in one session: the harness now wires the native-asset and payout
ports, and each of the three tests was observed failing with its fix removed.

## F-42 · The audit binding trusts a session variable any caller can set · BASELINE · P2 · FIXED

**Found by** an adversarial read of the migration surface.

Migration 00603 binds every state change of an audited entity to a transition
row written in the same transaction: an AFTER INSERT trigger on the transitions
table sets a transaction-local setting, and a DEFERRED constraint trigger on the
entity table raises `AU001` at COMMIT unless that setting matches the new state.
Seventeen tables depend on it.

Its header says: *"The application role cannot bypass this: it has no privilege
to drop triggers, and the flag can only be set by inserting an immutable
transition row."*

**The second clause is false.** `cp.transition.*` is a custom GUC, and
PostgreSQL lets any role set one. This repository proves the role can:
`internal/reconciliation/store.go:366` executes `SELECT set_config($1, '1',
true)` on a `cp.`-prefixed key as `cp_app`. So a caller that can execute SQL as
`cp_app` can satisfy `AU001` without writing a transition row, and change state
on any of the sixteen tables where `cp_app` holds unrestricted `UPDATE`.

`capability_gates` is the exception and shows the shape of a real fix: 00701
does `REVOKE UPDATE ON capability_gates FROM cp_app` and grants back only
`UPDATE (version)`, so a bare state update is impossible whatever the flag says.

**Why it is OPEN rather than fixed.** The obvious repair — have the deferred
trigger check that a transition row exists in the current transaction, instead
of trusting a setting — needs a way to say "in this transaction" that survives
savepoints. `xmin = pg_current_xact_id()::text::xid` was tried and rejected:
verified against this project's own PostgreSQL 16, a row inserted inside a
`SAVEPOINT` does NOT match the top-level xid, and the admin executor runs every
action inside one. A `created_at >= transaction_timestamp()` predicate was
rejected too — the test suites run on fake clocks, so a legitimately-written
transition row is routinely dated before the transaction that writes it.

What remains is the `capability_gates` treatment applied to the other sixteen
tables: revoke UPDATE on the state column and route state changes through
SECURITY DEFINER functions. That is a real piece of work and it is not being
started at the end of a session.

**Recorded now, with the exploit and the remedy, because the alternative is a
migration header that claims a guarantee the database does not make** — which
is exactly F-40, one file over.

### Update 2026-09-08: two more candidate fixes tried and rejected, with the evidence

Both are recorded so the next attempt does not repeat them.

**`pg_xact_status` does what `xmin = pg_current_xact_id()` could not.** Probed
against this project's own PostgreSQL 16, one row inserted at the top level of a
transaction and one inside a `SAVEPOINT`:

| row | `xmin` | `pg_current_xact_id()` | `xmin = pg_current_xact_id()::text::xid` | `pg_xact_status(xmin::text::xid8)` |
| --- | --- | --- | --- | --- |
| top level | 1911303 | 1911303 | **true** | in progress |
| in a savepoint | 1911304 | 1911303 | **false** | in progress |

So the earlier rejection was right about `xmin =` and the reason was the
subtransaction xid, exactly as recorded. `pg_xact_status` answers the question
correctly for both, and a row that is visible to us with an in-progress writer
can only be our own — no other transaction's uncommitted rows are visible at
any isolation level this system uses.

**It is still not safe to use.** `pg_xact_status` takes an `xid8`, and `xmin` is
a 32-bit `xid`. Casting it through text produces a full transaction id with no
epoch, which is correct until the counter wraps and then wrong for every row at
once. A control that works for years and then refuses every state change in the
system is worse than the forgery it prevents — the forgery needs the
application's database credential; the wraparound needs only time.

**Making the flag carry the transition row's id was also considered.** The
deferred trigger would look the row up rather than trusting a bare setting, so a
caller who sets the GUC without inserting a row fails. It does not close the
finding on its own: nothing stops a caller naming an OLD transition row with the
right `to_state`, replaying a state change that was once legitimate. Requiring
the row to be the entity's *latest* transition would close that — the latest
transition's `to_state` is by construction the current state, so a row naming a
different target cannot already be latest — but "latest" needs an ordering,
and the test suites run on fake clocks that make `occurred_at` non-monotonic.
That is the same rock the `created_at >= transaction_timestamp()` attempt hit.

**So the remedy is unchanged**: the `capability_gates` treatment — revoke
`UPDATE` on the state column, route state changes through SECURITY DEFINER
functions — applied to the other sixteen tables. It is privilege rather than
detection, which is why it works. It remains open because it is a change to the
mechanism seventeen state machines depend on, and the right way to do it is
deliberately, one table at a time with its own tests, rather than as a
sixteen-table migration written in one pass.

### Closed 2026-09-10 by 00741, and not by the remedy above

The remedy above is still the stronger answer and is still worth doing. It is
not what closed this.

**The problem was never the one the three rejected attempts were solving.** Each
tried to prove a transition row exists *in this transaction*, and each broke on
something real: subtransaction xids, `xid` wraparound, fake clocks. 00741 does
not detect anything. It makes the flag unforgeable, which is exactly the claim
00603's header made:

    tag = sha256(secret || '|' || pg_current_xact_id() || '|' || setting || '|' || value)

The secret lives in `cp_transition_key`, which no role but the owner may read;
the setters read it only because they are now SECURITY DEFINER; and EXECUTE on
every one of them is revoked from PUBLIC, which is what closes **F-128**, the
second forgery route this finding did not record.

`pg_current_xact_id()` is what the earlier attempts needed and did not use. The
rejection of `xmin = pg_current_xact_id()` was correct and was about a **row's**
xmin; `pg_current_xact_id()` itself returns the TOP-LEVEL id, identical in every
subtransaction. Probed again before the migration was written — top level, inside
a savepoint, after a write in a savepoint, and after `ROLLBACK TO SAVEPOINT` all
returned `3430236`. It also sidesteps the wraparound objection that killed
`pg_xact_status`, because nothing is stored or compared across time: the id is a
salt inside one transaction and never leaves it. And no ordering or timestamp is
involved, so the fake clocks that sank the other two attempts cannot reach it.

**What is left of this finding is the privilege work**, which is real and is now
tracked on its own terms rather than as this finding's blocker — and as of
00753 it is **done**.

**Zero of the seventeen bound tables grant `cp_app` blanket UPDATE.** Counted
from the schema, not from this list:

```
WITH bound AS (SELECT DISTINCT c.oid FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_proc p ON p.oid = t.tgfoid
 WHERE NOT t.tgisinternal AND p.proname LIKE '%require_transition%')
SELECT count(*) FILTER (WHERE has_table_privilege('cp_app', oid, 'UPDATE'))
     || ' of ' || count(*) FROM bound;
-> 0 of 17
```

Detection became privilege on every audited table. The transition row is the
state change; the application cannot write a state column at all.

Fourteen do not: `capability_gates` (00701), `withdrawals`, `assets`,
`instruments` and `payout_requests` (00733, F-109, the four whose columns are
money), `admin_actions`, and the eight done under this finding —
**`credit_fundings` (00743), `accounts` (00744), `wallets` (00745),
`native_markets` (00746), `native_assets` (00747), `orders` (00748),
`trade_intents` (00749), `agents` (00750), `reconciliation_records` (00751),
`deposits` (00752) and `kill_switches` (00753).**

### 00743 — the first of the ten, and what it cost

`credit_fundings` went first because its forgery costs money and because it is
the cheapest to do correctly: exactly one place in the repository changes a
funding's state, and it already writes the transition row immediately before.

The AFTER INSERT trigger on `credit_funding_transitions` now performs the state
change, as SECURITY DEFINER. `cp_app` keeps UPDATE on `lot_id` and
`provider_reference` and nothing else. **Detection became privilege:** the
refusal moved from `AUDIT_TRANSITION_REQUIRED` at COMMIT to `permission denied`
at the statement.

**The stamps moved with the state, and that is the better half.**
`reversible_at`, `settled_at`, `reversed_at` and `failure_reason` were set by a
switch on the destination that the caller had to remember. `reversible_at` is
what the settlement window is measured from, so a transition that moved the
state and forgot the stamp would leave money in REVERSIBLE with no clock running
on it. There is now no way to reach REVERSIBLE without `reversible_at`.

**The trigger's name is load-bearing.** PostgreSQL fires a statement's AFTER ROW
triggers in name order, and `credit_funding_transitions_writes_the_state` must
sort after both flag setters. Under a DEFERRED check the order is irrelevant;
under `SET CONSTRAINTS ALL IMMEDIATE` it is not, and something in this
repository does exactly that. Renaming it to something alphabetically earlier
would break the binding under an immediate check and pass every deferred test.

**Two fixtures were backdating `reversible_at` through the application pool**,
which is precisely the write that must be impossible. They were right about what
they needed and wrong about who should do it, so the credit suite now opens a
second pool on the migration role for exactly that. **That is the seventh
fixture this session found doing what a control forbids**, and every one was
found by closing the control rather than by reading the fixture.

**And one test was asserting the weaker refusal.** It required
`AUDIT_TRANSITION_REQUIRED`; the answer is now `permission denied`. A test still
expecting the old error would have been the thing telling us the stronger
control had not landed.

### 00744 — the second, and the strongest form the treatment takes

`accounts`, because freezing an account is the control that stops a compromised
or abusive one, and because the rule 00743 established picked it: **count the
state-change sites first, not the table's importance.** `accounts` has one.

All three columns the update wrote — `status`, `status_reason`, `frozen_at` —
were already carried by the transition row, so **nothing had to be derived, only
moved**. A status change and the reason for it can no longer disagree, because
they now come from the same row.

#### The row-lock rule, which will recur on all nine remaining tables

The plain REVOKE was written first, on the reasoning that
`cost_basis_method` is never updated anywhere so nothing needed granting back.
It broke four integration packages with:

```
accounts: lock: ERROR: permission denied for table accounts (SQLSTATE 42501)
```

**`SELECT ... FOR UPDATE` requires UPDATE privilege.** So do `FOR NO KEY
UPDATE`, `FOR SHARE` and `FOR KEY SHARE` — all four were probed and all four are
refused with no UPDATE grant at all. `accounts.Transition` locks the row before
checking the transition is legal, and that lock is what turns two concurrent
transitions into a clean CONFLICT rather than an audit row recording a
`from_status` that was already superseded.

A **column-level** grant is enough for the lock and still refuses the write,
which was probed rather than assumed:

```
GRANT UPDATE (cost_basis_method)  ->  SELECT ... FOR UPDATE   succeeds
                                  ->  UPDATE ... SET status   still refused
```

So the grant is there to permit a lock, not a write, and the migration says so
in both the header and beside the statement — because a reader finding it later
would otherwise conclude the application is meant to set that column.

**00743 got this right by accident.** Its two grant-back columns happened to
supply the same privilege, which is why `credit_fundings` passed its gate and
`accounts` did not. Budget a column grant per table for the remaining nine, and
say what it is for.

**The drill caught the change, which is what it is for.** Its probe issued an
`UPDATE accounts SET status` after writing the transition row, and that is now
`permission denied`. The probe asserts the status changed rather than changing
it — and it remains the one place in this repository that would catch a wrong
trigger NAME, because it forces the deferred check immediate, where the flag
setters must have run first.

**The counting rule has a limit worth knowing before trusting it.** `agents` has
one site and writes eight columns in it, including the promotion evidence. A
site count is a lower bound on the work, not an estimate of it.

### 00745, 00746, 00747 — three more, and what each one taught

A full read-only survey of the nine was done first and its recommended order
followed. The order was by cost, and cost turned out to be a good proxy for
risk: each of these three landed with one package to re-run and at most one
assertion to strengthen.

**`wallets` (00745).** The cheapest of the nine: the status update set exactly
one column, and it was `to_status` verbatim. The grant-back is the four
delegation columns, and `MarkDelegationVerified`'s own comment already drew the
line this migration enforces — "It is not a status change and needs no
transition row. An unverified status clears nothing: revocation is a status
transition to REVOKED." **The first table where no column had to be granted
purely for the row lock**, because the columns the application genuinely writes
already supplied the privilege.

**`native_markets` (00746).** The stamp must be coalesced or the fix reports a
different bug. `cp_native_market_curve_frozen` raises NM003 if `activated_at`
moves once set, so a trigger writing it unconditionally would make re-entering
ACTIVE look like a bonding-curve violation — the one error on that table that
means something else entirely. The Go code had the same guard for the same
reason, and moving it into the trigger is what makes it unforgettable.

Its grant is the one case so far that is **purely** for the lock, and the column
was chosen so the grant buys nothing: `virtual_credit_reserve` is already
refused by the frozen-curve trigger on any activated market.

**`native_assets` (00747).** Two call sites collapsed into one trigger, and a
comment got stronger than it was. `Activate` said "economics_locked_at is set in
the same statement as the status, so there is no instant in which the asset is
live and still editable." That was true of one statement written by one
function. It is now true of the **table**, because reaching ACTIVE any other way
is not possible and a second call site cannot forget.

This table's primary key is `asset_id`, not `id` — the binding already knew
(`cp_require_transition_edge('status','native_assets','asset_id')`) and a copy
of this migration for another table must not carry the join over blindly.

**One warning from the survey was checked and was wrong**, which is why it was
checked. It flagged two nativemarket tests as likely to start passing for the
wrong reason after the revoke. Both already ran on the migration role and both
assert the specific error string (`NATIVE_MARKET_CURVE_FROZEN`,
`native_markets_tradable_is_frozen`), so neither could. **A survey is a lead,
not a finding.**

### 00748 — the first that needed a decision, and it made two things stronger

`orders` was the first of the nine where a written column had nowhere to come
from. `transitionLocked` wrote `status`, `terminal_at` and **`rejection_code`**,
and `order_transitions` carried `reason` (free text) and `evidence_ref` —
neither of which is the code.

Two ways to close that: grant `rejection_code` back to the application, or put
it where it belongs. **It belongs on the transition** — it is why *this*
transition happened, and the emitted event already carries it next to the
transition id. Granting it back would have left the audit trail unable to answer
"why was this order rejected" from its own rows. So the column moved, with a
CHECK requiring it exactly when `to_status` is REJECTED: a Go-level validation
in one function became a property of the table.

**The compare-and-swap moved too, and got stronger.** The UPDATE carried
`WHERE id = $1 AND status = $5`. All four callers already hold a row lock —
checked — and nothing asserts its message, so it was belt-and-braces. It is now
in the trigger, where it applies to every writer and doubles as a second,
independent refusal of a transition row whose `from_status` does not describe
where the order actually is: the property 00731's edge binding asserts, enforced
when the row is written rather than at COMMIT.

**The CHECK is NOT VALID, deliberately.** Rows written before the migration have
no code because the column did not exist, and backfilling from
`orders.rejection_code` would be a guess — an order's current code came from
whichever transition set it last. NOT VALID enforces it on every future write
and skips the scan of what is already there. Making an audit trail complete
backwards is not something a migration can do truthfully.

**Two lists were read from the code rather than guessed**, and the first draft
had one of them wrong. `OrderStatus.Terminal()` is SETTLED, REJECTED, EXPIRED,
CANCELLED, FAILED_FINAL — **FILLED is not terminal**, because settlement follows
it, and the invented list in the first draft would have stamped `terminal_at` on
an order that was still moving.

### 00749 — the same decision, and where it deliberately differs

`trade_intents` had exactly the `orders` gap, so it got exactly the `orders`
answer rather than a second answer to reconcile later. **The CHECK is not a
copy**, and copying it would have been wrong: `orders` demands a code on
REJECTED and permits it nowhere else, while `trade_intents` demands one on
REJECTED *and* NO_VALID_PLAN and permits one on any of six terminal statuses.
Both rules were read out of `RequiresRejectionCode` and the validator rather
than out of the neighbouring migration. A single-equality CHECK would have
refused a legitimate FAILED transition that explained itself.

There is also no compare-and-swap here to preserve, because this UPDATE never
had one. Adding it would have been a new behaviour rather than a moved one.

### 00750 — where granting the obvious columns back would have undone 00739

`agents` is the table where the established pattern had to be refused.
`updateAgentStateSQL` wrote eight columns; three — `mode`, `envelope_id`,
`superseded_by_agent_id` — were on no transition row. Granting them back would
have left the application able to write LIVE and an envelope onto an existing
agent with no transition and no approval, **reaching by UPDATE exactly what
00739 refuses at INSERT.**

So they moved onto the transition. A promotion is the thing that grants
authority, so the authority it grants belongs on it — the 00748 argument, and
stronger here because what is granted is the right to move real money.

**Three drafts, and each was corrected by something in the schema rather than by
review.**

1. *Coalesce.* The first draft coalesced every column, so a transition that did
   not restate a value preserved it. `agents_check3` refused immediately: it ties
   stage to mode, and a demotion to VALIDATED that kept its mode is a row the
   table does not permit. Direct assignment is what the old UPDATE did, and a
   NULL means *cleared*.
2. *Deriving `failure_reason` from `reason`.* They coincide on the way into
   FAILED and diverge on the way out — a transition out of FAILED has a reason
   and must clear the failure. Carried explicitly as `to_failure_reason`.
3. *"The mode cannot be checked against the stage."* The first draft's header
   said so, because BACKTEST_ELIGIBLE permits BACKTEST or PAPER. That confused
   "not a function of" with "not constrained by". `agents_check3` has constrained
   it since 00501.

The third correction became the best part of the migration. **The three CHECKs
that constrain an agent's authority are now mirrored onto the transition**, so a
row that does not describe a legal destination is refused where it is *written*,
naming the rule it broke — instead of surfacing as a constraint violation on a
table the caller never touched. Three copies of a rule is normally what this
register complains about; these cannot drift unnoticed, because the agent's own
CHECKs still stand behind them. The mirror exists to move the error to where the
mistake was made.

**The test fallout was the largest of the eight** and every piece of it was the
same shape: fixtures that hand-build a transition row must now state the whole
destination, and probes that prove the *binding* must run as the migration role
because `cp_app` can no longer reach the column to be refused by it. Two forged
promotions are now refused earlier — at the row rather than at COMMIT — and the
assertions say which mechanism refused them, so a future change that silently
swapped one for the other would be visible.

**And the enum inventory did its job on the way past.**
`TestIntegration_NoEnumCheckAppearsUnnoticed` (F-74's answer) noticed both new
CHECKs and refused to pass until somebody decided what each was worth pairing
against. Two different decisions:

- `agent_lifecycle_transitions_destination_is_legal` is **compared with the
  constraint it mirrors** rather than with a Go list, which is a stronger
  pairing than the registry offers. 00750's header claimed the mirror and the
  original "cannot drift unnoticed, because the agent's own CHECKs still
  stand" — true of correctness and not of maintenance, since a mirror that
  permitted *less* would refuse legitimate promotions and nothing would say why.
  The comparison is what makes that sentence true rather than hopeful, and it
  was proven non-vacuous by narrowing the mirror to drop PAPER and watching
  BACKTEST_ELIGIBLE fail.
- `intent_transitions_rejection_code_check` is listed as unpaired, with the
  reason: the list it repeats is `intent.terminalStatuses`, which is
  **unexported**. Pairing it would mean exporting a package internal purely so a
  test could read it, trading real encapsulation for a check the constraint
  already enforces on every write.

### 00751 — the widest, and the trap in it

`reconciliation_records` wrote ten columns, six of them from a `ResolutionPatch`
carried on no transition row. All six moved onto the transition, for the reason
00748 and 00750 give and which applies most clearly here: **a record is resolved
BY a transition, so who resolved it, why, on what evidence, under whose approval
and with which compensating journal transaction are facts about that transition**
— not properties the record acquires beside it. A resolution written next to the
transition rather than on it is a record that cannot say from its own history who
closed it.

**The trap is that the two actors are different.** The transition row already had
`actor_type`/`actor_id` — who made the transition — and the record has
`resolved_by_actor_type`/`_id` — who resolved the discrepancy. Only the second is
refused to an AGENT. Mapping one onto the other, which is what "reuse the columns
already there" would have meant, **would have let an agent be recorded as the
resolver of a financial discrepancy** — exactly what
`reconciliation_records_resolved_by_actor_type_check` exists to prevent. The
survey flagged it; it was verified before writing, and the CHECK is mirrored onto
the transition so an illegal resolver is refused where the row is written.

**One ordering fact that would have been a silent hole.** The first draft wrote
the resolution in an UPDATE after `writeTransition`. The trigger fires on
INSERT, so the record would already have moved without it. The resolution has to
travel in the INSERT, and does.

**And the birth transition needed an explicit exemption.** `Create` writes the
record and then a `NONE -> OPEN` row; NONE is not a stored status, so the
compare-and-swap would have refused every record's own creation. The trigger
returns early on it, and 00724's `cp_require_transition_on_insert` — untouched —
is what still makes that birth row compulsory.

#### What the test fallout was, and it is larger than 00743's

Twenty sites in the migrations suite drove a bare `UPDATE accounts SET status`
to prove AU001 fires. cp_app can no longer issue one, so they were answered
`permission denied` and stopped measuring the binding. **They now run as the
migration role**, which retains table-level UPDATE and is still refused by the
deferred trigger — verified before rewiring them. That is not a weakening: the
owner is the only role left that CAN write the column, which makes it the only
one on which the binding is still what is being measured. That `cp_app` is
refused outright is asserted separately, because a single test passing for
either reason would not say which.

**And one subtest's premise stopped existing.** "Two rows do not license a jump
that skips the middle" wrote two transition rows and then a single UPDATE to the
final state. Since the trigger applies each row as it is inserted, writing both
rows walks both steps and there is no separate act of moving the entity left for
a caller to aim elsewhere. The jump is not refused, it is **inexpressible**. The
subtest now asserts that both steps were walked and that the trail describes
exactly them, in order — because asserting the old AU001 would have been
asserting that a weaker control was still in place. That work makes a bare state update
impossible rather than unforgeable, which is strictly stronger — but the claim
this finding was raised about is now true.

Counted from the schema rather than from memory:

```
SELECT c.relname, has_table_privilege('cp_app', c.oid, 'UPDATE')
  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_proc p ON p.oid = t.tgfoid
 WHERE NOT t.tgisinternal AND p.proname LIKE '%require_transition%';
```

**Proof.** Both forgeries reproduced as `cp_app` before the migration and
refused after it; a legitimate transition still commits, including one written
entirely inside a `SAVEPOINT` and one split across two; a flag captured from a
committed transaction and replayed in a later one for the same edge is refused.
Each test was proven non-vacuous by disabling the half of the fix it depends on.

**And the other half of 00603's claim now has a test.** Its header also says the
application role "has no privilege to drop triggers." That half was true and was
never the problem — but an unforgeable flag is worth nothing if the trigger that
reads it can be switched off, so the four ways to switch one off are now
asserted rather than assumed:

```
SET session_replication_role = replica            -> permission denied
ALTER TABLE accounts DISABLE TRIGGER <name>       -> must be owner
ALTER TABLE accounts DISABLE TRIGGER ALL          -> must be owner
DROP TRIGGER <name> ON accounts                   -> must be owner
```

The first is the one worth naming: `session_replication_role` disables every
trigger in the session at once, so it would bypass **every** trigger-based
control in this schema, not only this one. It is superuser-only and `cp_app` is
not one, which is a fact about PostgreSQL that this repository now depends on
out loud instead of silently.


### 00752 and 00753 — the two the survey called hardest

**`deposits` (00752) is where the revoke buys the least, and it was still worth
doing.** Its update is built dynamically from a patch of fifteen optional
columns — provider session ids and references, the amounts a provider reported,
chain signatures, fraud state, eligibility flags, the journal transactions a
settlement produced — and not one is a fact about a transition. All fifteen are
granted back, and the revoke buys exactly `status` and the thirteen timestamps
that record when each status was reached.

That is the smallest yield in the series and it is the point of the series:
**those fifteen columns are evidence, and `status` is authority.** They are now
separated by privilege rather than by convention. `created_at` is in
`timestampColumns` and deliberately absent from the trigger's CASE — a deposit
is INSERTed at CREATED, so a branch for it would be dead code that looked like
coverage.

**`kill_switches` (00753) is the one 00743 named as deliberately-not-next**, and
the reason it gave was real: its single write sets `active` with six other
columns under `version = version + 1 WHERE version = $11 RETURNING version`, so
moving one column breaks the concurrency check and the returned row at once.

That was a reason to move the version check too, not a reason to leave the
switch that stops the platform writable by the application. The transition row
carries `from_version`; the trigger performs the same compare-and-swap and the
same increment, and raises when the switch has moved underneath. It now applies
to every writer, and it is no longer possible to write the switch without
bumping the version — previously a convention of one statement.

`to_severity` is carried rather than derived, because severity comes from
`kind.Severity()` on activation and is left alone on release: deriving it from
`to_active` would be wrong on exactly the transition that matters, the release.

The birth row needed no exemption but did need a rule. A switch is INSERTed
already active by `insertActive` and `record` runs after it, so its transition
has nothing to move; a row with no `from_version` is not applied. The Go side
returns the switch by re-reading it, because the version and the timestamps are
the database's to produce now — the same correction 00747 made when it stopped
assigning stamps from a local clock.

## F-43 · MARKETPLACE was high-risk in Go and not in SQL · BASELINE · P1 · FIXED

**Found by** an audit of the documents against the code, which found
`docs/compliance-gates/PRODUCTION_GATES.md` asserting the opposite of
`gates.IsHighRisk` — and asserting it as a *correction* to an earlier table,
which is the form a reader trusts most.

The document was wrong: F-16 had already moved `MARKETPLACE` to high risk in Go,
because it gates the minting of the only withdrawable creator-earning
provenance, so leaving it low risk would let one approver switch that on.

**What was true is worse than what the document claimed.** The database did not
agree with Go. `cp_gate_is_high_risk` listed seventeen capabilities;
`gates.IsHighRisk` returns true for eighteen; the one they disagreed about was
`MARKETPLACE`. Migration 00701 describes the SQL check as *"the line that holds
when the Go check is bypassed"* — `GT003` refuses to approve a high-risk gate
without four evidence references and three distinct principals. For MARKETPLACE
that line was absent.

Nothing was exploitable through the API, because `gates.Admin` asks Go first.
What was missing is precisely the thing the second copy exists for: what happens
when the API is not the caller.

**Fix.** Migration 00716 adds `MARKETPLACE` to the SQL list, and
`TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk` drives
`cp_gate_is_high_risk` for every capability `AllCapabilities()` declares and
compares each answer with `IsHighRisk`. It was observed failing without the
migration, reporting `MARKETPLACE: Go says yes, SQL says no`. A second test
asserts every declared capability is nameable by the gate table's CHECK
constraint, because a membership test against a literal list treats an unknown
capability and a low-risk one identically.

The register already listed "the chart of accounts is duplicated between Go and
SQL" as a known, deliberately-unraised maintainability risk. This is that risk
arriving: a list duplicated in two languages diverged, and the half that
diverged was a security control.

## F-44 · Every native-market price was shown at the wrong scale · BASELINE · P1 · FIXED

**Found by** an audit of the browser interface against this project's own
honesty rules: a screen must never show a number the API did not return.

`NativeMarket.spot_price` and `NativeQuote.effective_price` are `Quantity` —
*"exact asset base units as an integer string"* — and both responses carry a
sibling `price_scale`. The web app declares `price_scale` in its own contract
and no component reads it. Both fields were rendered with `DecimalValue`, which
inserts thousands separators and nothing else.

`nativemarket.PriceScale` is **18**. So a price of a few thousandths of a Credit
was displayed as a sixteen-digit number of Credits, on the field labelled
"Price" and marked `emphasis`, and on the "Effective price" of a live quote —
the number a buyer commits against.

This is a worse failure than the class the honesty rules were written for. Those
forbid showing a zero for a figure the backend did not return. This showed a
figure the backend *did* return, in a unit the backend never used, with no
indication anything was wrong.

**Fix.** Both fields render with `Qty` at the scale the response carries. The
constant `CREDIT_DECIMALS = 6` now says in a comment that prices are not on that
scale and that the scale is a fact about the asset which the API states.

**Closed 2026-09-08.** See the update at the end of this entry.

**Was not fixed here, and recorded:** the same page scaled `circulating_supply`,
`asset_reserve`, the holder table and a quote's `expected_output` — all ASSET
units — by the same hardcoded Credit scale. They are accidentally right today
because the asset-creation screen also hardcodes six decimals and sends it, so
every asset made through the interface has six. An asset created through the API
with any other value renders wrong by a power of ten, and nothing would say so.

**Update, 2026-09-08 — the recorded remainder is closed.**

The asset quantities were left because they were accidentally right: the
creation screen also sends six decimals, so `CREDIT_DECIMALS` matched. They were
wrong for any creator who asked for anything else, by a factor of ten to the
difference, on exactly the supply and concentration figures a buyer judges a
market by.

The API could not have been used correctly, because it never stated the scale:
neither `NativeMarket` nor `NativeAsset` carried it, though
`CreateNativeAssetRequest` accepts it. So the fix is a fact, not a constant:
`asset_decimals` on `NativeMarket`, `NativeQuote` and `NativeFill`, read from
the asset registry in the same transaction the quote or fill was computed in.
`MarketView` gained the field and the port gained `NativeQuoteView` and
`NativeExecuteView` — the domain types stay free of a registry fact.

On the page, `circulating_supply`, `asset_reserve` and every holder's balance
now render at `asset_decimals`, and a BUY quote's `expected_output` does while a
SELL's stays in Credits, because that is which side the asset is on.
`CREDIT_DECIMALS` keeps its name and its comment now says the one thing it may
be used for.

**Evidence.** `TestIntegration_TheMarketResponseStatesItsAssetsScale` and
`TestIntegration_TheQuoteStatesTheScaleOfItsAssetSide` launch a market whose
asset has **nine** decimals — chosen so that a hardcoded six cannot pass —
and assert the response states nine, and that the asset scale and the price
scale are different numbers. The fixture checks the registry rather than what it
asked for.

## F-45 · The browser suite's own unit tests had been failing · NEW · P2 · FIXED

**Found by** running them.

`node --test src/lib/*.test.ts` is a source scan that enforces the money rules
on the app's own source, and it had been red since the purchase spec was written
— on that spec. Two lines converted a displayed Credit figure to a JavaScript
number, and the test's central assertion subtracted two of them and compared the
result to a third. IEEE-754 arithmetic on money, as the sole proof that money
moved, inside the test that exists to prove money moved.

The scan was right and the test I wrote was wrong. It parses exact base units
into `BigInt` now, and the `?? "0"` fallbacks are gone: a figure the page did not
show must fail the test, not become zero.

Fourth target found red on arrival in this session, after `make sast`, the
backup drill's Domain A coverage and `make lint`. The pattern is worth stating
once more: **a check that fails is a check nobody runs**, and every one of these
had been failing long enough that its failure had become the normal output.

## F-46 · The nine-page browser check asserted absences before the page loaded · BASELINE · P2 · FIXED

**Found by** the same audit, comparing the two page loops in
`critical-paths.spec.ts`.

F-32 established the rule: `toHaveCount(0)` passes the instant it is evaluated,
so asserting the absence of an error before the queries resolve proves nothing.
The fix was applied to the five internal-economy pages — `networkidle`, then no
`.loading`, then no "This response could not be trusted" — and the nine required
pages beside them kept the original shape: heading visible, one `h1`, no
`.malformed`, all evaluated immediately.

Home, Trade, Portfolio, Settings, Lab, Agents and Activity therefore passed with
every panel on them showing an error. All nine now settle first and assert the
same three things the other five do.

## F-47 · Two deliberate statements about who may read encrypted PII contradict each other · BASELINE · P2 · FIXED

**Found by** an adversarial read of the database as a security boundary,
enumerating what each role can actually do rather than what each migration says
it granted. **Then partly un-found by this project's own test suite, which is
the part worth recording.**

The observation is real. `cp_readonly` and `cp_ops` can SELECT `identity_pii`
(`email_encrypted`, `legal_name_encrypted`, `dob_encrypted`, `key_version`) and
`sessions` (`token_hash`, `roles`, `break_glass_until`, `ip`). Checked against a
live database: `has_table_privilege('cp_readonly','identity_pii','SELECT')` is
true.

They can because the role bootstrap says:

    ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public
        GRANT SELECT ON TABLES TO cp_readonly, cp_ops;

while migration 00010 grants those two roles SELECT on `users`, `accounts`,
`compliance_profiles`, the transitions and `security_events` — and deliberately
NOT on `identity_pii` or `sessions`. The migration's list has no effect while
that default exists.

**I revoked both, and the suite refused it.**
`test/integration/migrations/privileges_test.go` states the opposite contract in
a doc comment — *"cp_readonly and cp_ops can SELECT everything and write
nothing"* — and asserts it for every table in the schema. It failed. That is the
suite doing exactly its job, against a change made on an auditor's framing
without checking whether another part of the repository stated a different
contract.

And the revoke would have broken something real: the same file lists `sessions`
under `opsHousekeeping`, where *"cp_ops performs retention cleanup"*. A
`DELETE … WHERE expires_at < now()` needs SELECT on the columns it filters on,
so removing SELECT would have stopped session retention.

**So the finding is not "a privilege leak". It is that two deliberate statements
in this repository contradict each other, and the contradiction is invisible
because one of them silently wins.** Migration 00010's grant list was written by
somebody who meant those two tables to be withheld. The privileges test was
written by somebody who meant every table to be readable. Both are still in the
tree; only one has any effect.

**Why this stays OPEN rather than being resolved here.** Which statement is
right is a question about who may read encrypted personal data and session
material in this deployment. It has an operational constraint attached
(`cp_ops` needs `sessions`), it likely has a different answer for `cp_readonly`
than for `cp_ops`, and it is not a decision to take unilaterally inside a
migration at the end of a session. What is recorded here is the contradiction,
the evidence for both sides, and the constraint any resolution has to satisfy.

**What was left in place.** Nothing about privileges changed. The bootstrap
files carry a note saying the default is deliberate, and pointing at the
contradiction rather than resolving it.

### Update 2026-09-10: half of this IS resolvable, and it was the wrong half that stayed silent

The PII decision is not derivable and should not be forced — but **the
contradiction is not the decision**, and one half of it was a test asserting a
contract this repository does not have.

`privileges_test.go` said, flatly, *"cp_readonly and cp_ops can SELECT
everything and write nothing"*, and asserted it for every table. That is one
side of the disagreement stated as settled fact, in the file a reader would
check to learn what the contract IS.

It now names `identity_pii` and `sessions` as the exception, with the reason,
and asserts their current state as **"readable, and that is an open question"**
rather than as intent. Everything else is checked exactly as before. The
disagreement is now visible from either side of it instead of silently won by
the blanket default.

**And the decision's precondition is now stated**, which is the difference
between "needs a decision" and "the decision is premature". Whether SELECT on
`identity_pii` is an exposure depends on whether those columns hold ciphertext.
`SECURITY.md` records the application-layer encryption as **DESIGNED**, and
nothing in this repository writes those columns, so the table is empty in every
deployment that exists. The grant question is downstream of a control that has
not been built, and it cannot be answered before it — which is a reason, not an
absence of one.

`cp_ops` also needs SELECT on `sessions` to run retention, so the answer is not
even the same for the two roles. Any resolution has to satisfy that.

The finding stays OPEN. The decision is genuinely outstanding and marking it
otherwise would be the overstatement this register refuses. What changed is that
nothing in the tree now asserts one side of it as though it were decided.

### Update 2026-09-08: the table is empty, and there is now a fuse on it

Two facts that size the finding, both checkable rather than argued.

**Nothing reads or writes `identity_pii`.** `email_encrypted`,
`legal_name_encrypted` and `dob_encrypted` appear in exactly one place in the
repository outside documentation: the `CREATE TABLE` in migration 00010. No Go
file mentions any of them. So in every deployment that exists, the table the
contradiction is about holds no rows.

That is not a new discovery so much as a confirmation of what two other
documents already say honestly. `SECURITY.md` §167: *"Encryption of
`identity_pii` at the application layer: DESIGNED."* R-121-1 in the traceability
matrix, IN_PROGRESS: *"the column encryption is schema-shaped but has no Go
implementation, the identity_pii grant review has not been done, and there is no
test asserting the boundary."*

So an unresolved grant on an empty table costs nothing today, and the same grant
on a populated one is a standing exposure of every customer's name and date of
birth to two roles that were meant not to have it. The finding is not urgent; it
becomes urgent at an identifiable moment.

**That moment now fails a test.**
The fuse test (then named for the grant being unresolved; now `TestPII_OnlyTheEncryptingStoreWritesPersonalData`, asserting the resolved contract) scans `internal`,
`cmd` and `scripts` for a statement that writes `identity_pii`, and fails when
one appears, saying what has to be decided before it lands. Observed failing on
a planted writer, which it named by file. Its companion is the positive signal:
the same scan must find writers of `journal_transactions`, which certainly has
them, and the table must still be created by 00010 — so neither a broken
regex nor a rename can make the absence vacuous (F-32, F-46).

`sessions` is deliberately outside that check. `cp_ops` genuinely needs it for
retention cleanup, that need is recorded in `privileges_test.go`'s
`opsHousekeeping`, and it is written constantly. Its half of F-47 is a question
about column-level grants — `token_hash` and `break_glass_until` are not
`expires_at` — not about whether the table should have data.

**Still OPEN, and still for the same reason.** This is a fuse on a decision, not
the decision. Who may read encrypted personal data in a deployment is a policy
question with an operational constraint attached, and it is not one to settle
inside a migration.

### Update 2026-09-10, later: resolved, because the precondition now holds

The two earlier updates said exactly what would resolve this: the grant
question is downstream of an encryption that had not been built, and it could
not be answered before it. It is built now, and the answer follows.

**The encryption.** `internal/pii`: AES-256-GCM, a fresh 96-bit nonce per
seal, and additional authenticated data that binds each ciphertext to the
table, the column, the user id and the key version — so a ciphertext moved to
another row, relabelled as another column, or tagged with another version does
not open. Keys come from a versioned keyring in one SecretRef
(`CP_PII_KEYRING_REF`, `{"active": N, "keys": {"N": "<base64 32 bytes>"}}`):
writes seal under the active version, reads open under whichever version the
row names, and rotation is add-a-key, raise-active, deploy, `Reseal`, remove.
No KMS, because a key there is a fixed monthly cost and the launch tier may
not carry one; the row format does not change when the paid tier wraps the
same keyring in an envelope. The key never touches the database.

**The writer.** The login path keeps the verified e-mail address, encrypted,
beside the hash that finds it — filling an absence on every login the way
`email_hash` learned to, never following a change. `internal/pii/store.go` is
the only file that writes `identity_pii`, and `test/security` now asserts
that instead of asserting that nothing does. `legal_name` and `dob` have a
sealed home and no source yet: the identity-verification provider is B-06.

**The decision, derived rather than chosen.** With the columns ciphertext
under a key the database never holds, a grant with no use is a grant waiting
to become an exposure the day a key leaks. `cp_readonly` is analytics and
support reads, and PART 121 says personal data does not travel there.
`cp_ops` runs retention, which never touches `identity_pii`, and its one use
of `sessions` is `DELETE … WHERE expires_at < now() - <window>`, which needs
DELETE plus SELECT on that one column. So migration 00754 does what 00010
meant: both tables are revoked from both roles, and `cp_ops` is granted
`SELECT (expires_at)` on `sessions` and nothing else there. The bootstrap's
blanket default stays, because it is what makes every *new* table readable
without a migration remembering to say so; the withheld tables are a named
list in `privileges_test.go`, asserted in the strong direction like
`cp_transition_key`, so the list cannot grow silently.

**What the `sessions` half turned up.** Checking what `cp_ops` actually did
with the table before deciding what it may read: nothing. `PurgeExpired`
had no caller on any tier. That is F-133, fixed in the same change.

**Evidence.** TEST_UNIT: `internal/pii` — a keyring refused whole for eight
kinds of malformation without printing key material; seal/open round trip;
distinct ciphertexts for equal plaintexts; a ciphertext moved to another user,
another column or another version refuses to open, as does one altered by a
bit or too short to be one; rotation opens old rows and seals new ones under
the active version and a ring that dropped a key cannot open rows under it; a
nil store refuses. `cmd/api` — `identity.Deps` is built with the store; no
keyring is a WARN naming the variable and the consequence; an unusable keyring
is an ERROR and no store; a usable one names its versions and not its bytes.
`internal/config` — the variable, the golden `.env.example`, the STAGING/PROD
refusal (`RulePIIKeyring`). `test/security` — every writer of `identity_pii`
is the encrypting store, and the store is a writer. TEST_INTEGRATION:
`internal/pii` — the column holds ciphertext and NULL for an absent value; a
partial update keeps what it did not touch; rotation re-seals the row and a
ring holding only the new key then reads it; the wrong key of the right
version is an error, not an empty record. `internal/identity` — a login
stores `customer-a@dev.invalid` sealed, the column does not contain it, the
lookup hash agrees with it, and a second login does not rewrite the row.
`test/integration/migrations` — neither role can SELECT `identity_pii` or
`sessions`; `cp_ops` can SELECT `sessions.expires_at` and not `token_hash`,
`roles`, `break_glass_until`, `ip`, `user_agent` or `user_id`; `cp_ops` keeps
DELETE; `cp_app` still cannot DELETE. And the purge runs as `cp_ops` under
that grant (F-133). ADR-0021 records the decision; D-050 the reasoning.

## F-48 · Five SECURITY DEFINER functions did not pin `pg_temp` · BASELINE · P2 · FIXED

**Found by** the same audit, checking every SECURITY DEFINER body's
`search_path`.

Migration 00701 names this hazard precisely and pins `pg_catalog, public,
pg_temp` for `cp_gate_transition`. Five older functions were written with
`SET search_path = public` and never updated: `ledger_apply_entry`,
`cp_credit_lot_open`, `cp_credit_lot_apply_event`, `cp_native_market_open` and
`cp_native_market_apply_fill` — the triggers that enforce LG001, the credit-lot
invariants and the market's constant product.

`TEMP` on a database is granted to `PUBLIC` by default and is revoked nowhere in
this tree, so any caller may create temporary tables; with `pg_temp` unpinned it
is searched first. A temp relation shadowing `ledger_accounts` would be what
`ledger_apply_entry` reads, as the definer.

Not demonstrated as exploitable — the mechanism is confirmed, the exploit was
not built. Recorded at P2 and fixed anyway, because `ALTER FUNCTION … SET
search_path` changes nothing but the setting and the argument for pinning it was
already written down in this repository, one migration over.

## F-49 · An account chose its own exemption from the negative-balance guard · BASELINE · P2 · FIXED

**Found by** the same audit, asking what LG001 actually reads.

LG001 is the guard that stops a balance going below zero, and it asks the
account row:

    IF newbal < 0 AND NOT acct.allow_negative THEN RAISE ... LG001

`ledger_accounts.allow_negative` is `NOT NULL DEFAULT false` and `cp_app` holds
INSERT on the table. So a caller could create an account that says it may go
negative, and post it below zero for ever after. Migration 00604 restricts
UPDATE to `(status)` and explains that the app *"must never change an account's
negative-balance policy"* — which is true, and it was setting the policy at
creation instead.

The application itself is not the risk here: `internal/ledger` sets the column
from its chart of accounts and `checkDefinition` refuses an account whose stored
values disagree with its code. What was missing is the check where the guard
reads it.

**Fix.** Migration 00717 adds `cp_ledger_code_allows_negative` and
`cp_ledger_code_normal_side` and a CHECK constraint binding both columns to the
code. Existing rows are validated rather than grandfathered: a forged account
already in the table would be a finding, not an inconvenience.

**And the copy is compared, not trusted.** These functions are a second copy of
a Go table — the exact maintainability risk this register lists under "findings
deliberately NOT raised", and the one that arrived as F-43 in a security
control. `TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts` drives both
functions for every code the Go registry declares, with negative controls for
"every code allows negative" and "none does", either of which would make the
comparison agree perfectly and prove nothing.

## F-50 · An asset could be stored with no value domain at all · BASELINE · P2 · FIXED

**Found by** the parity test written for F-49's sibling claim — a comment in
`internal/assets` promising that "a test asserts the two agree for every kind",
where no such test existed. It failed on its first run.

Migration 00710 introduces `assets_kind_domain_agree` with the sentence *"a kind
and a domain that disagree is a data-entry error that would silently reclassify
value"*. It does not catch the commonest form of that error.

    CHECK ( (kind = 'CREDIT'       AND value_domain = 'INTERNAL_CREDIT')
         OR (kind = 'NATIVE_ASSET' AND value_domain = 'INTERNAL_NATIVE_ASSET')
         OR (kind = 'FIAT'         AND value_domain IS NULL)
         OR (kind IN (...)         AND value_domain IN (...)) )

For a CREDIT row with `value_domain` NULL the first disjunct is `true AND NULL`
= NULL and the rest are `false`, so the whole expression is NULL — **and a CHECK
constraint fails only on FALSE.** NULL passes.

Demonstrated as `cp_app` against a migrated database: a CREDIT asset with no
value domain inserts cleanly. `internal/assets` refuses the same shape, so Go
and SQL disagreed on every kind except FIAT.

**How far it goes.** Not far, and worth saying so rather than overstating it:
`cp_ledger_account_domain` raises VD001 for an asset with no domain, so a
malformed asset cannot hold a ledger account and cannot be posted. What it can
do is exist, be listed, and be referenced by anything that never opens an
account.

**Fix.** `assets_value_domain_presence`, stated as an equivalence —
`(kind = 'FIAT') = (value_domain IS NULL)` — so both sides are non-null booleans
and the constraint can never evaluate to NULL. The parity test drives both
constraints, coalescing each the way a CHECK behaves, and is exhaustive over
every (kind, domain) pair including the absent domain.

**The general lesson, which is not about this constraint.** Any CHECK whose
expression can be NULL is a CHECK that accepts. This one had been read by
several people, cited in a doc comment as the thing Go mirrors, and was wrong in
a way that reading it in Go-shaped terms does not reveal.

## F-51 · Three operations documents described a system that does not exist · BASELINE · P2 · FIXED

**Found by** an audit of the documents against the code — the same question
`test/docs` asks mechanically for cited test names, asked by hand of every other
kind of claim.

- **`docs/architecture/EXECUTION.md`**: *"The signing package is importable only
  by `cmd/execution-worker` (depguard rule)"*. Both halves false. The only
  depguard rule naming `internal/signing` is `agent-authority`, which denies it
  to the agent trees — a different restriction, in the opposite direction. And
  `cmd/execution-worker` does not import it: `internal/signing` has no non-test
  importer at all.
- **`docs/operations/DEPLOYMENT.md`**: *"`gates.Bootstrap` persists a `DISABLED`
  row for every capability at first start"*. It has no caller in `cmd/`; only
  `scripts/gateceremony` invokes it. Migration 00701 and the readiness report
  both say a fresh deployment has no gate rows; the deployment runbook said the
  opposite. Still fail-closed — an absent row is INACTIVE — but an operator
  looking for those rows after a deploy will not find them.
- **`docs/operations/RECONCILIATION.md`**: *"The reconciliation engine is not yet
  implemented (Stage 7)"*. It is: the engine exists, a worker drives it on a
  ticker, and the admin read endpoints are wired. Stale in the PESSIMISTIC
  direction, which misleads just as effectively — a reader would not go looking
  for a subsystem that is running and raising records.

All three are corrected in place and labelled as corrections rather than
silently edited, for the reason F-40 gives: a document that quietly changes its
mind teaches nobody anything.

**What this says about the mechanical check.** `test/docs/references_test.go`
verifies that every TEST NAME cited in five documents exists — one narrow class
of claim, in five of the repository's fifty-odd documents. Nothing checks a
claim about a control, a wiring, or a count. These three were found by reading.

## F-52 · An account's new risk could be blocked with no wired way to unblock it · BASELINE · P1 · FIXED

**Found by** a worker-reachability sweep asking which exported functions have no
caller a deployment can run.

The two halves of reconciliation were wired asymmetrically:

- **Blocking was live.** `cmd/reconciliation-worker` runs `RunPeriodic` and
  `SweepEscalations` on tickers and raises records. `ReconciliationBlockReader`
  is wired into `cmd/api` and reads `reconciliation_records WHERE
  blocks_new_risk AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')`,
  which removes the account's capacity to take new risk.
- **Resolution was not.** `cmd/api` set `Reconcile: nil`, so both admin
  endpoints answered 422 UNSUPPORTED and `Engine.ResolveManual` had no caller
  outside `internal/reconciliation`'s own tests.

A deployment could freeze somebody's account by automated check and had no
button to unfreeze it. The only recourse was a hand-written UPDATE against the
database — the thing the admin plane exists to replace.

**This is F-29 one level up.** That finding was *"a user could reserve their
Credits in a payout request and had no way to release them"*. This is capacity
reserved with no path to release, found by the same question.

**Fix.** `cmd/api` constructs a resolution-shaped `reconciliation.Engine` and
`httpapi.NewReconciliationPort` serves both routes.
`TestIntegration_AnOperatorCanClearAReconciliationBlock` raises a blocking
record, asserts the account IS blocked, resolves it over HTTP as an operator,
and asserts the block is gone and the record names who cleared it and why. It
was observed failing with the port unwired.

**Three things the domain refused, which the fix respects rather than works
around.**

1. **`INVESTIGATING` comes before `RESOLVED_MANUAL`,** and the state machine
   says why: somebody looked at the evidence before resolving. A worker-raised
   record is `MISMATCH`, or `ESCALATED` after the sweep — so a resolve-only
   endpoint could not clear a single record the worker actually produces, and
   this would have been fixed in name only. The adapter moves those two into
   `INVESTIGATING` first, carrying the operator's own reason.
2. **A record may change status at most once per transaction.** The repository
   refuses more than one, which is what keeps each transition bound to exactly
   one audit row. So the investigate commits first and the resolution follows in
   its own transaction. A failure between them leaves the record
   `INVESTIGATING`, which still blocks: nothing is lost and repeating the call
   finishes the job, because the first step is skipped for a record already
   there.
3. **An `OPEN` record is not operator-resolvable.** OPEN means the engine has
   not decided there is a difference at all, and its only transitions are the
   engine's own. The adapter does not force a path there;
   `TestIntegration_AnOpenRecordIsNotOperatorResolvable` asserts the refusal
   rather than working around it.

**What is deliberately NOT wired.** A compensating posting. It is the only way a
resolution changes financial state (PART 195), and building one from an API
request means choosing the posting's kind, its idempotency key and its
reference, and mapping each entry's account code onto an owner — decisions with
financial consequences that deserve their own design and review. The adapter
refuses one **by name**, and the engine it is given carries no ledger, so the
two agree even if somebody later changes only one of them.
`TestIntegration_AReconciliationResolutionRefusesToPost` asserts the refusal and
that the record is left exactly as it was.

## F-53 · A market could be tradable while its economics were still editable · BASELINE · P2 · FIXED

**Found by** an adversarial read of the migration surface, comparing which
column each guard actually reads.

Two freezes exist for PART XIII's *"do not allow a creator to silently change
economics after buyers enter"*:

    cp_native_market_curve_frozen     keys on native_markets.activated_at
    cp_native_asset_economics_frozen  keys on native_assets.economics_locked_at

and both `RETURN NEW` unconditionally when their column is NULL. What decides
whether a fill is accepted is a **different column**:
`cp_native_market_apply_fill` gates on `m.status IN ('ACTIVE','CLOSE_ONLY')`.

Nothing tied them together. A row with `status = 'ACTIVE'` and `activated_at`
NULL would trade normally while `virtual_credit_reserve`,
`initial_asset_reserve`, `platform_fee_bps` and `creator_fee_bps` stayed
mutable — so the constant product `k` and the NM005 supply ceiling could be
rewritten between fills, which is precisely the harm the freeze exists to
prevent.

The application never produces that row: `nativemarket.SetStatus` sets
`activated_at` in the same UPDATE as the status and `nativeasset.Activate` does
the same for its lock, each with a comment saying why. This is F-49's shape — an
invariant that holds because the service is careful, in a database that is
supposed to hold it whatever the caller is.

**Fix.** Two CHECK constraints in migration 00718, tying tradability to the
freeze on both tables. Written as `status NOT IN (...) OR <column> IS NOT NULL`
so the expression can never evaluate to NULL, which is F-50's lesson applied one
migration later.

**The test was written wrong twice, and both mistakes are the same mistake.**

1. It first tried to build the forbidden state by clearing `activated_at` from
   the fixture's live market. That is refused by `cp_native_market_curve_frozen`
   itself (NM003), a BEFORE trigger that runs ahead of the constraint — so the
   test would have passed on the trigger and proved nothing about the CHECK.
   The state has to be built from a market that has never been activated, where
   the freeze trigger returns early and the constraint is the only thing there.
2. Its positive control then tried `UPDATE … SET status = 'ACTIVE', activated_at
   = now()` by hand, and the AU001 audit binding refused it: a status change
   needs a transition row in the same transaction. The control now goes through
   the service, which is what a launch actually is.

Both are the same lesson as the finding itself: **a test that passes because a
different guard fired is a test that has not seen the guard it names.**

## F-54 · Every derivable count in the readiness documents had gone stale · BASELINE · P3 · FIXED

**Claim under test.** The numbers a reviewer reads in the readiness documents
describe the system as it is now.

**What was found.** Five of them did not.

| Document | Said | Actually |
| --- | --- | --- |
| `MASTER_BUILD_STATE.md` | capabilities: 20, of which **9** are high-risk | **18** |
| `MASTER_BUILD_STATE.md` | **59** permissions | **60** |
| `PRODUCTION_READINESS_REPORT.md` | browser suite: **69** tests | **70** |
| `PRODUCTION_READINESS_REPORT.md` | a **66**-test suite is not a person | same suite, same page |
| `BACKUP_RESTORE.md` | latest drill: **89** tables, version **604**, 7.8 s | **118** tables, version **718** |

Each was written accurately. The high-risk figure was right until F-16 moved
MARKETPLACE and the rest of the internal economy behind gates and did not
recount. The readiness report contradicted itself twice on one page because
three sentences were written on three different days. The restore drill's line
was the worst of them: a hundred and eleven migrations old, describing a run
that had never seen a Domain A table, the ledger's chart-parity CHECK, or the
frozen-economics constraints — which makes it not a weaker claim than it
appears but a claim about a different database.

**Severity.** P3. Nothing here is a control that fails; it is the evidence a
reviewer would use to decide whether the controls are there. A readiness
document whose numbers drift is exactly F-14 and F-18 again — a document
asserting something about the system by recalling it rather than checking —
in its slowest and least visible form. F-18 was caught because it was wrong in
four places at once. These were wrong in one place each, for months.

**Fix.** `TestDocs_CountsMatchTheCode` derives each number from the thing it
describes and asserts it against the sentence that carries it: capabilities and
high-risk capabilities from `gates.AllCapabilities`/`gates.IsHighRisk`,
permissions from `security.AllPermissions`, and the drill's schema version from
the newest migration in the tree. It sits beside the citation check this package
already ran, which is what caught the first attempt at this fix — the edit
cited `TestDocs_CountsMatchTheCode` in the build state before writing it, and
`TestDocs_EveryTestTheyNameExists` refused it. That is F-25's mistake, made
again, by the thing correcting F-25's class of mistake, and stopped by the
control built for it.

The migration-version claim deliberately fails on staleness rather than on
typos: it compares the cited version against the tree, so landing a migration
without re-running `make restore-drill` breaks the build. It broke immediately
— the drill was three migrations behind at the time of writing. The fix was
to re-run it, not to edit the number: version 718 on both sides, 118 tables, row
counts identical, zero balance drift, journal hashes equal, 10.6 s. That run is
also the first to carry 00716–00718 through a real dump and restore.

The browser count is left out on purpose. Deriving it means expanding the
Playwright loops in Go, and a derivation that is wrong while asserting against a
number that is right would be worse than the staleness it replaced.

**Evidence.** `go test ./test/docs/` — observed failing on the stale drill
version before the drill was re-run, then passing.
`TestDocs_CountsCheckIsNotVacuous` is the positive signal: it fails if the
derivations collapse to zero or to each other, so agreement means something.

## F-55 · Every runbook described a system that predates the build · BASELINE · P1 · FIXED

**Claim under test.** An operator following a runbook during an incident is
told what this system can actually do.

**What was found.** 140 `PENDING` markers across 21 runbooks and the runbook
index. Every one that named a Go package, a binary or a make target named
something that is on disk:

| The marker said | On disk |
| --- | --- |
| `cmd/api` — "no serving binary, no handler wiring" | serving the whole API, admin routes permission-gated and step-up-protected |
| `internal/reconciliation`, `cmd/reconciliation-worker` | both built; the worker runs on a ticker |
| `cmd/audit-worker` | built; `make verify-audit` drives it |
| `internal/agent`, `internal/model`, `internal/strategy`, `internal/reality`, `cmd/agent-worker`, `cmd/market-ingest-worker` | all six built and integration-tested |
| the executor/recoverer | `cmd/execution-worker` |
| `internal/provider/privy`, `signing.Service` | both exist |
| production broker/Temporal client wiring "in the `cmd/*` composition roots" | `cmd/relay-worker`, `cmd/workflow-worker`, `cmd/market-ingest-worker` |

Fifteen HTTP routes the runbooks tell an operator to call were checked against
`openapi/openapi.yaml`. **All fifteen exist**, and twelve of them carried a
marker saying they did not. The index's closing paragraph read: "Until
`cmd/api` lands, the only way to activate a kill switch is to call
`killswitch.Controller.Activate` from a Go program run by an operator against
the database; there is no CLI."

**Severity.** P1, and the reasoning is worth stating because no production code
is wrong here. A runbook is read once, under time pressure, by somebody who was
not there when it was written. `database-corruption.md` step 5 told a responder
that the reconciliation engine did not exist — during a corruption
incident, that is the responder not running the one tool that says what the
restored copy is missing. `global-kill-and-reenable.md` told them to write a Go
program to stop the platform. The cost of a stale runbook is paid in the
minutes an incident is made of.

**The sharpest instance** was not a stale marker but a positively wrong query.
`funding-provider-compromise.md` told an incident responder to detect webhook
forgery by looking for `provider_events.signature_verified = false` rows, and
said the `webhook_signature_failed` emitter was PENDING. Both halves are wrong
in the dangerous direction: `internal/webhook` rejects an event whose signature
does not verify *before* writing any `provider_events` row, so that column is
`true` on every row that exists and the query cannot return one however bad the
incident is — while the emitter it says is missing writes a HIGH-severity
`security_events` row on every failure. A detection query that cannot fire is
worse than no query, because it answers.

**Fix.** Every false marker corrected against the tree. The vocabulary is now
two words that mean different things: `PENDING` for what is missing from this
repository, `BLOCKED_EXTERNAL` for what needs a cloud account, a credential or a
funded wallet. Filing "we have no AWS account" under the same word as "this
package does not exist" is what made the second invisible.

Both claims are now checked, in `test/docs`:

- `TestDocs_NothingMarkedPendingAlreadyExists` fails when a marker names a
  package, command or make target that is on disk. This forces a marker to name
  what is actually absent: "PENDING: `cmd/api` route" is the shape that rotted,
  because `cmd/api` existed and the route was the pending part, so nothing
  distinguished the sentence from a true one once the route landed.
- `TestDocs_EveryRunbookRouteIsServed` holds every `GET`/`POST` path in these
  documents against the spec. A stale marker wastes a responder's time; a route
  that does not exist wastes it and leaves them with no containment action.

**Evidence.** Both observed failing on planted defects — a PENDING naming
`internal/ledger` and `make unit`, and a call to
`POST /admin/definitely-not-a-route` — and passing after. Three markers
survive, and all three are true: `internal/backtest` and `internal/performance`
do not exist, and there is no `make reconcile-full` target.

**Related.** This is F-51 at the scale of the whole runbook set. F-51 fixed
three documents by hand; the rest went on rotting, which is the argument for
the mechanical check rather than a second careful read.

## F-56 · Provider webhook evidence was mutable by the application role · BASELINE · P2 · FIXED

**Claim under test.** `provider_events` is the evidence record. Migration 00107
says so in its header comment, and its `Down` section says "provider evidence is
never dropped".

**What was found.** Neither statement was enforced. The table had no trigger of
any kind — alone among the evidence tables in this schema, where
`audit_events`, `raw_archive_objects` and `payout_provider_events` all carry
`forbid_mutation` — and `cp_app` held table-wide `UPDATE`. So the columns
the evidence consists of were writable by anything holding the application role:
`payload_hash` (what the provider actually sent), `signature_verified` (that we
checked it was them), `raw_ref` (where the untouched body is archived),
`provider_event_id`, `received_at`.

Nothing does write them. `internal/webhook` touches four lifecycle columns in
two places and is careful about it. That is exactly F-49's shape: an invariant
that holds because the one caller is well behaved, in a table whose entire
purpose is to be arguable against a provider who disputes what they sent.
Evidence the disputing party's own software could rewrite is not evidence.

**Fix.** Migration 00719 states it twice, following the house pattern for a
table that has lifecycle columns and so cannot simply be `forbid_mutation`'d
(`fills_guard`, `execution_plans_guard`, `notifications_guard`):

1. the `UPDATE` grant is narrowed to the five columns that legitimately move,
   so the privilege system refuses the rest before a trigger is reached;
2. `provider_events_guard` refuses them again with SQLSTATE `LG003`, which also
   covers the owning role and any future grant somebody widens without reading
   the migration.

`canonical_event_id` is in the mutable set deliberately — nothing writes it
today, but it exists to be filled in when an event becomes a canonical one,
which is the lifecycle assignment the guard permits. `signature_verified` and
`verification_error` are frozen at their inserted values, and the reason is
recorded in the migration: a `provider_events` row exists only for an event that
verified, so the column records a fact rather than holding a field to revise.

**Evidence.** `TestIntegration_ProviderEvidenceCannotBeRewritten` drives six
evidence columns twice — once as the owner, which every grant is irrelevant
to, so a refusal is the trigger and nothing else; once as `cp_app`, which is the
role a defect or an intruder in this codebase would hold. Observed failing with
the trigger dropped and the table-wide grant restored: **every one of the twelve
mutations succeeded**, including `cp_app` rewriting `payload_hash` and setting
`signature_verified = false` on a verified event.
`TestIntegration_TheLifecycleColumnsStillMove` is the positive control — a
guard that refused everything would pass all twelve assertions and break the
pipeline that writes the evidence, which would look like an empty table rather
than an error.

## F-57 · Five subsystems' tests skipped silently in CI, and fifteen VERIFIED rows cited them · BASELINE · P1 · FIXED

**Claim under test.** The `integration` job runs the integration tests.

**What was found.** It enumerated all of them and ran most of them. Five
packages need something a Go test binary cannot start, and the job started
Postgres alone:

| Package | Needs | Guard |
| --- | --- | --- |
| `internal/reality` | ClickHouse | `CP_TEST_CLICKHOUSE_ADDR` |
| `internal/reality/redpandabus` | Redpanda | `CP_TEST_REDPANDA_BROKERS` |
| `internal/archive` | MinIO | `CP_TEST_ARCHIVE_ENDPOINT` |
| `internal/workflows` | Temporal | a health check |
| `internal/ratelimit` | Redis | `CP_TEST_REDIS_URL` |

Each skipped, and a skip is a pass. **Fifteen rows of
`REQUIREMENTS_TRACEABILITY.md` marked VERIFIED cite those exact tests as their
evidence**, including R-S11-1, whose stage exit is entirely met by
`TestIntegration_ClickHouse_LookAheadLeakage` and
`TestProp_ClickHouse_SnapshotNeverReturnsFutureKnowledge`.

`internal/ratelimit` was worse than skipped: its Redis test sat in an untagged
file, so `make unit` invoked it without Redis and the integration job's
enumeration — which selects packages by the build tag — never reached
the package at all. It could not run anywhere.

And `make property`, cited as the operational evidence for R-150-1, runs
`go test -run 'Prop|Property' ./internal/...` with no build tag. It cannot
compile the database-backed property files, so it silently ran none of the ten
packages that hold them — capital conservation, reservation
oversubscription, reconciliation convergence, balance convergence among them.

**The repository already knew.** The chaos job carries this comment: "Without
these two, broker_stall_test and archive_refused_test t.Skip() silently and the
job finishes in under half a second while reporting success." That warning was
written for one job and never applied to the other, and
`PRODUCTION_READINESS_REPORT.md` action item 13 asked for exactly this
— "Export ... in the chaos **and integration** jobs" — and only the
chaos half was done.

**Severity.** P1. This is the recurring class in its quietest form: *a check
that can only skip is a check nobody runs*. It is worse than a check that can
only fail, because a failing check gets noticed.

**Fix.**

- `internal/testkit/deps` decides whether a missing dependency is a skip or a
  failure. A job that promises the stack sets `CP_TEST_REQUIRE_EXTERNAL_DEPS`;
  in that job a missing dependency is a failure. Anywhere else it stays a skip,
  which is what makes this safe to apply everywhere — a fast unit job that
  never starts Redis is not lying when it skips a Redis test, because it never
  claimed to have Redis. Only a job that claims it can be caught out.
- The `integration` job now runs `make infra-up` rather than starting Postgres
  alone, exports the five addresses, and sets the promise. A following step
  re-runs the five packages verbosely and fails on any dependency skip.
- The Redis test moved to `internal/ratelimit/redis_integration_test.go` behind
  the build tag, which is what puts it in the enumeration.
- `scripts/inttest` gained `-run`, filtering packages by whether they declare a
  matching test so a database is not provisioned for a package with nothing to
  run, and `make property` now invokes it. A target cited as proof has to
  execute the thing.

**Evidence.** `deps.Need` observed in both directions: with
`CP_TEST_REQUIRE_EXTERNAL_DEPS=1` and no endpoint,
`TestIntegration_S3_PutGetHeadListRoundTrip` **fails** naming the variable;
with the endpoint, it passes. All five packages then run green against the live
stack — `internal/reality` 36 passes and zero skips, with the two
look-ahead-leakage tests among them.

## F-58 · Documented SQLSTATEs that nothing raised, and raised ones nothing handled · BASELINE · P2 · FIXED

**Claim under test.** Each migration's `-- Custom SQLSTATEs:` header names the
codes its triggers raise, and the application classifies them.

**What was found.** Every custom code in the tree was traced from its
documentation to its raise site to its Go handler. Two gaps, in opposite
directions.

**Documented, never raised (4).** Each labels an invariant that *is* enforced,
by a different mechanism carrying a different code — so the property holds
and the documentation is fiction:

| Code | Says | Actually enforced by | Actual SQLSTATE |
| --- | --- | --- | --- |
| `CR002` | immutable row | `forbid_mutation()` on the three credit tables | `P0001` |
| `CR005` | more than one Credit asset | `assets_single_credit_asset` unique index | `23505` |
| `PO002` | illegal state transition | the `state` CHECK plus the `AU001` transition binding | `23514` |
| `PO004` | amount mismatch | `settled <= reserved <= requested` CHECKs | `23514` |

The cost is not a missing control; it is that the application cannot tell these
apart from any other unique or check violation, so a second Credit asset and a
duplicate idempotency key surface identically.

`PO003` is a third kind of drift: documented as "provenance mismatch", and all
three of its raise sites emit `PAYOUT_ALLOCATION_IMMUTABLE`. The Go handler
agrees with the SQL and contradicts the header.

**Raised, never handled (13).** `VD001`–`VD005` — the entire
value-domain family, thirteen raise sites — appear in no Go file at all,
test or otherwise: `internal/valuedomain` has no `SQLState` or `pgconn`
reference, so every one of them reaches a caller as an unclassified
`INTERNAL`. `BT001` likewise. `GT001`, `GT002`, `GT004`, `GT005` and `AU002`
are asserted only inside integration tests, never mapped in production code.

**The sharpest of these is `GT003`.** It has five raise sites, and migration
00716 describes it as "the line that holds when the Go check is bypassed" —
the database backstop for capability-gate evidence and windows. **No Go code
asserts on it, in production or in a test.** A backstop with no evidence that it
fires is the F-26 shape: a control believed because it was written.

**Fixed so far.**

- **GT003 now has evidence.** `TestIntegration_DatabaseRefusesActivationWithoutEvidence`
  drives all four evidence references and a whitespace-only one, and
  `TestIntegration_DatabaseRefusesActivationOutsideTheWindow` drives the expired
  and revoked cases. Seven assertions of `GT003` where there were none.

  Writing it turned up something worth keeping: blanking an evidence column
  **as `cp_app` is refused outright**, because 00701 grants the application only
  `UPDATE (version)` on `capability_gates`. The privilege layer already covers
  the attacker-with-the-application-credential case. GT003 covers what is left
  — a defect in the Go layer, or anything running as the owner — so
  the test drives the owning connection. Written against `cp_app` it would have
  passed on SQLSTATE 42501 and never reached the guard it names, which is F-53's
  mistake again.

  The window half also had to be restructured: pushing a gate's state back from
  ACTIVE to APPROVED to reuse it is refused by the AU001 audit binding. Each
  case now gets its own gate, driven to APPROVED through the real proposal and
  approval path.

- **The value-domain family is classified.** `valuedomain.MapError` maps all
  five codes, `ledger.MapError` consults it, and
  `TestIntegration_ValueDomainRefusalsAreClassified` asserts what a caller
  receives rather than what the database said. Observed failing with the
  mapping removed: the refusal for moving Credits into real capital came back
  `INTERNAL` rather than `VALIDATION_FAILED`.

  Why the existing tests could not see it: every value-domain test in
  `internal/ledger` asserts on the *text* of the database's message, which is
  there with or without a mapping. Nothing asserted on the classification, which
  is what the customer and the on-call engineer actually see.

**The four fictions are withdrawn, not implemented, and that is a decision
rather than a shrug.**

Migration 00722 withdraws `CR002`, `CR005`, `PO002` and `PO004` and records what
actually enforces each invariant — as a `COMMENT ON` the schema object that
holds it, so a `\d+` or a schema dump carries the correction rather than a
sentence in a file nobody greps.

Implementing them would cost more than the fiction does. Making `CR002` real
means replacing `forbid_mutation` on three live tables with a credit-specific
function, changing what `db.IsImmutableRow` sees for those tables, to buy a more
specific name for a refusal that is already correct and already tested. Making
`PO002` real means a second copy of a state machine the CHECK and the AU001
transition binding already enforce — and a second copy is a second thing to
drift. The cost of the fiction was never a missing control; it was that an
application cannot tell these refusals apart from any other unique or check
violation.

What makes it a correction rather than an excuse is the pair of controls:

- `TestIntegration_EveryDocumentedSQLStateIsRaised` fails on any code a
  migration header documents that nothing raises and no later migration
  withdraws. Observed failing with 00722's withdrawal line removed, naming
  exactly those four and no others; passing with it back.
- `TestIntegration_TheWithdrawnInvariantsAreStillEnforced` drives all four
  invariants and watches each refusal: a second Credit asset (23505), a payout
  state outside the set (23514), a legal state change with no transition row
  (AU001), settling more than was reserved and reserving more than was requested
  (23514). A withdrawal that was really an abandonment fails here.

`CR002` is the one honest exception in that test, and it is stated as such: it
asserts that the three credit tables carry the `forbid_mutation` trigger, with
the correct BEFORE/ROW/UPDATE/DELETE bits, because driving a real credit lot
needs a journal transaction with balanced entries — a fixture belonging to
`internal/credit`, not to a migration test. The function's refusal is already
driven directly by `TestIntegration_Migrations`. Two halves, stated as two
halves.

The census caught a fifth case immediately: migration 00720, written in the same
session, documented a `PL002` that is a CHECK raising 23514. The header was
corrected before it landed.

**Still open, and smaller.** `PO003` is documented as "provenance mismatch" and
all three of its raise sites emit `PAYOUT_ALLOCATION_IMMUTABLE`; the Go handler
agrees with the SQL and contradicts the header, so the behaviour is right and
the name is wrong. `BT001` and the test-only `GT001`/`GT002`/`GT004`/`GT005`/
`AU002` are asserted somewhere, just not in production code.

## F-59 · The prediction resolver applied no maximum price age · BASELINE · P1 · FIXED

**Claim under test.** A prediction is scored against the market.

**What was found.** `Resolver.Resolve` reads two prices, at `CommittedAt` and at
the horizon end, through `PriceAsOf`, whose SQL is:

```sql
 WHERE i.id = $1 AND p.received_at <= $2
 ORDER BY p.received_at DESC, p.observed_at DESC
 LIMIT 1
```

`received_at <= $2` is an upper bound — a look-ahead guard, which is the
property the doc comment claims and which holds. There is no lower bound. No
max-age parameter, no interval, and `observed_at` is selected and scanned but
never compared to anything. The `Resolver` struct carries no age field.

So when the price feed for an instrument stops, both calls return the same row:
the newest price that exists, however old. `returnInBPS(start, finish)` is then
zero, `realized` is `DirectionFlat`, drawdown is zero, and `Validate` passes
because both price refs are non-empty. **Every open prediction on that
instrument is durably scored as a miss, and `prediction_outcomes` carries
`forbid_mutation`, so the score cannot be corrected.**

The platform has this concept and the resolver does not use it:
`asset_policies.max_price_age_ms` is `NOT NULL CHECK (> 0)` and is honoured by
`valuation.PriceStore.Latest`, which bounds both ends. `internal/prediction`
reads `asset_prices` directly and never consults `asset_policies`.

**Coverage.** `TestResolverRefusesWhenThereIsNoPrice` covers **zero** price rows
and asserts `CodeStaleMarketData` — a code used here for absence, never for
staleness. `TestResolveScoresFromPointInTimePrices` writes its prices one second
before commit and exactly at the cut-off, and its one "must not be used" case is
a price from the *future*. No test writes an old price. That is the shape of the
gap: the look-ahead direction was tested thoroughly and the look-behind
direction was not tested at all.

**Fix.** `Resolver.usable` refuses a pair of prices that cannot measure the
window they are meant to measure. Two rules, and only the second has a number
in it:

1. **Unconditional:** the two endpoints must be different observations. If
   `PriceAsOf` returned one row for both cut-offs, no price arrived between the
   commitment and the horizon, and the computed return is zero by construction
   rather than by measurement. FLAT would be a statement about our data.
2. **Bounded:** each price must be within `maxPriceAge` of its own cut-off,
   measured on `ReceivedAt` — per `PricePoint`'s own comment, only what we
   had received decides what was knowable when.

`NewResolver` takes the age as a required argument and refuses a non-positive
one, naming the consequence. A default inside the constructor is how this comes
back: silently, in whichever deployment forgot to set it. `cmd/agent-worker`
sets fifteen minutes, with the reason written where the number is.

**Evidence.** `TestIntegration_ResolverRefusesADeadFeed`,
`TestIntegration_ResolverRefusesOneObservationSpanningTheWindow`,
`TestIntegration_ResolverRefusesAStaleStart` and
`TestResolverRequiresAMaximumPriceAge`. The dead-feed test also asserts that
**no outcome row was written** — the table is append-only, so a
half-resolution would be as permanent as a wrong one. The one-observation test
carries the positive control: adding a second price inside the window makes the
same prediction resolve, and resolve UP.

**Left open deliberately, and recorded here so it is a decision.** A refused
resolution stays unresolved and is retried, which for a permanently dead feed
means retried forever. That is the right behaviour for a resolver — it must
not invent a score — but it needs the accounting in F-60 to be visible
rather than merely absent.

## F-60 · Calibration dropped predictions silently, and its denominator hid it · BASELINE · P2 · FIXED

**Claim under test.** A calibration snapshot describes how a strategy version
performed over a window.

**What was found.** `calibrationSourceSQL` joins `predictions` to
`prediction_outcomes` with an **inner** join, so every prediction in the window
without an outcome — unresolved, or repeatedly unresolvable through F-59
— is dropped before Go sees a row. `PGCalibrator` holds only a clock: no
logger, no counter, no metric. The excluded rows never cross the database
boundary and nothing anywhere compares the window's prediction count against the
outcomes found.

`CalibrationRow.NPredictions` is `len(bucket)` — the count that *survived*.
There is no field for the window's population, the excluded count, or the
unresolved count, so a reader cannot tell 6 of 6 from 6 of 600. Worse, that one
figure sits beside four statistics computed over four different divisors:
`RealizedFrequency` over `hitCount`, `BrierMean` over `brierCount`,
`LogLossMean` over `logCount`, the return means over `len(bucket)`. An unset
statistic persists as SQL `NULL`, not as a visible gap.

These snapshots are load-bearing:
`agent_lifecycle_transitions.calibration_snapshot_id` is a foreign key onto this
table, so **a promotion decision cites a snapshot whose sample loss is
unrecorded.**

The existing test pins the behaviour rather than questioning it:
`TestCalibrationIgnoresUnresolvedPredictions` builds one open prediction and
asserts `err == nil` with zero rows. It is a test *for* the silent drop. No test
covers the mixed case — some resolved, some not — and the only
aggregation test writes an outcome for all six of its predictions, so the
`hasHit` / `brier != nil` / `logLoss != nil` branches are never exercised in the
false direction.

The empty-bucket skip is documented as deliberate, and the reasoning is right:
"an empty bucket is omitted rather than reported as zero, because 'no evidence'
and 'perfectly calibrated' are not the same statement." That reasoning simply
was not carried through to the predictions the query dropped.

**Fix.** Migration 00721 adds `window_predictions` and `window_scored` to
`calibration_snapshots`, and `Compute` counts the window's population with a
second query that deliberately does **not** carry the outcome join. The regime
filter is left off that count too: a regime is a property of the outcome, so an
unresolved prediction has none and filtering on it would re-introduce the very
exclusion the count exists to expose.

Every row of one computation carries the same two figures, which looks redundant
and is not: a promotion cites **one** row by foreign key, so that row has to be
self-describing.

The columns are NULLable, and that is the honest choice rather than a
convenience. A row written before this migration was computed without the
figure; a default of zero would assert that nothing was dropped, which is
precisely the false statement at issue. New rows always carry both, which two
CHECK constraints require, and `Persist` refuses a row that does not so the
message names the snapshot rather than the constraint.

**Evidence.** `TestIntegration_CalibrationStatesWhatItCouldNotScore` builds six
predictions and resolves two, then asserts the snapshot says **6 and 2** and
that the buckets partition exactly the two -- a number the old code could not
produce, since it only ever counted survivors. It reads the figures back out of
the persisted row, which is what a promotion decision reads.
`TestIntegration_CalibrationSampleIsNotVacuous` is the positive signal: with
nothing dropped the two numbers must agree, so a `WindowPredictions` that had
silently collapsed onto `WindowScored` fails one test or the other.

**Not fixed, and recorded.** The per-statistic divergence remains: `NPredictions`
sits beside four means computed over four different divisors (`hitCount`,
`brierCount`, `logCount`, `len(bucket)`), and an unset statistic still persists
as NULL. That is a narrower problem than the sample loss and it needs a decision
about what a partially-scored bucket should report, not just a column.

## F-61 · `position_lots` had no database invariant beyond per-row bounds · BASELINE · P2 · FIXED

**Claim under test.** Provenance lots are held to their invariants by the
database, as the credit lots beside them are.

**What was found.** The schema is a strong guard on per-row arithmetic and no
guard at all on anything relational:

| Invariant | SQL | Go |
| --- | --- | --- |
| `quantity_original > 0`, `0 <= quantity_open <= quantity_original` | CHECK | yes |
| at acquisition `quantity_open = quantity_original` | **no** | yes (one placeholder used twice) |
| `status = 'CLOSED'` iff `quantity_open = 0` | **no** | yes (a CASE in the UPDATE) |
| open quantity only ever decreases | **no** | yes (`OpenAfter = Open - min(Open, remaining)`) |
| cannot dispose more than the open quantity | **no** | yes |
| Σ open lots per (account, asset) = WALLET balance | **no** — detected, never enforced | reported as `Drift` |

`cp_app` holds table-wide `UPDATE`, and `position_lots` is deliberately absent
from `appendOnlyTables` (correctly — it has lifecycle columns). So
`UPDATE position_lots SET quantity_open = quantity_original` satisfies every
constraint and every trigger, refilling a consumed lot.

The asymmetry is the finding: `credit_lots`, doing the same job for Credits,
**does** get a SECURITY DEFINER trigger raising `CR001 CREDIT_LOT_OVERCONSUMED`.
The two lot tables were written to different standards.

No test writes an invalid lot directly through SQL. Every `position_lots`
reference in a test is a `SELECT`, and the concurrency test drives
`e.Dispose`, so it proves the Go compare-and-set rather than a database
constraint.

**Fix.** Migration 00720, in the shape `credit_lots` already had:

- `position_lots_status_matches_quantity` states `(status = 'CLOSED') =
  (quantity_open = 0)` as a CHECK. Both operands are NOT NULL, so the expression
  can never be NULL, which is F-50's lesson.
- `cp_position_lot_guard` raises `PL001` on an open quantity that rises, and on
  any change to `quantity_original`, `acquired_at` or `cost_basis_usd_minor` --
  the columns a FIFO ordering and a realized gain are computed from. Moving
  `acquired_at` reorders every disposal that has not happened yet.
- The UPDATE grant narrows to `quantity_open` and `status`, so the application
  is refused by privilege before a trigger is reached.

`updated_at` is deliberately not granted: the `set_updated_at` BEFORE trigger
assigns it and PostgreSQL checks column privileges against the statement's SET
list rather than what a trigger writes. That was verified by running the
disposal path, not reasoned about, and
`TestIntegration_DisposalStillWorksUnderTheGuard` asserts `updated_at >
created_at` so a wrong answer surfaces as a failure rather than as a stopped
clock.

The header of 00720 originally documented a second code, `PL002`, for the CHECK
-- which raises 23514 and not a custom code at all. Writing F-58 in the same
session is what caught it. A header that names a code has to be a header whose
migration raises it.

**Evidence.** Five tests, observed failing with the trigger dropped, the CHECK
dropped and the table-wide grant restored: the consumed lot was refilled, the
cost basis was edited, the acquisition time was moved back a day, the status was
decoupled from the quantity, and `cp_app` did all of it.
`TestIntegration_DisposalStillWorksUnderTheGuard` is the positive control, and
it is the reason the grant names exactly two columns -- one too few would pass
every refusal above and silently break the only path that writes these rows.

## F-62 · `normalized_events` replaced a column that varied between copies · BASELINE · P3 · FIXED

**Claim under test.** Re-ingesting an event is idempotent, as
`reality.Pipeline`'s doc comment says: "Every step is idempotent downstream
(archive dedup key, bus dedup id, ReplacingMergeTree, checkpoint version), so a
crash between steps replays safely."

**What was found.** The ClickHouse table is
`ReplacingMergeTree(platform_received_at)`, `ORDER BY (source, event_type,
dedup_id)`, `PARTITION BY toYYYYMM(decision_available_at)`.

On a re-ingest the archive returns the **original** meta, so
`platform_received_at` — the version column — is frozen; but
`Normalize` is handed a fresh `clk.Now()`, so `normalized_at` and therefore
`decision_available_at` are strictly later. **The version does not move and the
replaced column does.** Two rows with the same sort key and the same version
carry different `decision_available_at`, and which survives is decided at merge
time rather than by the data. Because the partition key is derived from the
column that varies, a re-ingest landing in a different month produces two rows
that `FINAL` cannot collapse.

There is also a semantic disagreement: `reality.Dedup` keeps the **first**
occurrence, and its comment says that is "what ClickHouse's ReplacingMergeTree
does at merge time". ReplacingMergeTree keeps the highest version or the last
inserted. When copies differ, a bus consumer and a ClickHouse reader resolve to
opposite values.

**Severity P3, and only because of what does not read it yet.** Both readers of
`decision_available_at` (`QueryNormalized`, `QueryMarketPrices`) have no
non-test caller: `internal/backtest` and `internal/performance` do not exist,
and `reality.Snapshotter`, which POINT_IN_TIME.md describes as the consumer,
returns zero hits. The Postgres lineage of the same column name, on
`predictions` and `tool_invocations`, is independent and unaffected. This is a
defect in the substrate of an unbuilt subsystem — recorded now because it
will be invisible once something reads it.

**The test masks it.** `TestIntegration_ClickHouse_NormalizedRoundTripAndReplacingDedup`
builds its duplicate by shifting `PlatformReceivedAt` **and**
`DecisionAvailableAt` together, so the version advances and the assertion "the
latest platform_received_at wins" holds deterministically. The pipeline does
the opposite. The pipeline-level test does exercise the real path but runs on a
fake clock that is never advanced between the delivery and the redelivery, so
both normalisations produce identical timestamps and the divergence cannot
appear. Two tests, neither able to see it: the F-26 shape again.

**Fix, and why it is in Go rather than the DDL.** The obvious repairs are to
the table — put `decision_available_at` in the sort key, or partition on the
version column instead. Neither can be deployed: `EnsureSchema` applies the DDL
with `CREATE TABLE IF NOT EXISTS`, so a new key never reaches a table that
already exists. A fix that cannot reach a running deployment is not a fix.

The root cause is in the pipeline anyway. `Ingest` handed `Normalize` a fresh
`clk.Now()` on every pass while `PutMeta` returned the original archived meta,
so the version column was frozen and the replaced column was not. It now derives
`normalized_at` from the archived `platform_received_at`, which makes every pass
produce byte-identical timestamps — and that is what makes the replacement
well-defined, whatever the sort key is.

It is also the more honest reading of the policy. `AvailabilityPolicy` already
adds `FeatureLatency` and `PipelineLatency` as *configured* allowances for
processing time, so a wall-clock `normalized_at` was charging real latency on
top of the modelled latency — inconsistently, and only on the first pass.

`reality.Dedup`'s comment, which claimed to match ReplacingMergeTree, is
corrected: it keeps the first occurrence, the engine keeps the last. The two
agree now because the copies are identical; before, a bus consumer and a
ClickHouse reader could resolve the same event to opposite values with neither
of them wrong.

**Evidence.** `TestIntegration_ReplayingAnEventProducesTheSameTimestamps`
re-ingests one event twice with an **advancing** clock — five minutes, then
forty days — and asserts every timestamp and the monthly partition are
unchanged. Observed failing with the wall clock restored:
`decision_available_at` moved from `12:00:00.755` to `12:05:00.755`, and on the
second replay to **October**, a different partition that `FINAL` cannot collapse
across, while `platform_received_at` stayed exactly where it was.

`TestIntegration_ADifferentEventStillGetsItsOwnTimestamps` is the positive
control: a pipeline that had simply frozen every timestamp would pass every
assertion above and record a stream in which nothing ever happens at a different
time. `TestReplayDeterminismIsAPropertyOfTheDerivation` states the rule without
a database and asserts that a later `normalizedAt` *would* have moved the
result — so the determinism test is not passing on a derivation that ignores
its input.

**What is still true.** There are still no production readers:
`internal/backtest` and `internal/performance` do not exist, and both readers of
`decision_available_at` have only test callers. This was fixed now because the
defect becomes invisible the moment something reads it.

## F-63 · The correction pass made the mistake it was correcting · NEW · P2 · FIXED

**Found by** an independent read of `internal/auth` after F-55 had been
committed, which reported that `auth.Manager.RevokeAllForSubject` has no caller
outside its own package. The runbook step that names it had been edited by F-55
to say it was served.

**What was wrong.** F-55 removed 140 `PENDING` markers that named packages and
binaries which exist. Three of those removals were wrong, because the marker was
about a *capability* and the pass judged the *package*:

| F-55 wrote | The truth |
| --- | --- |
| "`auth.Manager.RevokeAllForSubject(subject)` ..., served by `cmd/api`" | No route exists. `SessionsPort` exposes `ListForSubject` and `Revoke` by session id, and nothing outside `internal/auth` calls `RevokeAllForSubject`. The original marker said "PENDING: `cmd/api` route" and was right. |
| "executor and recoverer wired in `cmd/execution-worker`" (five places) | `bindProviders` returns an error in every production build — *"the venue adapter, chain observer, inspector, signing client and recoverer are not wired in this build"* — and is replaced only in tests. The worker cannot start. |
| "the agreement policy ... wired into `cmd/market-ingest-worker`" | `openProvider` returns a single `chain.SolanaDataProvider`. `chain.MultiObserver` and the two-observer agreement run in tests only. |

The reasoning that produced all three was the same, and it is exactly the
reasoning F-55 exists to name: *the directory is there, so the thing is done*. It
was committed in the act of correcting 137 instances of the same error.

**Why the controls F-55 shipped did not catch it.**
`TestDocs_EveryRunbookRouteIsServed` looks for HTTP paths, and the session step
names none. `TestDocs_NothingMarkedPendingAlreadyExists` looks at markers, and
the pass had removed the marker. Both check that a *negative* claim is honest;
neither checks a *positive* one. That was the missing third direction.

**A fourth thing the same read exposed**, and the reason the session step now
says more than "there is no route": **nothing revokes a session when a role is
revoked.** Roles are frozen into the session row at login, `operator_roles` is
read nowhere else, and `Manager.Rotate` — whose doc comment promises
"rotation on privilege change ... revokes the old session and issues a new
token" — also has no caller outside its package. So during an
admin-compromise incident, clearing `revoked_at` on the suspect's roles leaves
their ADMIN live until the session's own 12-hour expiry. The runbook now says to
revoke the sessions, not the roles.

**Fix.** All three claims corrected, and reworded so the marker names the thing
that is actually missing — `bindProviders`, `chain.MultiObserver` —
rather than a package that exists. That is the rule
`TestDocs_NothingMarkedPendingAlreadyExists` enforces, and it refused two of the
first rewrites for naming `cmd/execution-worker`, which was the check working on
the person who wrote it.

Writing those rewrites also exposed a flaw in that check: its clause boundary
knew about `". "` and not about `".**"`, so a marker in bold read on into the
next sentence and blamed it for packages that sentence merely mentioned. Fixed
with a `sentenceEnd` that understands markup.

**The new control.** `TestDocs_SessionRevocationIsStillUnreachable` asserts both
halves: that `RevokeAllForSubject` still has no caller outside its package, and
that the runbook step naming it still carries a marker. It fails in both
directions — if the function becomes reachable it says to delete the marker
and the control, and if the marker disappears while the function is still
unreachable it says the runbook has gone back to claiming a route. Observed
failing on the second, by restoring the exact sentence F-55 wrote.

It is deliberately one named claim rather than a general scan. The general form
was written first and abandoned: across every runbook there is exactly **one**
function written in call form, so a scan would guard a corpus of one while
carrying a name-based call index that cannot tell a package's own internals from
an external caller — which it got wrong on its first run, reporting
`RevokeAllForSubject` reachable because `auth.Manager` calls its own store method
of the same name. A curated claim that is honest about being curated is worth
more than a general check that is wrong.

**What this says about F-55.** The other 137 removals were checked against the
OpenAPI spec or against a package's existence where existence was the whole
claim, and `TestDocs_EveryRunbookRouteIsServed` covers the largest group of them
mechanically. These three were the ones where the marker's subject was narrower
than the package it named, and nothing distinguished them at the time. The
lesson is not "check more carefully" — it is that a positive claim about
wiring needs a check of its own, which it now has for the case that produced it.

## F-64 · An approval did not stay attached to what was approved · NEW · P1 · FIXED

**Found by** an independent adversarial read of `internal/admin` and
`internal/adminplane`, verified against the schema before acting.

**What was found.** Three columns decide what dual control means, and nothing
protected any of them:

| Column | Decides |
| --- | --- |
| `requires_dual` | whether a second signature is needed at all |
| `kind` | what the approval authorises |
| `target_id` | what it authorises it against |

All three sat on a table `cp_app` held **table-wide `UPDATE`** on, with no CHECK,
no trigger, and no coverage by `params_hash` — which hashes `params` alone
(`ParamsHash: hashBytes(params)`).

**Two consequences.**

*An approval could be repointed.* `VerifyApproved(ctx, q, approvalID, kind,
targetID)` compares the caller's target against the stored one, and nothing bound
the stored one to what was signed for. Two principals approve the release of one
kill switch; a single `UPDATE admin_actions SET target_id = ...` makes the same
approval verify for a different one. `KILL_SWITCH_RELEASE` is the sharp case
because its whole subject lives in `target_id` and its params are empty —
the hash protects nothing there.

*The dual-control gate read the row.* `VerifyApproved` used `a.RequiresDual`
while `Approve` and `executable` used `spec.RequiresDual` from the code's
`KindSpec`. It was the **only** consumer that trusted the column, and it is the
gate `killswitch`, `agent` and `reconciliation` call before acting. A row set to
`requires_dual = false` satisfied it with no approver at all.

**Severity P1** because it is the authority mechanism itself: dual control is the
control that makes every other administrative action safe to have. It needs the
application's database credential, which is the same bar as F-42 — and the
same answer, that privilege is the thing that holds when detection does not.

**Fix, in two independent halves.**

Migration 00723 freezes a proposal's identity with a guard trigger raising
`AD001`, and narrows the grant to exactly the columns `updateStatus` writes —
read off its five call sites, not guessed. `VerifyApproved` now derives dual
control from the `KindSpec` like every other caller.

Two things about the trigger's scope are worth recording, because both were got
wrong first.

*`params` is deliberately not frozen; `params_hash` is.* Freezing `params` made
the `ErrParamsTampered` check in `Execute` unreachable — a working, tested
control turned into one only a test could walk, which is the F-26 class this
session has spent its time removing. Freezing the hash instead is what makes the
pair sound: change `params` and the hash no longer matches; change the hash and
the trigger refuses; change both and the trigger refuses. `params` is outside the
grant regardless, so the application cannot rewrite it either way.

*The behavioural test for the dual-control half is deliberately not written.*
Reaching that branch needs an APPROVED dual-control row with no approver, which
needs a hand-written `admin_action_transitions` row to satisfy AU001 — which
is exactly the forgery F-42 records, performed by a test. The first attempt did
it, and the package's own `TestIntegration_AdminStreamVerifiesAfterEverything`
caught it immediately: one transition, no audit event, invariant broken. A
fixture that has to commit the exploit to reach the code is a fixture that should
not exist, so that half is asserted against the package's own source instead, in
the shape `accountscope_test.go` already uses.

**Evidence.** `TestIntegration_AnApprovalCannotBeRepointedAtAnotherTarget` drives
five identity columns as the owner (so a refusal is the trigger) and four as
`cp_app` (so a refusal is the grant), then re-verifies the approval still holds
for its own target and refuses the other one. Observed failing with the trigger
dropped and the table-wide grant restored: **the repoint succeeded.**
`TestIntegration_TheLifecycleStillMoves` is the positive control — a column
list one entry short would pass every refusal above and break
propose/approve/execute, and the failure would look like actions never completing
rather than like a permission error anybody was watching for.

`TestIntegration_TamperedParamsAreRefused` was updated rather than left: it used
to plant its tamper as `cp_app`, which the narrowed grant now refuses. It asserts
that refusal as the first line and plants the tamper as the owner to keep driving
the hash check as the second.

**Still open from the same read, and recorded rather than fixed:** two identities
for one human defeats approver ≠ proposer entirely, because `users` is
unique on `(idp_issuer, idp_subject)` and there is no person entity — a
human with two IdP subjects can propose from one and approve from the other.
`internal/security/roles.go` says "individually identified (no shared accounts)",
which is a comment rather than a mechanism. That is an identity-model decision,
not a code fix.

## F-65 · Two kill switches that reach nothing, documented as working · NEW · P2 · PART

**Found by** an independent adversarial read of `internal/killswitch`, verified
against the matrix, the agent runtime and the import boundary before acting.

**`MODEL_DISABLE` can never match.** `Matches` requires the action to carry a
model id:

```go
case ModelDisable:
    return a.ModelID != "" && (sw.ScopeID == GlobalScope || a.ModelID == sw.ScopeID)
```

No non-test code sets `ModelID` on a `killswitch.Action` — every
`ModelID:` assignment in the tree is on an audit or event struct. And it cannot
be fixed at the call site, because the model-call path is architecturally
forbidden from consulting a kill switch at all: `internal/killswitch` is in
`forbiddenForAgents`, and `internal/agent` and `internal/model` are agent trees.
`test/security/authority_boundary_test.go` enforces that.

**`AGENT_PAUSE` writes a table the agent runtime does not read.** Activating it
through `POST /admin/kill-switches` writes a `kill_switches` row. Every
agent-runtime pause check reads `agent_pauses` — the dispatcher, the broker,
the model call, the emitter and the runner, five independent points, all correct
and all looking at the other table. `agent.KillSwitchMirror` exists precisely to
keep the two together, and is implemented by nothing; its own doc comment says
the mirror is optional because "the pause itself is authoritative in
`agent_pauses`", which is true of `Lifecycle.Pause` writing *outwards* and says
nothing about an operator writing *inwards*.

**Four documents said otherwise**, and one runbook is the place an operator
looks during exactly this incident:

- `model-malfunction.md` step 2: *"The ToolBroker refuses `CALL_MODEL` under
  `MODEL_DISABLE`"*
- `AGENT_RUNTIME.md` §: `KILL_SWITCH_ACTIVE` listed for `MODEL_DISABLE`
- `SECURITY.md`: the ToolBroker row lists both switches among its checks
- `clickhouse-outage.md`: *"use `AGENT_PAUSE` per affected agent"*

`REQUIREMENTS_TRACEABILITY.md` marked R-053-12 (`MODEL_DISABLE`) **VERIFIED**,
citing a test that asserts the kind's severity and scope-string rules — which
is true of the kind and says nothing about whether anything consults it. That is
F-58's shape in the traceability matrix: a citation that is accurate about
something other than the claim.

**Severity P2 rather than P1**, and the reason is worth being precise about. The
agent runtime is inert: `agent.NewLifecycle` and `agent.NewEmitter` have no
production callers, `cmd/agent-worker` wires no evaluator and its `EmitterFor`
returns `UNSUPPORTED`. So there is no running agent for either switch to fail to
stop. The defect is that the documents promise a control that would not work the
day the subsystem is turned on — and the day it is turned on is exactly when
nobody re-reads the runbook.

**Fixed here: the documents.** All four now say what actually happens, and
`model-malfunction.md` points the operator at the mechanism that does work
(pause the agents; the broker checks `agent_pauses` on every call). R-053-12 is
downgraded to IN_PROGRESS with the reason.

**Not fixed here, deliberately: the bridge.** Implementing `KillSwitchMirror`, or
teaching the operator path to write `agent_pauses`, would wire a control into a
subsystem that does not run — producing exactly the thing this session has
spent its time removing, a control only a test can reach. It belongs with the
work that makes `internal/agent` reachable, and the runbooks now say so where an
operator will read it.

### 2026-09-10: the deferral is now watched, because it rested on an unwatched fact

The reasoning above is sound and it rests on a **fact about the code** —
`agent.NewLifecycle` and `agent.NewEmitter` have no production callers, and
`cmd/agent-worker`'s `EmitterFor` returns UNSUPPORTED. Both were re-verified
before writing this, and both still hold.

Nothing was watching them. **The day the agent runtime becomes reachable is
exactly the day nobody re-reads a finding from months earlier**, and that is the
day two runbooks start describing a control that does not work while an operator
is following them.

`TestDeferredBridge_TheAgentRuntimeIsStillInert` (`test/security`) watches the
premise rather than the bridge — there is no bridge to test yet. When the
runtime acquires a production caller it fails and states what has become owed:
the `agent_pauses` bridge for `AGENT_PAUSE`, and a separate decision for
`MODEL_DISABLE`, which cannot be fixed at the call site because
`internal/killswitch` is in `forbiddenForAgents` and the model path may not
consult it.

Proven non-vacuous by planting a file that mentions the constructor and watching
it fail, naming the file.

**This is what a deliberate deferral should look like in this repository.** The
finding stays PART, because the bridge is genuinely not built and pretending
otherwise would be the overstatement this register exists to refuse. What
changed is that the deferral can no longer expire quietly.

## F-66 · A future authentication time satisfied every step-up window · NEW · P1 · FIXED

**Found by** an independent adversarial read of `internal/auth` and
`internal/identity`, tracing `AuthTime` from where it is set to where it is
checked. Verified against the source and the generated decision vectors before
acting.

**What was found.** `security.RequireStepUp` clamped a negative age to zero
without a bound:

```go
age := now().Sub(p.AuthTime)
if age < 0 {
    age = 0
}
```

`auth_time` is copied verbatim out of the OIDC ID token and validated nowhere.
`checkClaims` verifies `azp`, `nbf`, `iat`, `nonce` and `sub`; `Identity.Validate`
checks only that a subject is present; `Session.Validate` does not mention it;
the column carries no CHECK.

So an `auth_time` any distance in the future — an hour, a year — made
the age zero, which satisfies **every** step-up window in the system: the
15-minute HTTP surface, and the 5-minute windows on the tightest admin kinds
including `LEDGER_CORRECTION`, `WITHDRAWAL_APPROVE` and `BREAK_GLASS_GRANT`. Not
once, but for the whole 12-hour life of the session, because step-up freshness is
the only thing those windows measure.

`adminplane.Actor.steppedUp` and the console's TypeScript port
(`apps/admin/src/decide.ts`) carried the same unbounded clamp, so the affordance
layer agreed with the enforcement layer — both wrong in the same direction.

**Severity P1.** Step-up is the freshness check standing between a stolen
session and every dual-controlled action. The controlling party is whatever mints
the ID token, so this is not reachable by an HTTP caller — `httpmw` reads the
cookie and nothing else, which was checked — but a misconfigured or
compromised identity provider, or a badly-set clock on one, turns it into a
permanent step-up bypass. There was **no test anywhere** for a future
`auth_time`; the OIDC fixture uses a value in the past.

**Fix.** A negative age is treated as "now" only within the tolerance the token
verifier already applies to `exp`, `nbf` and `iat` — `oidc.DefaultClockSkew`,
two minutes. Beyond it the age is not fresher, it is unknown, and an unknown age
fails closed with a message that says so rather than reporting a stale
authentication and sending a reader to the wrong place. The same bound is applied
in `adminplane` and in the console port, because an affordance more permissive
than the server is a live button the server refuses — which
`adminplane`'s own doc forbids.

`security.MaxAuthTimeSkew` and `oidc.DefaultClockSkew` are two numbers that must
agree and live in packages that do not import each other, so
`TestStepUpSkewMatchesTheTokenVerifier` pins them together.

**Evidence.** `TestRequireStepUp_AFutureAuthTimeBeyondSkewIsRefused` drives
inside the tolerance (0, 1 s, exactly the skew — all still pass), beyond it
(skew + 1 s, an hour, a year — all refused), and both ordinary cases as
positive controls: a recent authentication passes and a stale one still fails for
the ordinary reason. Observed failing with the bound removed: an auth time 2m1s
in the future was accepted as a fresh step-up.

The generated decision vectors moved, and the movement is the finding stated as
data: **139 cases changed, all of one principal** — `other_admin_elevated_late`,
whose `auth_time` sits 48 hours after the instant those cases evaluate at. **65
went from ALLOWED to STEP_UP_REQUIRED.** The same principal's cases at the later
instant, where it is genuinely one minute old, are unchanged, so the fixture
keeps the purpose it was written for.

## F-67 · The signing recovery path signed bytes nobody approved · NEW · P1 · FIXED

**Found by** an independent adversarial read of `internal/signing`, verified
line by line before acting.

**What was found.** `Sign` computes the request's hash first:

```go
txHash := sha256.Sum256(req.UnsignedTx)

// Fast replay path without chain I/O.
if existing, err := s.repo.findDecision(...); ... else if existing != nil {
    return s.replay(ctx, req, existing, log)
}
```

and on the replay path never used it. When a decision exists and is APPROVED but
no `signing_results` row does, `replayLoaded` finishes the job:

```go
// Approved but never signed (provider failure or crash): finish signing.
l, err := s.loadWalletAndAccount(ctx, req)
...
return s.signApproved(ctx, req, d, l.wallet, ...)
```

and `signApproved` hands the provider `req.UnsignedTx` — **this call's
bytes**. Nothing compared them against the decision's `InspectedTxHash`, which is
recorded on the row and was simply unused. `Request.Validate` checks only that
`ExpectedTxHash` is 32 bytes; it never checks that it is the hash of
`UnsignedTx`, and both come from the same caller anyway.

So for any attempt in the state *approved, not yet signed*, a second call with
the same `attempt_id` and arbitrary transaction bytes signed those bytes with the
wallet key. None of the sixteen inspector checks runs again on that path, and
neither does `validateLinkage` — wallet status, delegation, plan approval,
risk ALLOW, reservation and quote expiry are all re-read on the first path and
none of them on this one. The hash was the whole of the binding, and it was not
being applied.

**The window is not exotic.** The decision commits in one transaction and the
provider is called in a second. Every provider failure or crash between them
produces exactly this state, and the code documents it as normal recovery —
`TestIntegration_ProviderFailureThenRecovery` exercises it deliberately.

**Latent, not live.** `internal/signing` has no non-test importer:
`cmd/execution-worker`'s `bindProviders` returns an error in every production
build (F-63). This is a code defect on the most security-critical path in the
system, found before that path was turned on.

**Fix.** The replay path refuses unless the offered bytes hash to what the
decision inspected. The concurrent-winner path needed the same treatment and did
not obviously look like it: when a parallel `Sign` wins the attempt lock, the
decision loaded under that lock belongs to *its* request, so its hash travels out
of the transaction with it rather than being assumed equal to this call's.

**Evidence.** `TestIntegration_RecoveryRefusesDifferentBytes` leaves an attempt
approved-but-unsigned, then calls again with one byte flipped and an
internally-consistent `ExpectedTxHash` — which is what the old
`Request.Validate` was checking, and why it was no defence. Observed failing with
the binding removed: execution continued past the check and reached the signer.

Two positive controls, and both are load-bearing. The genuine recovery must still
complete — a binding that refused everything would make every provider
failure permanently unrecoverable, which is worse than the defect. And
`TestIntegration_ReplayOfASignedAttemptStillReturnsTheStoredBytes` pins the path
that must **not** gain a binding: once a result exists the replay returns the
stored bytes without reaching the signer, so a refusal there would break
idempotency instead.

**Related, from the same read and not fixed here:** `signing_decisions` has only
a plain index on `attempt_id`, not a unique constraint — "one decision per
attempt" is held by a `SELECT ... FOR UPDATE` on the attempt plus a re-check, and
`findDecision` concedes it with `ORDER BY created_at DESC ... LIMIT 1`. That is
the F-49 shape and it belongs with the work that makes this package reachable.

## F-68 · The production velocity policy bounds nothing, and said the opposite · NEW · P2 · FIXED

`cmd/api` wires `withdrawal.VelocityPolicy{}` under this comment:

> the bounds stay at zero (no rolling allowance) until the gate is approved

"Bounds at zero" reads as a refusal. A zero policy refuses nothing:
`MaxPerRequest` is not positive so the per-request branch is skipped, and
`!MaxPerWindow.IsPositive() && MaxCountPerWindow == 0` returns nil before
anything is counted. A request of 2¹²⁸−1 base units passes.

The semantics are deliberate and tested — `withdrawal_test.go` asserts "zero
policy is unlimited" — so the defect is not the policy type; it is that the
one place that wires it believed the opposite, in a comment, on a money bound.

**What actually refuses a withdrawal today** is the WITHDRAWALS capability gate,
DISABLED in every environment and checked several steps earlier. That is the
load-bearing control and the velocity policy is not a second one.

**Fix.** The value has a name that says what it does —
`withdrawal.UnboundedVelocity()` — with `PermitsEverything()` beside it, and
the composition root's comment now states the truth. Naming it is the point: a
reviewer reading the composition root sees a value that announces it bounds
nothing, rather than an empty struct that looks like a default somebody chose.

**Evidence.** `TestUnboundedVelocityBoundsNothing` drives the wired policy with
2¹²⁸−1 and asserts it passes, so the fact is recorded rather
than believed — and the day the gate is approved, that test is what says the
velocity policy is still a no-op. `TestARealPolicyActuallyBounds` is the positive
control: without it the first test would also pass against a `Check` that had
stopped refusing anything at all.

## F-69 · Inventory: six independent audits, and what was not fixed · NEW · P2 · OPEN

Six read-only audits ran over the subsystems this session had not touched:
auth/identity, killswitch/eligibility, capital/withdrawal, signing/execution,
admin dual control, and agent authority. Each was asked for the specific defect
classes this codebase keeps producing rather than for a general review.

**What came out of them and was fixed:** F-64 (dual control), F-65 (two dead
kill switches), F-66 (the step-up bypass), F-67 (the signing recovery path),
F-68 (the velocity comment), and the half of F-63 about session revocation.

**One audit claim was wrong and is recorded as such.** It reported that migration
00717 revokes `cp_readonly`/`cp_ops` SELECT on `identity_pii` and `sessions`.
00717 contains no REVOKE; it contains a comment *describing* the revoke that F-47
took back out. That is the second time an agent has misread that comment as the
code, which is why every claim in this session was verified before action.

**Verified and NOT fixed.** Each of these was checked against the source; the
reason for leaving it is given, because "not fixed" without a reason is
indistinguishable from "not noticed".

| What | Verified | Why not fixed here |
| --- | --- | --- |
| A withdrawal is checked against no balance, settlement state or hold — in Go **or** SQL | `internal/withdrawal` imports neither `capital` nor `buyingpower`; `Repository.Transition` has zero non-test callers and there is no approve route, so the "consulted at approval time" the package doc promises has no approval time to happen at | This is Stage-not-built, not a defect in built code. The WITHDRAWALS gate is DISABLED and is the control. Building the approval path is the work, not patching around its absence. |
| `EnvelopeService` is entirely unreachable, so `ApplyRealizedPnL` never runs and no envelope can be exhausted by losses | `NewEnvelopeService` has only test callers; no `EnvelopeAdmin` port, no OpenAPI path, no executor for `ENVELOPE_AUTHORITY_CHANGE` | Same shape. Wiring a limit into a subsystem nothing calls produces a control only a test can reach. |
| ~~`kill_switches_require_transition` fires `AFTER UPDATE OF active`, not on INSERT~~ | — | **Fixed as F-70**, with the migration and tests it wanted. |
| `internal/eligibility` has zero non-test callers for every function that refuses anything | Only non-test use is a struct field consumed inside the unreachable planner | Already recorded under F-34. Repeating it here so the inventory is complete. |
| ~~`agents.stage` and `agents.mode` are not bound to a transition row~~ | — | **Fixed as F-78**, which turned out to be larger than the item: the same gap let a transition row deny the change it licensed, and a promotion to LIVE needed no approval at all. |
| ~~`agent.NewEmitter` does not require an `EnvelopeReader`~~ | — | **Fixed as F-75.** The deferral reason was wrong: a constructor that must refuse a nil dependency needs no runtime to test. |
| ~~The runner never reads the agent's lifecycle state~~ | — | **Fixed as F-73**, with the migration the new skip reason needed. |
| ~~`Store.LiveTradingEnabled` scans `effective_at` and `expires_at` and discards them~~ | — | **Fixed as F-72.** On re-reading it, "which reader is authoritative" was not a design call: the mandate is to fail closed, and the stricter reader is the one that does. |
| ~~Four admin action kinds have no test at all~~ | — | **The two with live executors are fixed as F-80.** The other two execute nothing yet, which is F-34's territory. |
| ~~`ApproverIsNotTarget` is never evaluated true in any test or generated vector~~ | — | **Fixed as F-71**, with the export, the vectors, the console branch and the test that reaches it. |
| `admin.refuseAgent` permits the **absent** principal, so `VerifyApproved` runs fully unauthenticated | `if p, ok := PrincipalFrom(ctx); ok && p.ActorType == ActorAgent` | Deliberate per its doc ("it needs no principal but refuses agents") and every caller holds a `db.Querier` already. Recorded because the reasoning deserves to be revisited, not because it is wrong today. |
| Two IdP identities for one human defeat approver ≠ proposer entirely | `users` is unique on `(idp_issuer, idp_subject)`; there is no person entity; `RoleAdmin` holds both halves of break-glass | An identity-model decision, not a code fix. Already noted under F-64. |
| ~~`login_attempts` accumulates plaintext `nonce` and `code_verifier` indefinitely~~ | — | **Fixed as F-79**, in `audit-worker`, which already owns what the platform keeps and for how long. (The inventory named `PurgeExpired`, which purges sessions; `login_attempts` had no purge function at all.) |
| ~~`internal/auth/httpmw`'s guards all have zero non-test callers~~ | — | **Fixed as F-76**, in the doc and with a control that keeps it true. Six guards, not five. |
| ~~`signing_decisions` has a plain index on `attempt_id`, not a unique constraint~~ | — | **Fixed as F-81**, with the migration and the test that shows the second row being refused. |

**Two things the audits confirmed are genuinely well covered**, recorded because
a clean answer is worth as much as a finding: the kill-switch never-blocked
classes are proven by exhaustive property tests over all 2¹² kind
subsets with a nil `Querier`, so a database read would panic rather than pass;
and `agent.Authority` immutability is asserted by running the full adversarial
prompt corpus through quarantine and re-checking the fingerprint, effect set,
tool set, budgets, stage and mode byte for byte.

## F-70 · The first activation of a kill switch went unbound · NEW · P2 · FIXED

**Found by** the killswitch audit; verified against 00603 and `Controller.Activate`
before acting. Listed in F-69's inventory as worth a migration of its own, which
this is.

**What was found.** Migration 00603's header says it binds *"every state change
of an audited entity to a transition row written in the SAME transaction"*. For
`kill_switches` it binds one event:

```sql
CREATE CONSTRAINT TRIGGER kill_switches_require_transition
    AFTER UPDATE OF active ON kill_switches
```

A switch's **first** activation updates nothing. `Controller.Activate` calls
`insertActive`, which writes `active = true` in the INSERT itself, and the
trigger never saw it. `cp_app` holds INSERT, so a first activation carrying no
audit row was a legal write as far as the database was concerned. One test in
the tree did exactly that as a fixture convenience.

Go always writes the row — `Activate` calls `c.record` in the same
transaction — so the trail was complete in practice. That is F-49's shape,
and it matters more here than in most: this table is the record of who stopped
the platform and why, and the first activation is the one an incident review
reads.

`kill_switches` was also the only table carrying that trigger with **no negative
test at all**. `withdrawals`, `orders`, `trade_intents`, `deposits`,
`capability_gates`, `agents` and the three credit tables each have one.

**Fix.** Migration 00724 adds `cp_require_transition_on_insert`, attached
`AFTER INSERT ... WHEN (NEW.active)`. The `WHEN` is the whole of the scoping and
it is deliberate: a row born **inactive** is not a state change from anything
— it is the absence of a switch, written down — and requiring an audit
row for it would be requiring evidence that nothing happened.

**Evidence.** `TestIntegration_ASwitchBornActiveNeedsItsTransitionRow` drives all
three cases: born active with no transition row (refused, `AU001`), born active
with one (commits), born inactive with none (commits). Observed failing with the
trigger dropped: the unaudited activation committed.

`TestIntegration_ActivateStillWorksThroughTheController` is the positive control
that matters most — a trigger one condition too broad would pass every
refusal above and stop an operator halting the platform, and it would look like
the kill switch failing at the worst possible moment. It activates through the
real controller, asserts exactly one transition row was recorded, and then
asserts the switch actually blocks, so it is a switch and not just a row.

Writing it reproduced a defect this repository already knows: the first version
built scope ids from the **head** of a UUIDv7, which is the millisecond
timestamp, so two ids minted in the same millisecond collided on
`UNIQUE (kind, scope_id)`. `internal/httpapi`'s `launchMarket` carries a comment
warning about precisely that.

## F-71 · The grantee of an elevation was offered a button the server refuses · NEW · P2 · FIXED

**Found by** the adminplane audit; verified against `decision.go`, the generated
corpus and `apps/admin/src/decide.ts` before acting. Listed in F-69's inventory.

**What was found.** `decideApprove` carries two shapes of self-approval. The
first is approver ≠ proposer. The second is `ApproverIsNotTarget`: for a kind
whose `target_id` names a person who *gains* something — `BREAK_GLASS_GRANT`
is the only one — that person may not approve it, however many others were
involved, because otherwise the second signature is supplied by the beneficiary.

The server enforces both and that is proven end to end. The **affordance layer's**
copy of the second was never evaluated true:

```go
case a.UserID == "":
    d.Reason, d.Code = ReasonSubjectNotUser, errs.CodeForbidden
case a.UserID == action.ProposedBy:
    ...
case spec.ApproverIsNotTarget && a.UserID == canonicalUserID(action.TargetID):
```

Every fixture in the package used `TargetID: "t-1"`. That is not a user id, so
`canonicalUserID` returned `""`, and the `a.UserID == ""` case two lines above
caught first — in every unit test, every integration case and every generated
vector. `ActionKind` therefore had no such field, `authority.json` could not
carry it, and `apps/admin/src/decide.ts` had **no target check at all**: the
console rendered an enabled Approve button to the grantee, and the server
refused the click.

That is the specific failure `internal/adminplane/doc.go` forbids in as many
words — *"Be more permissive than the enforcing layer. The agreement test
treats an allowed-but-refused verdict as a failure, because that is the bug that
produces a dead button"* — and it is worse than an ordinary dead button,
because the operator it is dead for is the one person in the room with a motive
to keep clicking it.

**Why three separate guards all missed it.** Each for its own structural reason,
which is why it survived:

- `TestVectorsAreNotVacuous` asserts every `Reason` occurs somewhere in the
  corpus. `SELF_APPROVAL` did occur — always from the proposer arm. A
  coverage check on *outcomes* cannot see an unreached *cause* of a reached
  outcome.
- The Go↔TypeScript agreement test drives the same fixtures, so both
  implementations agreed about a case neither one ran.
- `TestNoStandingRoleCanApprove` skips kinds whose approve permission is
  standing, and `break_glass:approve` is standing by deliberate design — so
  it skips exactly the one kind carrying the flag.

This is a variant of a class already named twice in this document: a path only
tests can walk looks finished from inside the tests. Here the path was one no
test could walk, and the assertions were all about somewhere else.

**Fix.** `ApproverIsNotTarget` is exported on `adminplane.ActionKind` and
populated from the spec, so `authority.json` carries it. `actionView` gained a
`TargetID`, and `vectorActions` emits a `proposed_for_other` case for every kind
whose spec sets the flag, targeted at a principal the corpus actually has. The
console's port gained the matching branch. Both goldens were regenerated.

**Evidence.** `TestDecideApprove_TheTargetOfAnElevationCannotApproveIt` drives
the grantee (refused, `SELF_APPROVAL`), a third principal with identical
entitlements (**allowed** — the control that separates a corrected affordance
from a broken one), a non-user `target_id` (allowed, the shape every fixture used
to have), and a kind whose spec does not set the flag, elevated so it clears the
dual-control permission check rather than passing on `MISSING_PERMISSION`.

Observed failing with the branch disabled: `ALLOWED` where `SELF_APPROVAL` was
expected. The console's copy was disabled the same way and the agreement test
failed on the regenerated vectors — which is the point of the whole exercise,
because before this it could not have.

`TestTheVectorsReachTheTargetBranch` asserts the corpus still contains such a
case, so the check cannot quietly become unreachable again.

The regenerated corpus now carries 8 `SELF_APPROVAL` verdicts reached through
this branch, on `BREAK_GLASS_GRANT/proposed_for_other`, for principals whose
`subject_id` is the action's `target_id`.

## F-72 · An expired gate still read as live to the agent worker · NEW · P1 · FIXED

**Found by** the killswitch/eligibility audit; listed in F-69's inventory as a
divergence between two readers, deferred there on the grounds that deciding
which reader is authoritative was a design call. On checking it against the
source, it is not: the mandate is to fail closed, and the stricter reader is the
one that does.

**What was found.** `internal/agent` may not import `internal/gates` — an
agent tree can never reach the gate controller — so it reads
`capability_gates` with a SELECT of its own. That reader selected `effective_at`
and `expires_at` and then decided on `state` alone:

```go
err := q.QueryRow(ctx,
    `SELECT state, effective_at, expires_at FROM capability_gates ...`).
    Scan(&state, &effectiveAt, &expiresAt)
...
if state != "ACTIVE" { return false, state, nil }
return true, state, nil
```

Both window columns were fetched and neither was read. `gates.Evaluate` calls
the same row inactive once `expires_at` has passed, once `effective_at` is unset
or still in the future, and once `revoked_at` is set — four conditions the
agent-side reader did not have.

**Why it lasted.** `Admin.ExpireDue` moves an expired gate to EXPIRED, and its
own doc says it is "meant for a periodic worker". It has no non-test caller.
Nothing sweeps, so the persisted `state` never catches up on its own, and the
divergence lasted for as long as the row sat there rather than until the next
sweep. What it permitted was live agent trading past the end of its approval
window — the exact thing the window exists to stop, and the reason this is
P1 rather than P2.

`LiveTradingEnabled` also had **no test of any kind**, which is the other half of
the answer.

**Fix.** The reader applies every condition that can change after activation
without a state transition: revocation, `effective_at` unset or in the future,
`expires_at` passed. It returns a `LiveTrading` value carrying the state **and**
the refusing reason, because a gate whose window has closed still reads
`state=ACTIVE`, and an operator told only that would go looking for the wrong
thing. `cmd/agent-worker` logs both.

The quorum and evidence conditions `Evaluate` also applies are deliberately not
re-checked, and the code says why: `Activate` enforces them before it writes
ACTIVE, and migration 00701 leaves `cp_app` no UPDATE on any column of this table
except `version`, so no approver, evidence reference or window can be moved
underneath the reader by the application at all.

**Evidence.** `TestIntegration_TheAgentGateReaderAgreesWithTheAuthority` drives a
real proposal, approval and activation by three distinct principals, then
compares `Evaluate` and `Store.LiveTradingEnabled` against the same row at the
same instant across every condition: window open, `expires_at` passed (by moving
the CLOCK, not the row — which is how it happens in production: nobody edits
anything and the gate simply outlives its approval), `effective_at` in the
future, `effective_at` NULL, `revoked_at` set while the state still says ACTIVE,
open again after each case restores, and revoked through the workflow. It asserts
the two readers agree whatever the answer is, so a change that makes them differ
in the other direction fails here too.

`TestIntegration_TheAgentGateReaderFailsClosedWithNoRow` covers the absent gate.

Observed failing with the four conditions disabled: *"the authority says
active=false (expires_at has passed) and the agent reader says enabled=true"*,
and the same for the other three.

The test lives in `internal/gates` rather than `internal/agent` because
`test/security`'s boundary check counts test files, so the agent tree may not
import the gate package even in a test. The direction that is allowed is that
one.

## F-73 · Revoking an agent did not stop the runs it already had open · NEW · P2 · FIXED

**Found by** the agent-authority audit; listed in F-69's inventory.

**What was found.** `PGDispatcher.dispatchAgent` asks `a.Runnable()` before
opening anything, so a REVOKED, FAILED or SUPERSEDED agent gets no **new** runs.
That is correct and was already built.

`Runner.prepare` loads the same agent row, takes `stage` and
`risk_policy_version` from it and discards the rest; `ListOpenRuns` selects on
the run's status with no predicate on the agent. So every run already open ran
through to its intent.

Pause reaches an in-flight run at five separate points — at the start, and
again before every broker call, before the prediction and before the intent
— which is the right shape and is what made this easy to miss. The lifecycle
control an operator reaches for in an incident is the terminal one, and that was
the one that did not reach.

**Fix.** `Runner.Run` asks the same predicate the dispatcher asks, in the same
position relative to the pause check, so the two cannot diverge: one predicate,
two callers. Migration 00725 adds `AGENT_NOT_RUNNABLE` to the `agent_runs`
skip-reason CHECK. It covers all three states with the state itself recorded in
`agent_runs.error`, rather than three near-identical enum values that every
reader would have to know to treat alike. The existing reasons would each have
been a lie: `AGENT_PAUSED` names a different control, and `MISSING_DEPENDENCY`
(the fallback for an undeclared reason) would send an operator to look at data
feeds.

**Evidence.** `TestIntegration_ARevokedAgentStopsARunAlreadyOpen` drives a real
`Revoke` and a real `Fail` through the lifecycle, then runs the open run: the run
ends SKIPPED{AGENT_NOT_RUNNABLE}, the recorded error names the state, the provider
adapter is never dialled, the evaluator is never called, and **no prediction and
no intent exist** afterwards.

`TestIntegration_ARunnableAgentStillRuns` is the control that matters more: a
check one condition too wide would pass every case above and stop the platform's
agents altogether, which is the more expensive failure of the two.

Observed failing with the check disabled: the revoked agent's run reached the
evaluator and produced its prediction.

## F-74 · Three tests named for a comparison they never made · NEW · P2 · FIXED

**Found by** the F-73 work: adding a skip reason broke
`TestSkipReasonsMirrorTheDatabaseCheck`, and reading it to see what it wanted
showed that what it wanted was a number changed. The three names quoted in
this finding do not exist any more; they are what the tests were called
before it.

**What was found.** Three tests were named for a comparison against the schema
and none of them opened a database:

```go
// TestSkipReasonsMirrorTheDatabaseCheck and the two like it do not exist any
// more. This is what they were.
func TestSkipReasonsMirrorTheDatabaseCheck(t *testing.T) {
    require.Len(t, SkipReasons(), 10)
    for _, r := range SkipReasons() {
        assert.True(t, r.Valid())      // Valid() means "is in SkipReasons()"
    }
}
```

A hardcoded length, and then that every member of a list is a member of that
list — which holds however far the CHECK constraint has drifted. The same
shape in `TestPauseReasonsMirrorTheDatabaseCheck` and in `internal/prediction`'s
`TestModesMirrorTheDatabaseCheck` — names that do not exist any more, for
the reason this finding gives. A fourth,
`TestModeMappingMirrorsTheDatabaseCheck`, compares `ModesForStage` against a
table typed out inside the test — a third copy of the mapping, compared
with the second, while `agents_check3`, the constraint that actually refuses the
write, went unread.

Five rows of `REQUIREMENTS_TRACEABILITY.md` cited these as the evidence that Go
and the schema agree about modes, skip reasons and pause reasons.

The failure they were meant to catch arrives at the worst possible moment: a
value declared in Go and absent from the CHECK is a row that cannot be written at
all, and the constraint violation lands exactly when the thing being recorded has
already gone wrong — a skipped run, a raised pause, a halted market. The
other direction is quieter and worse: a value the database accepts that no switch
in the code handles.

**How wide it is.** The schema holds **163** enum CHECK constraints. Before this
finding, **none** was compared against a Go declaration by any test.

**Fix.** `test/integration/enums` compares 32 constraints against the 20 Go lists
that declare them — including all twelve `mode` columns — and
`agents_check3` against `ModesForStage`, parsing the constraint's stage-to-mode
structure out of `pg_get_constraintdef` rather than trusting a copy.
`TestTheThreeModeListsAgreeWithEachOther` keeps `agent.Modes`, `intent.Modes` and
`prediction.Modes` in step with one another, which comparing each against the
schema separately would not catch.

The pairing is written out by hand and not derived. Pairing by "these two sets
happen to be equal" would pass by construction and prove nothing — the same
defect in a new place.

`TestIntegration_NoEnumCheckAppearsUnnoticed` names the **131** enum CHECKs still
unpaired, so a new enum column fails the suite until somebody decides whether it
has a Go counterpart worth comparing against. That is the decision nobody made
for the 131. The list is an inventory, not an allow-list: the right number is
zero, and adding a name to it is admitting a second copy of a list with nothing
keeping the two in step.

The two tautological unit tests keep the part a test with no database can
establish, and are renamed for it — `TestSkipReasonsAreDeclaredAndClosed`,
`TestPauseReasonsAreDeclaredAndClosed`, `TestModesAreDeclaredAndClosed`. The five
traceability rows now cite the test that makes the comparison.

**Evidence.** Observed failing three ways. A value added to `allSkipReasons` with
no migration: *"agent.SkipReasons() and agent_runs_skip_reason_check have
diverged"*. A name removed from the unpaired inventory: *"an enum CHECK appeared
or disappeared without a decision about its Go counterpart"*. And
`stageModes[StageShadow]` given `ModeLive` — a change that would let a
SHADOW agent run in LIVE mode: *"ModesForStage(SHADOW) and agents_check3 have
diverged"*.

**One thing this surfaced and did not fix.** `admin_actions.kind` has **no CHECK
constraint at all**, while every other enum column in the schema has one:
`admin.Kinds()` is 19 values the database has never heard of. The Go layer
refuses an unknown kind at `Propose`, and an unknown kind reaching the executor
finds no executor, so the blast radius is a junk row rather than an unauthorised
action. It is recorded here rather than fixed because adding the CHECK creates an
obligation — a new admin action kind becomes a migration — and that
is a decision about how the kind list is meant to evolve, not a patch.

## F-75 · The emitter's envelope reader was optional · NEW · P3 · FIXED

**Found by** the agent-authority audit; listed in F-69's inventory, deferred
there because "its test needs the runtime the finding is about". That was wrong:
a constructor that must refuse a nil dependency is testable with no runtime at
all, which is what the fix does.

**What was found.** `NewBroker` refuses six nil dependencies. `NewEmitter`
refused four and let `EmitterDeps.Envelope` be nil, and `Emit` guarded the whole
envelope block on it:

```go
if e.deps.Envelope != nil && e.authority.EnvelopeID() != "" {
```

A caller that omitted the reader lost the instrument allow-list and the
single-trade cap, at any stage, while the agent's own frozen authority still
carried an envelope id. The two conditions in that line are not alike: an empty
envelope id is a legitimate state — the stages below CANARY carry no
envelope, and `agents_check2` is what says so — while a nil reader is a
wiring mistake being treated as a decision.

**Honest scope.** The path is inert. The only production `EmitterFor`, in
`cmd/agent-worker`, refuses outright with "the intent emitter is wired by the API
composition root; this worker proposes no intents yet", and the only other caller
is a test that passes the reader. Nothing is currently unguarded. This is
hardening ahead of the wiring, and it is worth doing exactly because the wiring
has not happened: a fail-open default is at its most dangerous in the window
between "nothing calls it" and "something does".

**Fix.** `NewEmitter` refuses a nil envelope reader, and `Emit` now guards only
on the envelope id, with the reason in the code.

**Evidence.** `TestNewEmitterRefusesEveryMissingDependency` accepts the complete
set first — without that control every refusal below it could be passing
for some other reason — then blanks each of the five dependencies in turn
and requires a VALIDATION_FAILED naming the missing one.

## F-76 · Six route guards that look like the API's authorization · NEW · P3 · FIXED

**Found by** the auth/identity audit; listed in F-69's inventory.

**What was found.** `internal/auth/httpmw` exports `RequireAuth`, `RequireRole`,
`RequireRoleAt`, `RequirePermission`, `RequirePermissionAt` and `RequireStepUp`,
and its package doc presented them as the authorization layer. The API mounts
none of them: every reference outside the package is in the package's own test
file. Authorization is decided in `internal/httpapi/authz.go`, per operation,
from the generated operation id.

That layer is the better design and is not in question here — a route
added to the OpenAPI document cannot be left unguarded by forgetting to wrap it,
which is exactly the failure a middleware chain invites. The finding is the doc:
a reader following it would believe the guards are live, and six exported guards
that look live are an invitation to mount one beside the real enforcement layer.
Two authorization paths over one route is how a route ends up guarded in only
one of them.

**Fix.** The package doc says which layer enforces, in those words, and says the
guards are kept because they are the right shape for a second server rather than
because anything uses them.

**Evidence.** `TestDocs_TheHTTPMiddlewareGuardsAreStillUnused` checks both
halves: no reference to any of the six outside the package, and the doc still
carrying the sentence that says so — without the second half the check
would pass on a doc that had quietly gone back to advertising them. It carries
its own positive control (`httpmw.Session`, which `cmd/api` certainly mounts),
because a broken walk would report every guard unused and agree with any doc at
all.

It looks for the qualified form, `httpmw.RequireStepUp`, and it has to:
`RequireStepUp` is also a function in `internal/security` with real callers, and
a bare-name scan reports it reachable — the check would have passed for
the wrong reason.

Observed failing with `r.Use(httpmw.RequireAuth)` planted in
`internal/httpapi/server.go`.

## F-77 · A test suite that poisons its own broker · NEW · P3 · FIXED

**Found by** this session's integration sweep failing three Redpanda tests with
`INVALID_PARTITIONS: Number of partitions is below 1` — on calls that pass
a partition count of 1 and 3.

**What was found.** `createTopic` provisions a topic per test and nothing ever
removed one. The local broker had **134** leftover `test.bus.*` topics from
previous runs, 252 partitions in total, and had reached the point where it would
create no more. Deleting them made the suite pass unchanged.

The error is the interesting part. It names the partition count, and the
partition count in the failing call is correct, so it sends the reader to look
at the one thing that is not wrong. Three tests failed and two passed, which
reads like a flaky broker rather than a full one.

CI never sees this: every run gets a fresh container. It fails only for somebody
running the suite repeatedly on one machine — which is the person running
it most, and the person for whom a confusing failure costs the most time.

**Fix.** `createTopic` registers a `t.Cleanup` that deletes the topic. The
deletion logs rather than fails: the test it belongs to has already finished,
and a broker that cannot delete a topic is worth knowing about without turning a
passing test red at cleanup time.

**Evidence.** Measured directly: 7 topics before a full package run and 7 after,
where before the fix each run added 7. The suite passes on a clean broker and on
one it has already used.

## F-78 · A promotion licensed by a row that denied it · NEW · P1 · FIXED

**Found by** following F-69's inventory item about `agents.stage` and `mode` not
being bound to a transition row. The item was real and narrower than what was
actually there.

**What was found.** Migration 00690 binds `agents.state` to a row in
`agent_lifecycle_transitions` written in the same transaction, and its header
states the stake plainly: *"without this, a bare `UPDATE agents SET state =
'LIVE'` by the application role would move an agent onto real customer capital
leaving no evidence of the promotion, its approval or its gate evidence"*.

The binding compares one thing. `cp_flag_transition` records the row's
`to_state`; `cp_require_transition` raises AU001 unless that equals the NEW
state. Nothing looks at `from_state`, and nothing looks at `stage` at all.

Both promotion CHECKs on the transitions table open with `from_stage = to_stage
OR ...`, and they must: a pause or a resume keeps the stage and cannot be made
to carry promotion evidence.

Those two facts compose. In one transaction, holding nothing but the
application role's INSERT and UPDATE:

```sql
INSERT INTO agent_lifecycle_transitions (from_state, to_state, from_stage, to_stage, ...)
VALUES ('CANARY', 'LIVE', 'LIVE', 'LIVE', ...);          -- the lie is from_stage
UPDATE agents SET state = 'LIVE', stage = 'LIVE', mode = 'LIVE' WHERE id = ...;
```

`from_stage = to_stage` satisfies the first clause of both CHECKs, so no
`approval_id`, no `ir_hash`, no risk policy version or hash and no
`evidence_hash` are required. `to_state = 'LIVE'` is exactly what the state
binding demands. The agent is at LIVE, on the ladder, with an envelope, and the
only record of how it got there is a row saying it was already there.

**A second route needs no lie.** `agents_check` permits any stage while the
state is a side state, so a PAUSED agent's `stage` can be moved with no
transition row at all — and `Resume`, which needs only `agent:pause`, then
sets the state to whatever stage it finds. That is the escalation F-69's
inventory described, and it is the smaller half.

This was **observed committing** before the fix, from the application pool,
against an agent walked to CANARY through the real lifecycle.

**Fix.** Migration 00726 binds the EDGE rather than the destination. The flag
records `<from>><to>` and the constraint trigger compares it against
`OLD.col || '>' || NEW.col`, so a row licenses a change only if it says where
that change started. `stage` gets a binding of its own under a separate label.
The destination-only trigger is replaced rather than kept beside the new one:
two triggers raising the same SQLSTATE for one write would make every failure
ambiguous about which rule fired, which is F-53's lesson.

**Scope, stated rather than implied.** This covers `agents`. The other ten
bindings 00603 established compare the destination alone and have the same
shape; whether the same composition is reachable depends on each table's own
CHECKs, and answering that is a migration per table with its own exploit test.
It is recorded here rather than done on the strength of the analogy — the
analogy is what would make it a claim instead of a proof.

`mode` is deliberately not bound: `agent_lifecycle_transitions` has no `to_mode`
column to bind it to, and `agents_check3` pins mode to stage for every rung
except BACKTEST_ELIGIBLE, whose two modes both move simulated money only.

**Evidence.** `TestIntegration_APromotionCannotBeLicensedByARowThatDeniesIt`
drives the exploit above and requires AU001, then re-reads the agent and asserts
it is still at CANARY. `TestIntegration_ABareStageUpdateIsRefused` pauses through
the real lifecycle first, so the state is legitimately PAUSED and `agents_check`
is not what refuses, then moves the stage with no transition row.

`TestIntegration_AStageChangeWithAnHonestRowIsAccepted` is the control, and the
suite's existing ladder walk is the larger one: a binding one condition too
strict would refuse every real promotion, and the whole lifecycle suite passes
unchanged.

Both refusals were observed failing before the migration: *"An error is expected
but got nil — the application role promoted an agent to LIVE with no
approval and no evidence"*.

## F-79 · A plaintext PKCE verifier kept forever, under a migration saying otherwise · NEW · P2 · FIXED

**Found by** the auth/identity audit; listed in F-69's inventory as "a retention
job, which is operations work with no home in this tree yet".

**What was found.** `login_attempts` holds the OIDC `state`, the `nonce` and the
PKCE `code_verifier` in plaintext, plus the IP and user agent of whoever began
the login. Migration 00641 grants `DELETE` on it to `cp_ops` and says, in its
Down section, that the table "is transient and purged by the ops role instead".

Nothing deleted a row. Not a worker, not a script, not a scheduled task: every
login ever begun was still there in full, secrets included.

The secrets are single-use — `Complete` sets `consumed_at` under
`FOR UPDATE`, and `test/security/replay_test.go` proves a consumed attempt
cannot be replayed — so this is exposure rather than an authentication
hole. That is why it is P2. It is still a table of credentials and IP addresses
growing without bound, against a written statement that it does not.

**Where the job now lives, and why.** `cmd/audit-worker`. Adding a binary was
available and rejected: ADR-0003 lists the eight deployables as a decision and
puts "any additional binaries" out of scope, so a ninth is an ADR amendment
rather than a patch. Of the eight, this one already owns what the platform keeps
and for how long — it reads `CP_RETENTION_SECURITY_AUDIT_DAYS` to set the
archive's Object Lock window — so retention of records is the remit it
already has.

`audit-worker purge` runs one pass; `audit-worker run` runs one an hour. The
interval is fixed rather than configurable: retention is measured in days and
the pass is a single DELETE on an indexed column, so there is nothing to tune,
and one more knob is one more thing that can be set to a value meaning "never".

**The credential stays where it belongs.** `cp_app` holds SELECT, INSERT and
UPDATE on `login_attempts` and deliberately not DELETE, so an attacker holding
the application credential cannot erase the record of the logins they attempted.
The purge therefore needs `cp_ops`, and `CP_DATABASE_OPS_URL` is a new optional
variable rather than a new production requirement for every binary: making it
required in STAGING/PROD would hand `cmd/api` an operations credential it never
uses, against PART 100's rule that a binary loads only the secrets its role
permits.

`audit-worker` opens that pool before its first tick and **refuses to start
without it**. A retention pass that quietly does nothing is the defect this
finding is about, and the operator who deployed the binary asked for the purge
by deploying it.

**Retention.** `CP_RETENTION_LOGIN_ATTEMPT_DAYS`, default 2. The durable record
of a login is a `security_events` row of kind `login`, which the purge never
touches, so the investigative trail survives; what goes is the plaintext. The Go
layer also refuses anything under a 24-hour floor, because a configuration value
is one edit away from meaning "all of them", and an operator looking at a
login-flow anomaly is usually doing it the next morning.

**Evidence.** `TestIntegration_ExpiredLoginAttemptsArePurged` plants three
attempts — long expired, expired inside the window, and not yet expired
— and asserts only the first goes. It asserts the privilege first: the
application role's own DELETE is refused with 42501, which is what makes the ops
role necessary rather than merely conventional.
`TestIntegration_ThePurgeRefusesTooShortARetention` covers the floor.

Observed failing with the DELETE made a no-op: *"the purge deleted nothing at
all"* and *"purge-old-... survived the purge"*.

**What this did not fix.** `CP_RETENTION_SOCIAL_DATA_DAYS`,
`CP_RETENTION_MODEL_IO_DAYS` and `CP_RETENTION_OPERATIONAL_LOG_DAYS` are
declared, validated and read by nothing. Three of the six retention classes the
configuration announces have no enforcement anywhere. Two of the six do:
SECURITY_AUDIT sets the archive's Object Lock window, and RAW_MARKET_DATA sets
the ClickHouse TTL.

The three that do not are harder than this one and not the same shape: the
tables they would cover (`tool_invocations`, `model_calls`, the audit stream)
carry `forbid_mutation` triggers that refuse DELETE outright, so retention there
means partition management or archival-then-drop, not a DELETE with a WHERE
clause. That is a design decision about how an append-only financial record is
aged out, and it belongs in an ADR rather than in a purge command.

## F-80 · Two executors nothing had ever run · NEW · P2 · FIXED

**Found by** the admin dual-control audit; listed in F-69's inventory as four
kinds with no test at all.

**What was found.** Grepping for the constant **and** the literal across every
`*_test.go` found nothing for `ENVELOPE_AUTHORITY_CHANGE`, `WITHDRAWAL_APPROVE`,
`NATIVE_ASSET_DELIST` or `PAYOUT_MANUAL_REVIEW_RESOLVE`.

The first two execute nothing yet — their subsystems are unreachable,
which is F-34's territory and recorded there. The other two run real code
against real state, and one of them is the only Domain A kind that is
dual-controlled in both directions, because resolving a payout by hand decides
what happens to money somebody is waiting for.

An untested executor is worse than an untested branch. An executor is the thing
that turns two signatures into an effect, and nothing had ever run these.

`PAYOUT_MANUAL_REVIEW_RESOLVE` had a second reason for being untestable: the
Domain A harness built its payout service **after** the executor set, and
`DomainAExecutors` registers that executor only when it is given one. The kind
was not merely untested on that harness; it was not registered.

**Evidence, and what running them found.** Both tests failed the first time they
ran, on properties worth having:

*Delist.* An ACTIVE asset cannot be delisted at all. `statusTransitions` sends a
live asset through CLOSE_ONLY or HALTED first, so holders are either given the
chance to exit or the halt is a recorded decision somebody has to make. An
approval does not create an edge the subsystem does not have.
`TestIntegration_DelistingALiveAssetGoesThroughTheStatusTable` asserts the
refusal, the step-down, the delist, and that DELISTED is terminal.
`TestIntegration_ADelistTargetingSomethingElseIsRefused` covers a target that is
not an asset id, because the executor reads the action's target rather than its
params.

*Payout.* `Create` records a REJECTED request when the eligibility decision
denies, rather than returning an error, so the fixture built a rejected payout
and then failed to flag it for review: *"a payout cannot go REJECTED ->
MANUAL_REVIEW"*. The helper now asserts `decision.Sufficient()` — the
precondition everything below it depends on — and reports the eligible
amount and the reasons when it does not hold.

`TestIntegration_ResolvingAStuckPayoutTakesTwoPeople` drives the whole shape:
one signature does not execute, the proposer cannot approve their own however
elevated, a second person can, and only then does the payout move.
`TestIntegration_NoResolutionDeclaresAPayoutSettled` is the property the
executor's own comment names — the provider is authoritative for
settlement, and an operator who could assert it by hand could close a ticket by
claiming money moved. It tries SETTLED, PAID, COMPLETE and the empty string,
each fully approved so the refusal cannot be coming from the approval, and ends
with a declared resolution that does execute so the four refusals are not
passing for some other reason.

## F-81 · An attempt could carry two signing decisions · NEW · P2 · FIXED

**Found by** F-69's inventory, where it was recorded under F-67 and left.

**What was found.** `signing.Service.Sign` treats a decision as the attempt's
idempotency record: it calls `findDecision`, and replays what it finds instead
of inspecting and deciding again. The refusal path writes a row too, so a
rejected attempt replays its rejection. One decision per attempt is the model.

00300 gave `attempt_id` a plain index and no uniqueness, and `findDecision`
concedes it in its own SQL:

```sql
WHERE attempt_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1
```

"The latest decision" is a phrase that only makes sense if there can be more
than one, and check-then-insert with no constraint is how there comes to be:
two concurrent `Sign` calls for one attempt both find nothing and both insert.
The rows are immutable, so the loser is not corrected — it stays, and every
later read silently prefers whichever sorted last.

That matters more here than a duplicate row usually does. A decision carries
`inspected_tx_hash`, and F-67 made the replay path bind the bytes offered
against the bytes that decision approved. Two decisions for one attempt are two
different sets of approved bytes, with the binding comparing against whichever
row was written last.

**Fix.** Migration 00727 replaces the plain index with a unique one. It replaces
rather than joins it: a unique index serves every lookup the plain index served.

**Evidence.** `TestIntegration_AnAttemptCannotCarryTwoSigningDecisions` signs
once through the real service, then writes a second decision row for the same
attempt directly — which is what the losing side of the race does, with the
INSERT privilege the application role holds — and requires 23505. It then
signs again and asserts the replay still returns the one decision there is,
because a constraint that refused legitimate replays would be worse than the
hole it closes.

`TestIntegration_ADifferentAttemptStillGetsItsOwnDecision` is the control: a
constraint one column too wide would refuse the second attempt of a retrying
plan and stop execution altogether.

## F-82 · The rate limiter counted per replica, so the configured limit was never the enforced one · NEW · P2 · FIXED

**Found by** the per-service configuration audit (`internal/config/service.go`),
which set out to establish what each binary actually dials and found one client
that nothing anywhere constructed.

**What was found.** `internal/ratelimit` ships two stores. `MemoryStore` keeps
counters in the process. `RedisStore` shares them, with an atomic Lua
`INCR` + `PEXPIRE` so two replicas cannot both believe they were first.
`NewRedisStore` had no caller. `cmd/api` chose the memory store and passed
`failOpen = true`, with a comment explaining that a Redis outage must never take
the API down.

The comment was answering the wrong question. The cost was not what happens when
Redis fails; it was what happens when it is absent, which was every deployment:

```hcl
api_autoscaling = { min_capacity = 3, max_capacity = 12 }
```

Three tasks, each with its own copy of the counters, enforce the configured
limit three times over. `CP_RATELIMIT_GENERAL=600/1m` admitted 1800 requests a
minute at the floor and 7200 at the ceiling, and the number moved whenever the
service scaled. A limit that is not the limit is worse than a wrong limit,
because it reads as a right one: the configuration, the docs and the
`RateLimit-Limit` header all said 600, and none of them was describing the
system.

Two things kept it invisible. `REQUIREMENTS_TRACEABILITY.md` cited "Redis
integration tests" as evidence for R-180-1, and those tests are real and pass —
they prove the store works, not that anything uses it. And the unit tests build
one limiter, where every store behaves identically; the defect only exists at
two.

**Fix.** Where the counters live is now a stated deployment decision rather than
an unstated code one.

- `CP_RATELIMIT_BACKEND` (`memory` | `redis`) is required of `cmd/api` and of
  nothing else, because nothing else serves HTTP.
- `config.Validate` refuses `memory` for an HTTP binary in STAGING and PROD.
- `cmd/api` builds the store the backend names, `PING`s Redis before the server
  starts, and refuses to start if it does not answer. Redis is not optional once
  chosen: a limiter that silently became per-process would report the same
  numbers as one that had not.
- The Redis-backed limiter fails **closed**. This reverses the earlier comment
  deliberately. With one replica, failing open costs a limit that was
  per-process anyway; with three or more it removes the only limit there is,
  at precisely the moment something unusual is already happening.
- Redis became the first *conditional* dependency. `serviceDeps` answers "what
  does this binary always need", and the answer for Redis is "it depends on the
  configuration", so `Config.RequiresDependency` decides it instead. `Load`
  gained a second pass for the same reason: the value that decides is read
  before the value it decides about, whatever order the table is in.

**Evidence.** `TestReplicas_ProcessLocalCountersMultiplyTheBudget` is the defect
as a measurement: three limiters over three memory stores, a limit of 10, and 30
requests admitted. `TestReplicas_OneSharedStoreEnforcesOneBudget` is the same
arrangement over one store and admits 10.

`TestIntegration_RedisStore` extends that to the real thing: three clients,
three stores, one server, ten allowed out of sixty.

`TestReplicas_ASharedStoreOutageDoesNotBecomeAProcessLocalLimit` holds the
absence of a fallback open — every request still reaches the failed store, each
is refused, and the middleware answers 500 rather than serving the request. 500
and not 429, because the limit is not exceeded, it is unknown.

`TestRateLimitStore_*` in `cmd/api` cover the composition: production refuses
the process-local store, a missing or malformed `CP_REDIS_URL` stops startup, a
plaintext URL under `CP_REDIS_REQUIRE_TLS` is refused rather than silently
upgraded, an unreachable Redis fails the `PING` rather than the first request,
and no failure path returns a memory store. One of them asserts only that a
password never appears in an error, because go-redis quotes the URL it was
handed and startup errors are logged.

`TestService_TheAPINeedsRedisOnlyWhenItsCountersAreShared` is the configuration
half: the same binary in the same environment needs Redis or does not, according
to one value.

## Reconciling the provider workstream (F-83 – F-92)

Fifty-eight commits landed after checkpoint `80edf58`, deploying the service to
Render's free tier against Neon and Stripe's sandbox, and producing
`docs/operations/PROVIDER_ACTIVATION_CHECKPOINT.md`. That document was read as a
claim, not as evidence. Six read-only audits ran over the surfaces it covers,
every claim they reported was re-verified against the source before anything was
changed, and the live deployment was probed directly.

**What the checkpoint got right, said plainly.** The signature verification is
correct in every particular a reviewer would check: constant-time comparison,
tolerance bounded on both sides, the signed payload exactly `<t>.<raw>`, multiple
`v1=` candidates handled with no early exit, every malformed shape refused.
Replay dedup is enforced by database constraints, not by a check-then-insert.
The exactly-once property holds across duplicate delivery, concurrent delivery,
and a crash at either boundary. Credit quantity is derived server-side and event
metadata cannot redirect or inflate it. Fail-closed under database unavailability
holds on every path traced: a dead Neon connection is an error, never
`ReasonNoGateRow` and never "not at capacity". The `CAPTURED` regression the
document reports is genuinely fixed and genuinely tested. The five-condition gate
ceremony is re-derived in SQL as well as in Go.

**What follows is what did not survive.**

## F-83 · The lint gate was red, and the mechanism it removed was still there · NEW · P3 · FIXED

`make lint` failed at `16cba60` at its first step. Eight files were not
gofumpt-formatted, `staticcheck` reported `cmd/api.stringEnv` unused, and
`golangci-lint` reported two `gosec` G101s and a `revive` argument-order issue.
The repository's standing discipline is that every commit is gated on `make lint`
and `make unit`; fifty-eight commits landed without it. The checkpoint says
"`go test ./...` green", which is true and is narrower than a reader will take it.

More than tidiness: `stringEnv` and `durationEnv` are the helpers that read
configuration straight from the environment. The workstream's own defect #5 was
that ten variables did exactly that and were therefore outside `configcheck` and
outside the configuration hash. The variables moved; the mechanism stayed, with
`durationEnv` kept alive by its own test. Both are deleted — a helper kept
alive by a test is an invitation to use it again, and the next use would be
invisible to the hash in the same way.

## F-84 · Validation runs before authentication · NEW · P3 · PART

Checkpoint §9 records that `/v1/payments`, `/v1/credits/balance` and
`/v1/payouts` "each 401 without a session". Probed against the live deployment:

```
GET /v1/credits/balance            400  "Query argument account_id is required"
GET /v1/payouts                    400  "Query argument account_id is required"
GET /v1/credits/balance?account_id=<uuid>   401
```

The generated `ServerInterfaceWrapper` binds and validates parameters before the
strict middleware where `authorize` runs, so an unauthenticated caller reaches
the validator. The parameter names are in the published OpenAPI document, so
what leaks is not secret; what is wrong is the readiness document asserting a
behaviour the deployment does not have, in the section a reviewer would read to
decide whether the surface is closed.

Recorded as PART rather than fixed: moving authorization ahead of parameter
binding means authorizing on the chi route pattern before the generated wrapper
runs, which is a change to the boundary's structure and wants its own design.
The document is corrected to say what the deployment does.

## F-85 · The body is buffered before the rate limiter · NEW · P2 · FIXED

`captureBody` is middleware position 233 and `rateLimit` is position 235, so
every request — including the ones the limiter would refuse — has up to
`CP_HTTP_MAX_BODY_BYTES` (1 MiB) read into memory with `io.ReadAll` first, and
then copied again into a `bytes.Reader`.

On the deployed topology that matters more than it usually would: one Render free
instance, 512 MB, no horizontal capacity. An unauthenticated caller can force
megabytes of allocation per second before any application-level limit applies.

**Fix.** `rateLimit` now runs ahead of `captureBody` (`internal/httpapi/server.go`),
so a request the limiter refuses is refused before its body is read.

This entry said "left open deliberately" for one batch, and the reason it gave
was real: the ordering is load-bearing in the other direction too — `captureBody`
is what makes the raw body available to the webhook signature check, and the
limiter's key depends on the principal the authenticator attaches. Reordering
middleware on the money path was not a change to make at the end of a batch, so
it was made at the start of the next one, with the adversarial body-limit suite
in `internal/httpapi/bodylimit_test.go` written first.

## F-86 · The payload-hash binding stopped at PROCESSED · NEW · P2 · FIXED

Checkpoint §3: "An event id reused with different bytes is refused with 400
rather than accepted as a repeat." True only when the first delivery reached
PROCESSED.

`Inbox.Once` calls `checkHash` in the two `StatusProcessed` branches. The
`StatusFailed` branch relocks, re-checks for PROCESSED and RECEIVED, and then
calls `i.run` with no comparison at all — and `markFailedInboxSQL` did not
write `payload_hash`, so a row created straight into FAILED had nothing to
compare against.

So an event whose first delivery failed could be re-delivered under the same id
with different bytes and be processed from them, while `provider_events` —
immutable since F-56, written by the first delivery, `ON CONFLICT DO NOTHING`
— went on recording the first delivery's `payload_hash` and `raw_ref`. The
effect and the evidence would disagree, on the table this system argues with a
provider from. It needs a valid signature, so it is Stripe or a secret-holder
rather than an anonymous forgery, which is why it is P2.

**Fix.** `MarkFailed` takes the payload hash and records it, with `COALESCE` on
the conflict path so the FIRST payload seen under an id stays the identity;
`markProcessedSQL` likewise prefers the stored hash over the incoming one; and
the FAILED branch calls `checkHash` before it re-runs.

## F-87 · A planted callback signed the victim in as the attacker · NEW · P1 · FIXED

`internal/auth/identity.go` says "state binds the callback to the browser
session". Nothing implemented that. `GetAuthLogin` discarded `res.State` and set
no cookie; `Complete` found the attempt by `state` alone; the session cookie was
then set in whichever browser made the callback request.

That stops a callback being **replayed** — the attempt is claimed once under
a row lock, which `test/security` proves. It does nothing about one being
**planted**:

1. the attacker calls `/v1/auth/login` and authenticates at the identity provider
   as themselves, keeping `code=C&state=S` without following the redirect;
2. the victim's browser is induced into a top-level navigation to
   `/v1/auth/callback?code=C&state=S` — a link is enough, and `SameSite=Lax`
   permits `Set-Cookie` on exactly that;
3. the victim is silently signed in as the attacker.

Everything the victim then does lands in the attacker's account: a Credit
purchase on the victim's card, identity documents, a payout destination. The
audit trail attributes all of it to the attacker's subject, which is exactly
backwards.

**Fix.** `SetLoginState` writes a short-lived `__Host-` cookie carrying
`base64url(sha256(state))` — the digest, because a cookie is readable by
anything that can read the browser's storage and the raw state is the key to a
pending attempt row. The callback requires it to match, in constant time, before
`Complete` runs, so a planted callback does not consume the attacker's attempt
either. `SameSite` is Lax for the same reason the session cookie is: Strict would
not be sent on the identity provider's top-level redirect, and would break every
login rather than only the planted ones.

**Evidence.** `TestAPlantedCallbackDoesNotSignAnybodyIn` drives the attack: a
browser with no cookie is refused, a browser with a cookie for a different state
is refused, and the browser that began the flow still completes it. Observed
failing with the guard disabled: *"a callback that did not begin in this browser
signed somebody in"*. `test/security`'s
`TestReplay_APlantedCallbackCannotSignAnybodyIn` runs the same attack over HTTP
against a real database and additionally asserts the refused callback consumed
nothing.

The suite's own login helper had to change to carry the cookie between the two
requests — which is to say the security suite had been logging in the way
the attack does.

## F-88 · One rate-limit bucket for everybody · NEW · P1 · FIXED

`principalKey` fell back to `ratelimit.ByRemoteIP`, which reads `r.RemoteAddr`
and nothing else. Its comment says "after trusted proxy handling upstream"; no
such handling existed. `clientIP` **is** proxy-aware and reads
`CP_HTTP_TRUSTED_PROXY_CIDRS`, and it was used only for audit records.

Render terminates TLS at its edge, so `RemoteAddr` is Render's and identical for
every caller. `CP_API_RATE_LIMIT_AUTH: 30/1m` was therefore **one bucket for the
whole deployment**: any unauthenticated client issuing 31 requests a minute to
`/v1/auth/login` returns 429 to every user's login for the rest of the window,
and an attacker's brute-force attempts are counted against the crowd rather than
against the attacker. `CP_HTTP_TRUSTED_PROXY_CIDRS` was not set, and setting it
would not have helped, because the rate-limit path never read it.

**Fix.** The limiter keys on `clientIP`, so the limiter and the audit trail agree
about who is calling. `config.Validate` gains `TRUSTED_PROXY_DECLARED`: a
STAGING or PROD deployment must name the networks its balancer speaks from,
because without them every caller looks like the balancer. `render.yaml` declares
the private ranges, with the reason it is safe to trust them written down —
nothing routes to that container except through the platform, and a caller
arriving from a public address is not in those ranges, so its header is ignored
and it is keyed on where it really came from.

**Evidence.** `TestRateLimitKeyDistinguishesCallersBehindAProxy` asserts two
callers through one balancer get two buckets, that the same caller is one bucket
whatever ephemeral port the balancer used, that an untrusted caller cannot forge
its key, and that an authenticated caller is keyed by subject. The new validation
rule was observed firing against the real blueprint before `render.yaml` was
changed.

**Note for the operator:** `CP_HTTP_TRUSTED_PROXY_CIDRS` is in the configuration
hash, so the deployed hash changes with this. That is the mechanism working.

## F-89 · A configuration variable that bounded nothing · NEW · P2 · FIXED

`CP_AUTH_STEP_UP_MAX_AGE` is parsed into `Auth.StepUpMaxAge`, validated `> 0`,
and read by **nothing**. Every step-up window in the process is a hard-coded
constant: 15 minutes at the HTTP boundary and in `internal/gates`, per-kind in
`internal/admin`. `render.yaml` sets 5 minutes, so the enforced window was three
times the configured one, and an operator tightening it further changed nothing.

**Fix.** The boundary enforces the tighter of its constant and the configured
value. Taking the minimum rather than the configured value outright is
deliberate: a deployment may make every window stricter, and may not use this
variable to widen one the code chose for a sensitive action.

**Not fixed here:** the domain packages still carry their own constants, and
`gates.Admin.Propose` enforces no step-up at all in the domain layer — the
proposer's step-up exists only at the HTTP boundary, so a non-HTTP caller holding
`gate:propose` proposes with no recent strong authentication. Recorded below.

## F-90 · The ceiling that could only ever rise · NEW · P1 · FIXED

`internal/capacity` counts CAPTURED and REVERSIBLE as money at risk and excludes
SETTLED, and says why: counting SETTLED "would turn the ceiling into a lifetime
cumulative cap that can only ever rise, so the tier would end up refusing every
purchase forever — an outage, not a ceiling."

The only exits from REVERSIBLE are `SettleDue` and a won dispute. `SettleDue`'s
only caller is `cmd/reconciliation-worker`. `render.yaml` deploys one service, a
web service, and says so: "No workers and no cron jobs. Both are paid service
types on Render." So nothing settled, the sum was monotonically non-decreasing,
and the deployment would have refused **every** Credit purchase with
`AT_CAPACITY` — permanently — at $2,000 of lifetime sales. The
reasoning behind the exclusion was right; the deployment removed the component
that made it true. `LAUNCH_TIER.md` calls the ceiling "a revenue signal"; on that
topology it was a terminal state.

An abandoned checkout is the same shape: a `CREATED` funding counts as at risk
and nothing cancels a stale one.

**Fix.** The API process runs the settlement sweep on a ticker, once at startup
and every 15 minutes. `cmd/api/creditsettle.go` states what makes that
acceptable: the sweep is idempotent, takes its rows `FOR UPDATE SKIP LOCKED` so a
worker tier added later needs no coordination with it, holds no provider call and
no lock across a network hop, and is never the only reason a number is correct.

`CP_CREDIT_SETTLEMENT_WINDOW` moves from an environment read in the worker into
the configuration table, because it is a recorded risk determination and a value
read from the environment is outside `configcheck` and outside the hash —
the workstream's own defect #5, in a variable it did not reach.

**Evidence.** `TestIntegration_SettlementDrainsTheMoneyAtRiskCeiling` measures
the ceiling before a purchase, after it, after a settlement pass that finds
nothing because the window is open, and after one that settles. The middle
assertion is the one that matters: without it the test would pass against a sweep
that settled everything the instant it was captured.

## F-91 · The launch cohort admitted everybody · NEW · P1 · FIXED

`CP_CAPACITY_MAX_ACCOUNTS: 50` is read, validated, and logged at startup as
"launch-tier capacity ceilings in force". `capacity.ActionOpenAccount` had **no
reference outside `internal/capacity`** and its own tests. Accounts are
auto-provisioned on the first successful OIDC login, so the 51st authenticated
user got one, and so would the five-thousandth.

The guard was also built inside `wireCreditPurchase`, so a deployment with no
payment provider had no ceilings at all — including the one about accounts,
which has nothing to do with selling Credits.

**Fix.** The guard is built once in `build()`, before identity, and given to
both. `identity.Deps` gains `AdmitAccount`, asked before the account row is
written so a refusal leaves no half-provisioned user. It is a function rather
than a `capacity.Guard` so `internal/identity` keeps no dependency on the
ceiling's implementation.

**Evidence.** `TestEveryCapacityActionIsAskedAboutSomewhereReachable` reads the
`Action` constants out of the package and requires each to be referenced by
non-test code outside it. Observed failing with the wiring removed: *"capacity.
ActionOpenAccount is declared and nothing outside internal/capacity ever asks for
it, so the ceiling it names is configured, logged and unenforced."* It is the
same question `test/reachability` already asks of methods that move money, aimed
at a control, and it found the same answer.

## F-92 · Two ceiling tests that needed the database to already have rows · NEW · P2 · FIXED

Checkpoint §8: "All seven `internal/capacity` integration tests pass against
Neon." Against a freshly migrated database, three subtests fail.

`TestIntegration_EachCeilingRefusesAtTheMeasuredValue` sets each ceiling from a
live measurement, on the stated grounds that this "is what makes this safe to run
against a live database: it writes nothing and still exercises the real number".
The instinct is good and it has a hole: when the measurement is **zero**,
`max64(0, 1)` asks the guard to refuse at a ceiling of 1 with nothing at risk,
and the guard correctly admits. The ceilings for accounts, daily purchases and
money at risk therefore only pass where rows already exist — which is why
they passed against Neon and fail on a clean database. That is a test that passes
because of what the environment happened to contain.

**Fix.** The test seeds one account and one CAPTURED funding inside a transaction
it rolls back, so the suite still writes nothing durable and the numbers it
exercises are ones it put there. It asserts the seed is visible to the
measurement before using it, so a seed that stopped working would fail loudly
rather than restore the old vacuity.

## F-93 · Inventory: the provider workstream, verified and not fixed · NEW · P1 · OPEN

Six read-only audits ran over the surfaces the provider workstream added:
configuration and environment enforcement, the webhook and evidence path, the
capacity guard, identity and ZITADEL, the Render/Neon topology, and the two
inventories §4 depends on. Every claim below was re-verified against the
source before being written down; the ones that were wrong are recorded as wrong
in the last section.

**Not fixed. Each was checked; the reason for leaving it is given, because "not
fixed" without a reason is indistinguishable from "not noticed".**

| What | Verified | Why not fixed here |
|---|---|---|
| ~~The capacity ceiling is not authoritative under concurrency~~ — **fixed as F-96** | `wiring_native.go` opens the purchase transaction `ReadCommitted` with no retries; `AdmitAmount` reads `sum(paid_amount_minor)`, which cannot see a concurrent uncommitted INSERT; there is no unique or exclusion constraint that could make the ceiling authoritative. `internal/credit/purchase.go` states the opposite in a comment: "The guard reads inside this transaction, so two concurrent purchases cannot both be admitted against the same headroom." **`db.Serializable` exists, its doc says "Use it for every financial state change (PART 22)", and it has zero production callers.** | The fix is entangled with the next row and must not be done separately. Raising isolation while an outbound Stripe call sits inside the transaction means a serialization retry re-calls the provider; taking an advisory lock instead holds one of eight pool connections across that call and queues every other purchase behind it. The provider call has to come out of the transaction first. |
| ~~The provider call is inside the purchase transaction~~ — **fixed as F-96** | `internal/credit/purchase.go` calls `s.provider.CreatePurchase` between `CreateFunding` and the transaction's commit, with a 20s provider timeout. The comment above it claims the funding row is "persisted BEFORE the provider is called"; it is written, not persisted, and a rollback erases it while the Stripe object survives. `cmd/reconciliation-worker/creditsweep.go` documents the opposite rule for itself — "a provider call inside a transaction holds a database connection across the network... The pool starvation that caused (F-27)". | A real restructure of the money path: `StartPurchase` takes a `pgx.Tx` from its caller, so splitting it into commit-then-call-then-record changes the port, the adapter and the replay path. It is the next thing to do on this surface and it is not a change to make at the end of a batch. The existing replay path (`if f.ProviderReference != ""`) already anticipates the shape. |
| ~~A ceiling set to zero means "unlimited", per ceiling~~ — **fixed as F-97**, at the configuration boundary rather than by changing the guard's semantics | `Measure` skips the query and `Admit` skips the comparison when a ceiling is 0; `NewGuard` and `config.Validate` refuse only when **all four** are zero. So `CP_CAPACITY_MAX_AT_RISK_MINOR=0` loads, validates, and logs "ceilings in force" with the money cap silently off. | Genuine fail-open, and the fix is a semantic decision rather than a patch: `0` legitimately means "no quota" for `MaxDatabaseBytes`, which `doc.go` documents. Making zero mean "refuse everything" for the other three needs the four to stop sharing one rule. |
| ~~A wrong Stripe account is a warning, not a refusal to start~~ — **the silent half fixed 2026-09-10** | `cmd/api/wire_credit.go` logs WARN and returns an empty wiring, so the webhook route 404s while `/v1/healthz` answers 200. The checkpoint claimed the opposite: "Naming it also makes a key rotated to the wrong account **a refusal to start**". It was the same "warning nobody reads plus a silently disabled capability on a service answering 200" the same document lists as its own defect #2. | The availability decision stands and is the code's own: a deployment that cannot sell Credits must still serve the rest of the product, and refusing to start on a provider check would turn every cold start during a Stripe blip into an outage. What was wrong was the *silent* half, and F-118 removed the reason it had to be silent: a configured provider that ends up disabled — wrong account, refused by the registry, no ceilings, service or webhook ingestion unbuildable — now carries its reason out of `wireCreditPurchase` and the composition root raises `credit_purchase_disabled` at SEV2 to the alert destination. An unconfigured provider, the LOCAL default, raises nothing. `TestADisabledCreditPathIsAPage`. |
| `CP_API_SETTLEMENT_MINT` is a Solana **devnet** USDC mint | `render.yaml` sets it and says so; `config.Validate` checks only non-emptiness; `cmd/api/wire.go` checks only that the pair resolves to a registered stablecoin in this database. | The checkpoint's "going live is two changes" is corrected to three. A validation rule that refuses a devnet mint in PROD needs a list of known devnet mints, which is a fact about Solana this repository should not invent. |
| ~~The JWKS cache has no maximum age~~ — **fixed as F-98** | `internal/auth/oidc/keyset.go` refreshes only when the cache is empty or a token names an unknown `kid`. A key removed from the JWKS keeps validating tokens for the life of the process. | A hard TTL is a small change and a real behaviour change to token validation. It belongs with a test that can advance the clock over the cache, which this batch did not build. |
| ~~`cmd/api` requires `CP_DATABASE_MIGRATE_URL`~~ — **fixed 2026-09-10** | Declared as an unconditional `req`; the only reader anywhere is `cmd/migrate`. So the internet-facing process is given the schema-owner credential, and the owner can `ALTER TABLE ... DISABLE TRIGGER` — which is the guard the evidence-immutability argument rests on. | Was deferred because a blueprint change and a redeploy were needed. **The value of fixing it rose sharply**: 00743-00753 moved every state machine in this system onto triggers, so the owner credential now undoes the transition bindings, `forbid_mutation` on fifty-three append-only tables and eleven state-writing triggers, in one statement. It is now `only(ServiceTooling, ...)` and absent from `render.yaml`, with `TestTheWebServiceIsNotGivenTheSchemaOwner` asserting both halves — proven by putting each back and watching it fail. |
| Three IdP identities satisfy the whole gate ceremony | Every distinctness check compares `security.Principal.SubjectID`, which is `users.id`, keyed by `UNIQUE (idp_issuer, idp_subject)`. There is no person entity and no uniqueness on email. | An identity-model decision, not a code fix — the same conclusion F-64 reached. What is new is that the checkpoint presents "three distinct principals" as the control that stands between this deployment and selling Credits, without saying that three subject strings are not three people. The document is corrected. |
| `email_verified` does not gate buying Credits | It gates Domain A (native assets, commerce, on-chain trade) through `NODAL_IDENTITY`, and payouts through `PAYOUT_KYC`. `StartPurchase` checks the capability gate and the ceiling and nothing about verification. | Whether an unverified account may pay money in — as opposed to take value out — is a product and compliance decision, not a defect. Recorded so it is decided rather than defaulted. |
| ~~`gates.Admin.Propose` enforces no step-up in the domain layer~~ — **fixed as F-99** | `Approve`, `Activate` and `Resume` call `requireStepUp`; `Propose` does not. The proposer's step-up exists only at the HTTP boundary. | Adding it is one line and changes a ceremony that is about to be performed for real. It wants its own test and its own commit, not a rider on F-89. |
| ~~`CP_RETENTION_LOGIN_ATTEMPT_DAYS` is 90 on a table designed for 2~~ — **fixed 2026-09-10** | `render.yaml` set 90; the table entry says "Minimum 1", defaults to 2, and explains the row holds "a plaintext OIDC nonce and PKCE verifier". It was also the one retention variable with no validation rule. And it was inert: the purge lived in `cmd/audit-worker`, which this tier does not deploy, and `CP_DATABASE_OPS_URL` was unset. | Both halves landed, and the second is what made the first matter. **F-105 put the purge in `cmd/api` and `CP_DATABASE_OPS_URL` is now set**, so the number stopped describing a sweep that never ran and started describing one that does. It is 2, with the reasoning in the blueprint: the secrets are single-use and the durable record of a login is a `security_events` row, which is why this variable's default is 2 while its neighbours are in the thousands — 90 was this line agreeing with them rather than with what the table holds. The documented minimum is now enforced, rather than a maximum being invented: how long single-use secrets are kept is a deployment decision, and the rule only refuses what the documentation already refuses. |
| The 48-name configuration backlog can grow | `test/infra` really does parse `cmd/` for `CP_*` literals and really does hold `cmd/api` to zero. It walks `cmd/` only, misses a name built by concatenation, and its "frozen" list is frozen only against accidents — adding a line to the slice makes it green. | The scan is honest about what it is; the sentence in the checkpoint is not. Widening it to `internal/` and to non-literal reads is a real improvement and a separate piece of work. |
| ~~Render replica count is an assumption, not an assertion~~ — **fixed 2026-09-10** | Three controls compared configuration against configuration; nothing observed the platform, and `cmd/api/ratelimitstore.go` said so in as many words. `render.yaml` sets no `numInstances`, so the dashboard is authoritative for it. | The fix this row named is the fix that landed: `pg_try_advisory_lock` on a session-scoped key, held for process life, taken only when the counters are process-local — a shared Redis store is what makes several instances correct, so locking there would refuse a topology that works. It **refuses to start** rather than warning, because a warning is the defect this register keeps recording and because two instances silently double every transport limit, including the ones in front of the money path and the unauthenticated webhook surface. On a one-service free tier, refusing a second copy loses nothing. `TestIntegration_TheSecondInstanceIsRefused` proves both halves: the second instance is refused, **and the lock is released so a redeploy is not locked out by its predecessor** — the half that would turn a safety control into an outage if it were wrong. |
| ~~Neon: no `MaxConnIdleTime`, no `ConnectTimeout`~~ — **fixed 2026-09-10** | `db.Config` exposed neither, so pgxpool's 30-minute idle default stood against a compute that suspends after about five. The first request after each suspension paid a dial-and-fail on a stale pooled connection, and a connection failure is not retryable by design. | Both knobs added, and the two values were arrived at differently — which is the part worth keeping. **`ConnectTimeout` is derived**: a dial that outlives the request it was opened for is work done for nobody, holding a pool slot the next request wanted, so `config.Validate` refuses any value at or above `CP_API_REQUEST_TIMEOUT` and the blueprint sets 5s under a 10s request budget. **`MaxConnIdleTime` is not derived and says so**: Neon does not publish the free tier's idle threshold and this repository still has no measurement, so 4m is a judgement written in the blueprint next to that admission, and the code default stays 0 — pgxpool's behaviour — for every deployment that has not measured its own. |
| The in-process provider circuit-breaker resets on restart | `internal/provider/health.go` holds `disabled` in memory and `NewTracker` always starts healthy, so an operator disable is lifted by a redeploy. | Latent on this deployment: `cmd/api` builds an empty registry and no admin route calls `Disable`. Live on the workers, which this tier does not run. Recorded against the day either changes. |

**Two claims from the audits that were wrong, recorded as such.** An audit
reported that `Dispatch` returning `Ignored` for an unresolvable provider
reference is a silent loss; it is a durable, committed `IGNORED` record with an
inbox row, which is the correct outcome for a foreign object on a shared account.
Another reported the archive-replay translation as untested; it is untested by
`go test ./...`, and it *is* covered by `test/deployed`, which CI does not run
— which is a different and more useful statement, and is recorded in the
CI row above rather than as a missing test.

## F-94 · A transition row's origin was never read · NEW · P2 · FIXED

This is §4's first item, recomputed. The count was recorded as ten; it is
**sixteen**, and the recount changed the finding rather than confirming it.

**What F-78 established, and what it did not.** 00726 bound the EDGE for
`agents` because the exploit was reachable there: both promotion CHECKs on
`agent_lifecycle_transitions` open with `from_stage = to_stage OR ...`, so a row
claiming the stage had not moved satisfied them and licensed moving it. The
other bindings were left alone because the analogy was not a proof.

The recount is the proof, and it says the exploit is **not** reachable on them:
`agent_lifecycle_transitions` is the only transitions table in the schema with a
`from_X = to_X OR ...` CHECK, and eleven of the sixteen have no CHECK constraints
at all. Recording that plainly matters as much as the fix — it is the
difference between a P1 and what this is.

**What is present on all sixteen** is weaker and still worth closing: **nothing
anywhere reads `from_status` or `from_state`.** Not a CHECK, not a trigger, not
a function. `cp_app` holds INSERT on every transitions table and UPDATE on every
entity, so a row recording an origin the entity was never in commits cleanly.
Demonstrated against the pre-migration schema, as `cp_app`:

```sql
INSERT INTO account_status_transitions (from_status, to_status, ...)
VALUES ('RESTRICTED', 'FROZEN', ...);          -- the account was never RESTRICTED
UPDATE accounts SET status = 'FROZEN' WHERE id = ...;
COMMIT;                                        -- committed; status is FROZEN
```

The trail now says the account passed through RESTRICTED. It did not. On a
system whose stated principles include evidence and provenance, the recorded
origin being unchecked is the finding.

**A second widening, found in the same recount.** 00712 replaced
`cp_require_transition` so the flag accumulates and the check is membership, with
a header stating "Membership is exactly as strong as equality was". It is not.
Under equality only the LAST flagged destination satisfied the check; under
membership any of them does. So two rows written in one transaction — A->B
and B->C, which is what submitting and launching a native asset in one call
writes — licensed a single UPDATE straight from A to C. Demonstrated on the
same schema: two honest-looking rows, one update, and the account arrives at
FROZEN having never been RESTRICTED.

That is not a privilege escalation, and the finding says so: a transaction that
can insert two rows can insert one. It is a defect the binding used to catch and
stopped catching, which the migration's own header claimed it had not.

**Fix.** Migration 00731 converts fifteen bindings to the edge form. The flag
records `<from>><to>` and accumulates; the check asks whether `OLD>NEW` is among
the edges flagged. So a row claiming nothing moved licenses nothing, a row
claiming a false origin licenses nothing, and A->B plus B->C license exactly
A->B and B->C. Accumulating EDGES keeps 00712's multi-step flows working and
gives nothing back, because an edge names both ends.

`kill_switches` is deliberately not converted, with the reason in the migration:
`kill_switch_transitions` has no `from_active` column and needs none, because for
a two-valued column the destination determines the origin. The destination form
is already edge-complete there.

**Evidence.**
`TestIntegration_ATransitionRowCannotDenyTheChangeItLicenses` drives all three
refusals — a row saying nothing moved, a row with an invented origin, and
two rows licensing a jump that skips the middle — plus one entity's row not
licensing another's change. Its three controls are the point: an honest single
step commits, an honest TWO-step transaction commits both steps, and the ladder
walks. A binding one condition too strict would pass every refusal and refuse
every real change.

`TestIntegration_EveryAuditedEntityBindsTheEdge` reads `pg_trigger` rather than a
list typed out in the test, so a table that gains a destination-only binding
tomorrow fails rather than joining the set quietly, and carries a positive
control that the edge form is actually in use.

## F-95 · Nine more enum lists are compared against the schema · NEW · P3 · PART

§4's second item. The inventory was recomputed after 00728–00730 and was
found exactly current — 130 unpaired, no name missing and none stale.

Every unpaired constraint was then matched against every exported Go enum list
by value set. Twelve matched exactly; **nine are genuine domain matches and are
now paired**: `ledger_accounts.code` (the most consequential enum in the system),
the two native-economy status tables, both payout enums, both reconciliation
enums, and the two `retention_class` columns.

**Three matched and were deliberately left unpaired**, which is the more useful
half. `data_sources`, `venues` and `venue_listings` all hold
ACTIVE/DEGRADED/DISABLED, which is exactly `agent.ToolStatuses()`. Pairing them
with it would be the "these two sets happen to be equal" mistake the registry
exists to avoid: a tool's health and a venue's listing status are different facts
that agree today by coincidence, and binding them would make a legitimate change
to one break the other.

**A positive result worth recording.** The same pass looked for constraints whose
domain HAS a Go list that disagrees with it — live drift. There is none. The
only near-match is `agents_check3`, which is the stage-to-mode mapping and
already has a bespoke comparison.

**121 remain unpaired**, and the honest reason is that most of them have no Go
counterpart to compare against: they are schema-only enums that no Go code
switches on. Manufacturing a hundred Go lists to pair with them would be
inventing structure to satisfy a test. The ones that matter are the ones a
switch statement depends on, and those are now covered.

## F-96 · A provider call inside the transaction, and a ceiling that was not one · NEW · P1 · FIXED

The two rows F-93 said had to be fixed together. They were.

**The ordering.** `StartPurchase`'s doc said the point of it was that "the
funding row and its idempotency key are persisted BEFORE the provider is
called, so that a lost response leaves a record to reconcile against rather
than a charge nobody knows about". Everything ran inside one transaction the
caller opened, with `s.provider.CreatePurchase` between the INSERT and the
COMMIT. The row was written before the call; it was not persisted before it.

A rollback — a request deadline, a killed instance, a dropped connection
during the 20-second provider timeout — erased the funding row and left the
Stripe object. The retry then made a NEW funding row, and Stripe's idempotency
returned the SAME object, still carrying the first funding's id in its metadata.
Every subsequent webhook delivery hit the metadata-disagreement check, which is
a hard error rather than a `review()`, so the transaction rolled back and the
answer was 500 — for three days, until Stripe gave up. Money captured,
Credits never minted.

It also held one of eight pool connections across a network call, which is the
starvation `cmd/reconciliation-worker` documents itself avoiding for exactly
this reason: *"a provider call inside a transaction holds a database connection
across the network... The pool starvation that caused (F-27) is the reason this
loop looks inefficient and is not."* The same repository, the opposite practice,
on the money path.

**The ceiling.** `internal/credit` said: *"The guard reads inside this
transaction, so two concurrent purchases cannot both be admitted against the
same headroom."* Under READ COMMITTED that is false. `AdmitAmount` sums
`paid_amount_minor`; a concurrent uncommitted INSERT is invisible to that sum;
there is no constraint on `credit_fundings` that could catch it afterwards. So N
transactions measure the same headroom and all N are admitted, and the window is
as wide as the provider call inside it.

`db.Serializable` exists, its doc says *"Use it for every financial state change
(PART 22)"*, and it has **zero production callers**.

**Why one fix and not two.** Raising isolation while the provider call is inside
the transaction means a serialization retry re-calls the provider. Taking a lock
instead holds a pool connection across that call and queues every other purchase
behind one HTTP request. Neither is safe until the call is outside.

**Fix.** `StartPurchase` takes a `Transactor` rather than a transaction and runs
three phases: commit the gate, the ceiling, the price and the funding row; call
the provider with nothing held; commit the provider reference and any state it
reported. The capacity guard then takes `pg_advisory_xact_lock` before it
measures — transaction-scoped, so it cannot be leaked, and sound only
because that transaction no longer contains a network call. One key per action,
so opening an account does not queue behind buying Credits.

A crash between any two phases leaves a funding row with no provider reference,
which is the state the replay path already handled: the same idempotency key
returns the same funding and the provider returns the same object.

**Evidence.**
`TestIntegration_ConcurrentPurchasesCannotAllPassOneCeiling` fires eight
simultaneous purchases of 10,000 minor units at a ceiling of 30,000 and requires
**exactly three** to be admitted, every refusal to carry `AT_CAPACITY`, and the
database's own sum to equal 30,000. Asserting the exact count rather than "some
were refused" is deliberate: the looser assertion passes against a guard that
refuses at random.

Observed failing with the lock disabled: *"the ceiling admitted 8 purchases of
10000 against a ceiling of 30000"*. The whole 51-package sweep passes with it.

## F-97 · A ceiling set to zero was a ceiling switched off · NEW · P2 · FIXED

`Measure` skips a query when its ceiling is 0 and `Admit` skips the comparison,
so zero means "this ceiling does not apply". That is right for a library:
`MaxDatabaseBytes` is genuinely zero on managed Postgres with no storage quota.

What it must not be is reachable by a one-character edit on a deployment that
takes money. `config.Validate` refused only the all-four-zero case, so
`CP_CAPACITY_MAX_AT_RISK_MINOR=0` loaded, validated, and produced a startup log
line reading `launch-tier capacity ceilings in force max_at_risk_minor=0`.

**Fix.** In STAGING and PROD the three ceilings about people and money must each
be stated. `MaxDatabaseBytes` is deliberately excluded, with the reason in the
code: a deployment with no storage quota has nothing to state.

**Evidence.** `TestValidate_AMoneyCeilingMustBeStated` sets each of the three to
zero in turn and requires the load to fail, and carries the control that a zero
database ceiling still loads — without it the rule could have been written
as "no ceiling may be zero" and looked correct.

## F-98 · A withdrawn signing key kept working · NEW · P2 · FIXED

The JWKS cache refreshed in exactly two situations: when it was empty, and when
a token named a `kid` it did not have. Both are right for **rotation**, which
introduces a new key and therefore a new kid.

Neither serves **revocation**. An operator withdrawing a compromised key removes
it and introduces nothing, so no unknown kid ever arrives, no refresh is
triggered, and the withdrawn key goes on verifying tokens for the life of the
process. It self-heals only by accident, at the next ordinary rotation. That is
the emergency case, and it was the one case the refresh policy could not serve.

**Fix.** A maximum cache age of fifteen minutes, so a set older than that is
re-fetched before it is used. Fifteen minutes is the window an emergency
revocation takes to land; the cost is one small HTTP request per process per
fifteen minutes.

A refresh that fails inside the window keeps serving the cached set, because a
briefly unreachable issuer should not stop every login. Past the window it does
not: "the provider is down" must not come to mean "the withdrawn key works
again".

**Evidence.** `TestKeySet_AWithdrawnKeyStopsVerifyingWithinMaxAge` publishes two
keys, withdraws one, and asserts three things: the withdrawn key still resolves
inside the window **and the set is not re-fetched** (so the test is about the
age and not about a cache that refetches constantly), it stops resolving past
it, and the key that is still published still resolves (so the expiry refreshed
the set rather than emptying it). Observed failing with the age check removed:
*"a key withdrawn from the JWKS still verified"*.

## F-99 · The first signature needed no authentication · NEW · P2 · FIXED

`gates.Admin.Approve`, `Activate` and `Resume` each call `requireStepUp`.
`Propose` did not. The proposer's step-up existed only at the HTTP boundary, in
`internal/httpapi/authz.go`.

So any caller that is not the REST surface — a worker, a script, an admin
CLI holding `gate:propose` — opened a proposal for a high-risk capability
with no recent strong authentication at all. `scripts/gateceremony` is the
existing non-HTTP caller; it is fenced to LOCAL/DEV/TEST and a loopback database
host, so it is not itself the hole, but it proves the path is reachable.

The first signature of a dual-controlled ceremony is a signature. It is also the
one that names the four evidence references, which makes it the step where the
external approvals enter the system.

**Fix.** `Propose` requires a step-up like every other step.

**Evidence.** `internal/gates`' own expiry test caught it immediately and
correctly: the renewal proposal it makes an hour after the ceremony began was
refused, because the proposer's authentication was an hour old. The test now
builds a freshly authenticated proposer, which is what the real ceremony
requires — the change makes the test describe the ceremony more accurately
than it did.

## F-100 · A funding parked for a human is un-parked by the next webhook · NEW · P1 · FIXED

**Found by** an independent read of the Stripe adapter and the funding state
machine, checking what MANUAL_REVIEW actually means to each caller.

`fundingTransitions[FundingManualReview]` (`internal/credit/funding.go:145-157`)
lists ten destinations. Its comment says why:

> An operator resolving a review may send the funding anywhere a provider event
> could legitimately have sent it -- with one exception. There is no resolution
> to SETTLED.

The reasoning about SETTLED is careful and right. The premise underneath it is
not: **`Dispatch` consults the same table.** `internal/credit/purchase.go:405`
calls `CanTransitionFunding(f.State, to)` for a provider event exactly as an
operator resolution would, and `apply` (`purchase.go:429`) then performs the
economic effect. Nothing distinguishes the two callers, so every destination
written for a human is also a destination the next webhook can take.

MANUAL_REVIEW → CAPTURED is one of them, and CAPTURED is the edge that mints.

**The sequence.** A funding is CAPTURE_PENDING.

1. `charge.refunded` arrives first -- Stripe does not order deliveries, and a
   fraud auto-refund can fire while the success delivery is being retried.
   `to = REFUNDED`; CAPTURE_PENDING → REFUNDED is not legal; the state is not
   terminal; `isBackwards` returns false because REFUNDED is absent from the
   rank map (`funding.go:513-521`). So `review()` parks it in MANUAL_REVIEW --
   and returns `webhook.Applied` (`purchase.go:504`), which marks the event
   PROCESSED in the inbox. **The refund will not be redelivered.**
2. `payment_intent.succeeded` arrives. `to = CAPTURED`. MANUAL_REVIEW →
   CAPTURED is legal, so `apply` runs `AdvanceFunding` then `MintFrom`.

Credits are minted for a payment that was refunded, and the event recording the
refund has already been consumed.

`charge.dispute.created` before `payment_intent.succeeded` is the same shape,
with `DisputeFunding`'s freeze (`purchase.go:449-455`) never running.

**And there is no operator.** `internal/admin/kinds.go` has no credit-funding
resolution kind -- compare `KindPayoutManualReviewResolve` at `kinds.go:94`.
`AdvanceFunding` and `DisputeFunding` have no caller outside `internal/credit`,
and the HTTP surface over fundings is read-only
(`internal/httpapi/ports_native.go:46`). The state whose transition table was
widened so that a human could resolve it has exactly one resolver, and it is a
machine.

**Reproduced against a real database.** The sequence above, driven through the
real services: `processing` → CAPTURE_PENDING, `charge.refunded` → parked in
MANUAL_REVIEW, `payment_intent.succeeded` →

```
    expected: "MANUAL_REVIEW"      actual: "REVERSIBLE"
    Expected nil, but got: &id.ID[credit.lotKind]{...}
      "Credits were minted for a payment that was refunded"
    expected: "0"                  actual: "10000"
      "the refund was consumed and the balance says the money is ours"
```

DISPUTED and CHARGEBACK events resolve a review the same way, which is the same
defect without the mint.

**Fix, first half.** `Dispatch` refuses to act on a parked funding at all. The
event is recorded and reported Applied — an error would roll back and the
provider would redeliver forever, and the review is already the durable
statement that something needs a human. The identity-mismatch check stays above
the guard, because two identities that disagree is tampering and deserves an
error whatever state the funding is in.

**Fix, second half, and why it was not optional.** Refusing the webhook is only
safe if something else can resolve a review. Nothing could: there was no admin
kind for a credit funding, `AdvanceFunding` and `DisputeFunding` had no caller
outside the package, and the HTTP surface over fundings is read-only. Left
there, the fix would have converted "mints the wrong amount" into "parked
forever, with its money counted against the at-risk ceiling for the life of the
deployment" — which is F-90's failure returning through a different door.

So `KindCreditFundingReviewResolve` exists, dual-controlled, on the
`KindPayoutManualReviewResolve` template. It differs from that template in the
one way that decides its permissions: resolving to CAPTURED **mints**. So the
approve half is `credit:adjust`, the dual-control permission that already
guarded an administrative balance adjustment, rather than a review-side one of
its own; the propose half is the new `credit:review`, held by OPERATIONS and
FINANCE. The effect runs through `apply`, the same function a provider event
uses, so a hand resolution mints, freezes or claws back by exactly the audited
path an automatic one would, and the approval id is recorded where an event id
would be.

The resolution vocabulary omits two things deliberately. There is no resolution
to SETTLED — the reason `fundingTransitions` already gave, that settlement is a
fact about a clock and an operator who could assert it by hand could make value
payout-eligible by closing a ticket. And there is no resolution that returns a
funding to the provider-driven path: resuming would hand it back to `Dispatch`,
which parked it because this binary could not read what the provider was saying,
and being told to look again does not make it able to.

**Evidence.** REAL_DB_INTEGRATION. Observed failing with the three assertions
above before the guard, passing after. Six subtests cover both halves: the
un-park refusal for CAPTURED, DISPUTED and CHARGEBACK; the control that an
unparked funding still mints normally; that a resolution to CAPTURED mints and
one to FAILED does not; that a funding which is not parked has no review to
resolve; that SETTLED is refused and leaves the funding parked; and that every
resolution names a state MANUAL_REVIEW can legally reach, so a resolution that
always refuses cannot be added by accident.

## F-101 · One transition row licenses a second, unrelated edge · NEW · P1 · FIXED

**Found by** an independent read of migration 00731 -- the fix for F-94 -- asking
what the flag string is made of rather than what the check does with it.

00731 binds an audited change to a transition row describing BOTH endpoints. It
encodes the edge as `<from>` `>` `<to>`, joins the edges of one transaction with
`|`, and checks exact membership over `string_to_array(flagged, '|')`.

Both delimiters are in band, and `from_*`/`to_*` are unconstrained `text` on
fourteen of the fifteen tables 00731 converted -- only
`wallet_status_transitions.to_status` carries a CHECK, and no table constrains
its origin. So a single row whose endpoint CONTAINS the delimiters splits the
flag into more elements than the row describes.

**Reproduced as `cp_app` against PostgreSQL 16 with migrations through 00731.**
Two accounts, both ACTIVE. The control is the forgery 00731 exists to refuse:

```sql
INSERT INTO account_status_transitions (..., from_status, to_status, ...)
VALUES (..., 'RESTRICTED', 'FROZEN', ...);
UPDATE accounts SET status = 'FROZEN' WHERE id = $1;
-- ERROR: AUDIT_TRANSITION_REQUIRED ... (AU001).  Account stays ACTIVE.
```

The same forgery, with the delimiters:

```sql
INSERT INTO account_status_transitions (..., from_status, to_status, ...)
VALUES (..., 'RESTRICTED', 'FROZEN|ACTIVE>FROZEN', ...);
UPDATE accounts SET status = 'FROZEN' WHERE id = $1;
-- COMMIT.  Account is FROZEN.
```

    flagged = 'RESTRICTED>FROZEN|ACTIVE>FROZEN'
    split   = {'RESTRICTED>FROZEN', 'ACTIVE>FROZEN'}

The account moved ACTIVE → FROZEN under a row that says it moved from
RESTRICTED to a string that is not a status. The origin position works too, and
was observed separately: `'RESTRICTED|ACTIVE' → 'FROZEN'` gives
`{'RESTRICTED', 'ACTIVE>FROZEN'}` and licenses the same change.

**It needs one row.** 00731's header reasoned about how many rows it takes to
license an extra edge and answered "more than one, and each must describe its
own step". That was true of honest rows and was never true of this one.

**Fix.** Migration 00732 refuses `|` and `>` in either endpoint, in
`cp_flag_transition_edge` -- where the flag is built, so no forged flag exists
to be read -- and symmetrically in `cp_require_transition_edge` for the entity's
own values. Refused rather than escaped: escaping keeps the encoding ambiguous
and hands the question to the next reader, while refusing makes the two
characters impossible inside a state, which is a smaller thing to know. Every
state name in the schema is SCREAMING_SNAKE and neither character has ever
appeared in one; the guard turns an assumption the encoding already depended on
into a constraint it enforces. It lives in the shared functions rather than in
fifteen CHECK constraints, so the sixteenth table to be bound gets it by
construction.

**Evidence.** REAL_DB_INTEGRATION. Both variants observed committing against
migrations through 00731 as the real application role, and both observed
refused with AU001 after 00732, with the entity unchanged in each case. Two
subtests in `TestIntegration_ATransitionRowCannotDenyTheChangeItLicenses` now
carry them, beside the forgeries 00731 already refused; the suite's honest
single-step and two-step controls still pass, so the guard is not one condition
too strict.

**What this says about the class.** F-94, F-78 and this are three findings about
the same mechanism, each closing the previous one's blind spot: the destination
was checked and the origin was not; then the origin was checked and the encoding
that carried it was not. The register's standing note -- that a control nobody
executes decays to a claim -- has a sibling here: a control whose input is
attacker-shaped is only as strong as the parser in front of it.

## F-102 · Two writes kept the operator read override, and the guard could not see them · NEW · P1 · FIXED

**Found by** an independent authorization audit enumerating, per route, where
ownership is actually proven.

F-36 established the rule and this repository states it in the helper's own doc
comment: `accountScope` honours the operator override `account:read_any`, which
is right for a read and wrong for a write, because RoleAdmin holds that
permission **and** the customer surface. `accountScopeWrite` has no override.

Two write routes did not use either helper. They scoped through helpers written
for records fetched by their own id, where there is no account id in the
request to hand to `accountScopeWrite`:

- `POST /v1/intents/{id}/cancel` → `requireIntentScope` → `security.RequireAccount`
- `POST /v1/internal-products/{id}/status` → `securityRequireAccount` → the same

Both are the read-grade primitive. So one ADMIN session could cancel any
customer's trade intent, and publish, pause or withdraw any seller's product —
with no reason, no second principal and no admin action, which is exactly the
accountability the admin plane exists to impose. `internal/commerce`'s
`SetStatus` takes no principal, so nothing downstream compensated.

**Reproduced.** An ADMIN principal with no account memberships, against an
intent belonging to another account:

```
POST /v1/intents/{id}/cancel   ->  202 Accepted
```

with the cancellation reaching the port. After the fix, 403, the port not
called, the owner's own cancel still 202, and an operator's READ of the same
intent still 200 — the override was never the problem on reads.

**The finding behind the finding.** `TestAccountScope_EveryWriteUsesOwnershipOnly`
exists precisely to stop this, and passed throughout. It is a source scan that
matched the literal string `accountScope(ctx`. Neither offender contains it.
A guard over a two-helper invariant, defeated by naming a third helper.

**Fix.** `requireIntentScopeWrite` (ownership only) for the cancel;
`securityRequireAccountOwner`, which already existed, for the product status.
Then the guard was given the full list of read-grade helpers — and a second
test, `TestAccountScope_TheReadGradeListIsComplete`, which derives that list
instead of trusting it: every function in the package that calls
`security.RequireAccount` must be declared, so a fourth helper cannot appear
unchecked.

**It found one on its first run.** `requireOrderScope`, guarding
`GET /v1/orders/{id}`. Read-only and correct — and nothing had ever confirmed
that. Four read-grade helpers existed where the guard knew of one.

**Evidence.** TEST_DOUBLE_ONLY for the route behaviour — the harness drives the
real router, the real generated wrapper and the real authorization middleware,
but the intent port is a fake, so what is proven is the boundary's decision and
not the domain's. STATIC_PROOF for the completeness check. That is the right
level for this defect: the refusal being tested belongs to the boundary.

## F-103 · Five configuration rules that permitted what the deployment cannot survive · NEW · P1 · FIXED

**Found by** an independent audit of `internal/config` that ran the repository's
own loader over crafted environment files rather than reading the rules.

The configuration check is the control that is supposed to see a bad deployment
before the deployment does. Five things it could not see.

### 1. Live provider credentials outside PROD — the P1

The environment/mode pairing was written as `switch env { case EnvProd: ...;
case EnvStaging: ... }`. **Every other environment fell through it.** DEV
therefore accepted `live` mode on all fourteen provider slots.

That matters because DEV constrains nothing else either. One environment file,
two values of `CP_ENV`, run through `scripts/configcheck`:

| `CP_ENV` | result |
|---|---|
| `DEV` | 91 variables supplied, and the configuration is valid |
| `PROD` | **20 rule violations** |

The identical file carried every provider at `live`, `CP_AUTH_MODE=dev`,
`CP_SEED_ENABLED=true`, `CP_DATABASE_REQUIRE_TLS=false`, `CORS_ORIGINS=*`, a
non-secure cookie, an `http://` base URL, every transport budget `off`, and no
trusted proxies. So a DEV deployment could hold live payment credentials, mount
the dev identity picker, talk to the production database without TLS and
enforce no rate limit — and pass.

The belt-and-braces had the same hole: both money-taking adapters carry the
same two-case switch, and `internal/provider/stripe` has no pairing check at
all.

**A test asserted it.** `TestValidate_TheEnvironmentAndTheProviderModeMakeTheSameClaim`
had a subtest named *"DEV is not constrained either way"*, whose comment
reasoned that a DEV deployment pointed at a live provider *"is a decision its
operator gets to make"*. It has been replaced by *"live money belongs to PROD
and nowhere else"*, stated per environment and per slot.

**Fix.** The rule is now about the MODE, not a list of environments: live is
permitted only in PROD, and PROD requires it. A total rule cannot acquire the
same hole when a sixth environment is declared.

### 2. A trusted-proxy list that trusts everyone

`RuleTrustedProxyDeclared` (F-88) made `CP_HTTP_TRUSTED_PROXY_CIDRS` mandatory
in STAGING/PROD. Nothing bounded its width, and `0.0.0.0/0,::/0` validated
clean.

A trusted peer's `X-Forwarded-For` is taken at face value, so a default route
does not widen the control — it inverts it. Every caller then chooses the
address that lands in the audit record, in `login_attempts`, and in every
unauthenticated rate-limit bucket, including the 30/minute auth budget. F-88
made the list mandatory; nothing made it mean anything.

**Fix.** A prefix length of zero is refused, with the reason.

### 3. The seventh retention class had no floor

Six retention classes were checked non-negative. `Retention.LoginAttemptDays`
was not in the map, though its own documentation says "Minimum 1". A negative
value made every purge pass in `cmd/audit-worker` fail, so the plaintext OIDC
nonces and PKCE verifiers the class exists to delete were never deleted — and
the symptom was a failing worker rather than a failing configuration check.

The regression test loops over all seven rather than restating the one, because
the defect was an absence from a list.

### 4. A legal policy the binary refuses to boot on

`cmd/api`'s `legalRouterFor` refuses an unknown policy name, and refuses
`DEVELOPMENT` in a production-like environment. `internal/config` had **no rule
for `API.LegalPolicy` at all**, so `CP_API_LEGAL_POLICY=development` in STAGING
validated clean and then would not start — the same failure `test/infra`
already records for the settlement asset, reproduced by a variable that is in
the table and had no rule.

**Fix.** The vocabulary moved into `internal/config` as
`NormalizeLegalPolicy`, and `legalRouterFor` now reads it instead of carrying
its own switch. Two lists that must agree are now one list — which is the
repeated defect this register calls "a list duplicated in two places diverges",
here in its sharper form: one of the two places had no list.

### 5. A TLS rule nothing read

`RuleTemporalTLS` refuses `CP_TEMPORAL_REQUIRE_TLS=false` in STAGING/PROD. The
only Temporal dial in the tree passed no `ConnectionOptions`, so a production
workflow worker connected in plaintext while the configuration check certified
TLS. The other four `*_REQUIRE_TLS` flags are honoured by their clients; this
one was F-89's shape — a value loaded, validated, and read by nothing.

**Fix.** The dial sets a TLS config when the flag is set. Verification against
the system roots, deliberately: an empty `tls.Config` that skipped verification
would be the same rule enforcing nothing in a more convincing way.

**Evidence.** STATIC_PROOF for the fixes, plus the auditing run's LOCAL
execution of `scripts/configcheck` — the repository's own loader — for the
DEV/PROD asymmetry and for each accepted-bad-value above. Four new tests, each
carrying its control so that none of the rules is one condition too strict.
Correcting the fixtures was itself informative: several tests built "the
production configuration, but DEV" by overwriting `CP_ENV` alone, which left
fourteen providers on live credentials. That is the same mistake the rule now
refuses, made inside the suite that was supposed to catch it.

## F-104 · Three controls that reported something other than what they enforced · NEW · P2 · FIXED

**Found by** two independent audits — one of observability as a security
surface, one of test-suite integrity — arriving at the same shape from
different directions.

### 1. A secret redactor that never satisfied the interface it named

`internal/config/secret.go` carried:

```go
// LogValue implements slog.LogValuer so that a SecretRef logged directly is
// always redacted.
func (r SecretRef) LogValue() fmt.Stringer { return redactedStringer(r.Redacted()) }
```

`slog.LogValuer` requires `LogValue() slog.Value`. The comment's claim is
false: `SecretRef` did not implement the interface, `slog` never called the
method, and the type fell back to being formatted as the string it is.

Observed, by logging a plain `SecretRef` under two ordinary keys:

```
{"level":"INFO","msg":"startup","archive_ref":"hunter2-the-live-password"}
{"level":"INFO","msg":"startup","anything_at_all":"hunter2-the-live-password"}
```

Under a key the logger's own denylist recognises — one containing "secret",
"password", "key" — the value was masked anyway, by a different control. That
is what kept this invisible, and it is exactly why the regression test logs
under `anything_at_all`.

There is no evidence of a live leak: `Redacted()` has no non-test callers, and
the two near-miss sites log `*_ref` values, which are references and are safe
by design. The defect is that the guarantee did not exist, not that it was
breached.

**Fix.** The correct signature, plus `var _ slog.LogValuer = SecretRef("")` —
the compile-time assertion `internal/id` already makes for the same reason. A
wrong signature now fails to build rather than compiling quietly into a control
that does nothing.

### 2. A step-up window three times the one enforced

`authorize` uses `effectiveStepUpMaxAge(configured)`, the MINIMUM of the
deployment's `CP_AUTH_STEP_UP_MAX_AGE` and the package ceiling — the F-89 fix,
so a deployment can only tighten. `toAPIPrincipal` built `step_up_valid_until`
from the ceiling alone.

The deployed value is 5 minutes. So `GET /v1/me` told an operator their step-up
was good for fifteen minutes while the boundary refused after five. The
operations that gate on it are the capability-gate ceremony and the admin plane
— the three-principal ritual that decides whether this deployment may sell
Credits — and a client that trusts the field submits an approval it is about
to be refused for.

F-89 fixed the enforcement and left the reporting one call site behind.

### 3. A ceiling test that passed because a different guard fired

`TestBudgetWithNoCeilingIsRefused` asserted *"a negative ceiling is a typo, not
a policy"* with `Budget{MaxAccounts: -1}`. `NewGuard` checks, in order: nil
clock; then whether **all four** ceilings are `<= 0`; then the per-field
negative loop. With one field at `-1` and the rest at their zero values, all
four are `<= 0`, so the no-ceiling rule returns first and the loop the
assertion names had never run in any test — the register's own recurring
class, "a test that passes because a different guard fired has not seen the
guard it names".

**The consequence reported to me did not follow, and that is worth recording.**
The audit that found it concluded `MaxAtRiskMinor: -1` would therefore be
accepted and the money-at-risk ceiling silently disappear. Checked: it would
not. With any other ceiling positive the no-ceiling rule does not fire and the
loop refuses it; with none positive the no-ceiling rule refuses it. The loop is
reachable and correct. What was wrong was only the test — which is a real
defect, because a control nothing exercises is a claim, but it is a smaller one
than reported.

**Fix.** Four cases, one per field, each with a positive sibling so the
no-ceiling rule cannot answer first, and each asserting the message so the
guard that spoke is the guard being tested.

**Evidence.** STATIC_PROOF for the fixes; the secret leak was observed — the
test fails against the old signature with the plain value printed in full, and
passes against the new one. The step-up test builds a server with a tightened
window and compares the reported instant to it, with a control that an
unconfigured deployment still reports the ceiling rather than zero.

## F-105 · An unauthenticated caller chose how much of the database to consume · NEW · P1 · FIXED

**Found by** an independent audit of the abuse and resource-exhaustion surface,
reasoning from the deployment (one Render free instance, Neon free tier) rather
than from the code alone.

Five facts, each checked:

1. Every rejected webhook delivery writes a `security_events` row, outside any
   transaction, unconditionally — `internal/webhook/handler.go`, and the
   function's own comment says it "is the only persistence a rejected delivery
   leaves behind".
2. The route is **unauthenticated**. Rejection is what happens when the
   signature does not verify, so producing one requires no credential.
3. `security_events` is append-only by trigger (`security_events_immutable`
   BEFORE UPDATE OR DELETE → `forbid_mutation`), and **no role holds DELETE**.
   `cp_app` has SELECT and INSERT. Nothing in the tree deletes from it.
   `CP_RETENTION_SECURITY_AUDIT_DAYS` exists and feeds a *checkpoint* retention
   option, not a purge of this table.
4. `render.yaml` deploys **exactly one service**, a web service. There is no
   worker and no cron, so nothing that might have pruned it runs at all.
5. `capacity.Admit` refuses **every** action — opening an account and buying
   Credits alike — once `pg_database_size` reaches
   `MaxDatabaseBytes × DatabaseHeadroom`.

So an anonymous caller could write permanent rows at request rate until the
capacity guard halted the entire economy, with **no remediation path**: the
rows cannot be deleted by anyone, and the Neon free tier suspends the project
at its own ceiling. The audit's arithmetic put it at roughly 400,000 requests,
which the shared 120/min command budget allows in about two days from one
address and hours from a handful.

`GET /v1/auth/login` has the same shape through `login_attempts`: also
unauthenticated, also writing a row before anything else happens, and its purge
lives in `cmd/audit-worker`, which this deployment does not run.

### What is fixed

**The row count is now bounded by the clock, not by traffic.** One row per kind
per minute, carrying `suppressed_since_last` — the number of rejections it
stands in for. That is strictly more informative than N identical rows, and it
makes the rate a property of time.

Per KIND rather than per handler, so a flood of forged signatures cannot hide
the one stale timestamp that arrived during it. The kind vocabulary is four
constants, so the map cannot grow with traffic — a bound that leaked memory
instead of rows would not be a fix.

**The webhook route no longer shares a budget with the admin plane.** It fell
through to the Command bucket at 120/min. It is now on the unauthenticated
budget, which is the strictest, and which is the right home for it: a
provider's real delivery volume is a few a minute, and a 429 makes it retry
rather than lose the event.

**Two existing tests asserted the defect** and have been corrected. Both drove
two rejections and required two rows. They now require one, plus the
suppression count on the next window's row, plus the control that twenty
further deliveries are still each REFUSED — suppressing the row never
suppresses the rejection.

### What remains, and why it is not closed

**`security_events` is still unprunable.** The bound converts "hours,
unattended" into "months, loudly", which is a large improvement and not a
complete one: any nonzero steady rate eventually fills a 500 MB database when
nothing can ever remove a row.

The complete fix is the one ADR-0020 already decided for append-only tables:
retention is **partition detachment, never row deletion under a disabled
trigger**. `security_events` is not partitioned, so that is a migration that
converts it, plus the detachment job and the tests that prove a detached
partition takes its rows with it and leaves the chain intact. It is deliberately
not being written at the end of this batch, for the reason F-42 records about
changes to mechanisms many things depend on.

### Update 2026-09-10: the `login_attempts` half is done

It was recorded here rather than fixed, on the grounds that the answer was
"either a cron on the Render blueprint or a ticker inside `cmd/api` the way
`runCreditSettlement` already is, and that is a deployment decision". Half of
that was right and half was an excuse: **which** answer is a deployment
decision, but the ticker is the same one F-90 already reached for settlement,
and the reasoning transfers without modification — when the deployment has one
process, the periodic work belongs in it.

`runLoginAttemptRetention` purges expired attempts hourly, from its own `cp_ops`
pool, because `cp_app` deliberately holds no DELETE on `login_attempts`: an
attacker with the application credential must not be able to erase the record of
the logins they attempted.

**What it does when it cannot run is the part with a test.** With no
`CP_DATABASE_OPS_URL`, or a retention of zero, it logs a WARN naming *what will
not happen* — "plaintext OIDC nonces and PKCE verifiers are kept
indefinitely" — and returns. It does **not** refuse to boot: `cmd/audit-worker`
refuses because purging is why that binary was deployed, and this is a web
service whose job is serving requests; taking it down over an unconfigured
retention pass trades a disclosure risk for an outage. But it must not be silent
either, because "a control that reports success having run nothing" is the
defect class this register keeps recording, and a retention pass that quietly
does not exist is the same shape.

`render.yaml` now declares `NODAL_DB_OPS_URL` with `sync: false`, so the
operator is prompted for it. **Until they supply it the purge still does not
run** — that part is genuinely theirs, and the WARN is what says so.

**Still open:** `security_events` remains unprunable. That is the larger half
and it is a schema change, per ADR-0020: partition and detach, never delete
under a disabled trigger.

**Evidence.** STATIC_PROOF for the exhaustion path — the five facts above were
each read, not inferred, and no flood was run against the deployed service.
REAL_DB_INTEGRATION for the fix: the webhook suite drives real deliveries
against a real database and now observes one row where it observed two, the
suppression count on the next window's row, and twenty further rejections still
answered 400.

## F-106 · An idempotency key was not one account's · NEW · P1 · FIXED

**Found by** an independent audit of the financial kernel, tracing what each
client-supplied value is allowed to decide.

Seven tables carry a `idempotency_key` that is UNIQUE across the whole table
rather than per account. That alone is not the defect — what matters is what
the lookup does with the row it finds.

The HTTP boundary's own idempotency record is keyed by `(actor, endpoint,
key)`, which is correct and is also why this is reachable: a **different**
caller reusing a key finds no record there, passes cleanly, and arrives in the
domain. The domain is where it is decided whether to hand the row over.

**Four asked whose it was** and refused with `INVALID_IDEMPOTENCY_REUSE`:
`credit_fundings`, `deposits` (twice — repository and service),
`withdrawals`, `asset_reservations`.

**Three did not:**

| table | call site | what came back |
|---|---|---|
| `payout_requests` | `payout.Create` | another account's payout: its account id, requested/reserved/settled quantities, destination and failure reason |
| `internal_commerce_orders` | `commerce.Purchase` | another account's order: buyer, seller, price, platform fee, proceeds |
| `native_market_fills` | `nativemarket.Execute` | another account's trade — and a market moves on every fill, so at a price this caller never saw |

In every case the caller's own request was **silently discarded** and they were
told a record existed that they had never created.

`native_market_fills` is the sharpest instance: the lookup did not even
**project** `account_id`, so no caller could have made the check. The comparison
had to be added to the query before it could be added to the code.

**And one table gets it right in the schema, which is the strongest form.**
`trade_intents` is `UNIQUE (account_id, idempotency_key)`, with `ON CONFLICT
(account_id, idempotency_key)` to match. There the collision cannot happen at
all rather than being caught afterwards.

**Reproduced.** Two accounts, one key, against a real database:

```
account A: POST payout, key "shared-key-0001"  -> created
account B: POST payout, key "shared-key-0001"  -> A's payout, no error
```

After the fix, B is refused with `INVALID_IDEMPOTENCY_REUSE`, and A's own retry
still returns A's row — which is the control that matters, because scoping a
key must not stop it being a key.

**Fix.** The three now make the comparison the other four already made. Plus a
structural guard, because the defect was silent: nothing anywhere said which
tables were in this shape.
`TestIntegration_EveryGlobalIdempotencyKeyIsScopedByItsOwner` reads
`pg_index` for every unique index over an idempotency key and requires each
table either to scope it by `account_id` in the schema, or to be declared with
the place the account comparison lives. A new table with a global key fails
until somebody writes that down. Two are declared as deliberately not
account-scoped, with the reason: `journal_transactions` compares a content hash
and its key is server-derived, and `execution_plan_steps`' key is the planner's,
never a client's.

**Evidence.** REAL_DB_INTEGRATION. The payout case was observed succeeding
before the fix — "An error is expected but got nil" — and refused after, with
the owner's own retry still a replay. Commerce and native market carry the same
pair of assertions. The structural guard reads the live catalogue rather than a
list typed out beside it.

## F-107 · Two money decisions taken by the party who benefits from the answer · NEW · P1 · FIXED

**Found by** two independent audits — the financial kernel's sweep of
client-supplied values, and the money-out audit's read of the payout state
machine.

### 1. The seller set the platform's commission

`POST /v1/internal-products` took `platform_fee_bps` from the request body.
Validation had a **ceiling and no floor** — `PlatformFeeBPS < 0 ||
PlatformFeeBPS > MaxPlatformFeeBPS` — and the schema agreed, `DEFAULT 0 CHECK
(0..3000)`. Omit the field and it is zero.

That number is the sole input to the revenue split and flows to the
`PLATFORM_FEE_RECEIVABLE` entry and the order's `platform_fee` and
`seller_proceeds`. And `commerce:sell` is a **CUSTOMER** permission. So the
platform's take was whatever the counterparty who benefits from it being zero
decided, and every rational seller decided nothing.

Two things make it a defect rather than a pricing choice, and both are in this
repository:

- The contract's own words are *"The platform share, capped at 3000 (30%)"*,
  and `MaxPlatformFeeBPS`' comment says the cap exists so *"a creator cannot be
  asked to agree to a share that leaves the transaction pointless"*. The cap
  protects the creator FROM the platform. It is unambiguously the platform's
  number.
- The native market decides the same number through a **dual-controlled admin
  action**, not by the creator.

**Fix.** The field is gone from the create request — removed from
`openapi.yaml` and regenerated, in both the Go server and the TypeScript client
— and `CreateProduct` overwrites the value from a server-side policy set by
`SetPlatformFeeBPS`. Overwritten rather than validated, because a value the
seller cannot influence needs no validation and refusing would tell them a
field exists that does not.

`DefaultPlatformFeeBPS` is **zero**, deliberately, and this is the part worth
being precise about: **no business rate has been invented here.** Zero is the
rate the system already charged, because the seller-supplied value defaulted to
the Go zero value and sellers left it there. What changed is *who decides*, not
*what the number is* — which is the whole defect. A deployment that wants a
commission now has one place to say so, with an owner.

### 2. A payout the provider may have paid could be cancelled by its owner

`Cancel` refused `SUBMITTED`, `PROVIDER_PENDING` and `PAYOUT_STATUS_UNKNOWN`.
**`MANUAL_REVIEW` is not in that list** — and `applyProviderResult` parks a
payout there precisely when the provider WAS called and the settlement could not
be recorded: a lost response, or a ledger posting that refused.

So the sequence is: submit → provider pays → response lost → parked for a
person → the account owner calls `POST /v1/payouts/{id}/cancel`, which is
`payout:create`, no step-up, no approval → `returnReservation` posts
`PAYOUT_PENDING → INTERNAL_CREDIT` and restores every lot. The provider has
paid and the user has their Credits back.

**The asymmetry is what names it.** `everSubmitted` exists for exactly this
question, reads the transition history rather than the current state, and was
already consulted by `ResolveManualReview` — the dual-controlled path. So the
two-person path refused to retry an ever-submitted payout while the single-user
endpoint would unwind one.

**Fix.** `Cancel` consults `everSubmitted` too.

**Evidence.** REAL_DB_INTEGRATION for both. The payout case was observed
succeeding before the fix — "An error is expected but got nil", "the
reservation was released on a payout the provider may have paid" — and refused
after, with the reserved balance unchanged. The commerce case asserts the
platform's receivable moves by the deployment's rate on a sale the seller
listed, as a delta because that receivable is one account for the whole
deployment; plus that the policy is bounded on both sides, a negative being a
typo rather than a fee schedule.

Correcting the commerce fixtures was itself the smaller half of the finding: the
suite's `list` helper took a fee per product, because that was the shape of the
thing. It now sets the deployment's policy, so every existing test that says "a
product with a 4% platform fee" still says it — to the place that decides.

## F-108 · Silence was read as proof · NEW · P1 · FIXED

**Found by** the money-out audit, reading what `provenAbsent` actually
consults.

`chain.AgreementPolicy.ResolveSingle` sets `Degraded: true` on the honest
not-found path, and says why in its own comment: *"absence is never proven"* by
one observer. `provenAbsent` refuses a degraded resolution, and repeats the rule
at the decision point.

Three resolutions in `recovery.go` are constructed by hand rather than by the
policy, and all three left `Degraded` at its zero value:

- no chain observer configured
- primary failed and there is no secondary
- **both observers errored** — `Detail: "no observer answered"`

So "the RPCs failed" arrived at the decision indistinguishable from "both
observers looked and the transaction is not there". The remaining gate is
`GetBlockHeight` — a **different method**, routinely healthy while
`getTransaction` is rate-limited.

**What that costs.** A transaction that landed on chain, with the user's tokens
already spent, is recorded `PROVEN_ABSENT`; its attempt is marked EXPIRED,
which is not `Recoverable`, so nothing ever looks again; and its reconciliation
record is written MATCHED, immaterial, non-blocking and terminal. The user's
asset is gone, the ledger says they still hold it, and the books say everything
agrees. The same pass sets `RetryAllowed = true`.

**Fix.** `Degraded: true` on all three. The field means "this rests on
incomplete observation", and all three do — including the first, which was
reachable only in the sense that a *different* check (`Primary != nil`) happened
to refuse it first.

### The test had to be written twice, and that is the finding behind it

The first version faulted one observer and **passed against the unfixed code**.
A single failure does not reach the branch: it falls to `ResolveSingle`, which
sets `Degraded` correctly. The test named the right defect and exercised a
different guard — the register's own recurring class, caught only because the
fix was removed and the test re-run before it was believed.

The version that stands faults **both** observers' `GetTransaction` and leaves
`GetBlockHeight` healthy on both, which is the shape of a rate-limited RPC
rather than a dead node, and is what made the remaining gate useless. Against
the defect it reports:

```
Should not be: "PROVEN_ABSENT"
  an RPC failure was read as proof the transaction never happened
Should be false   -- a retry under uncertainty may double-spend
Should be true    -- uncertainty about a spent balance must hold new risk
```

`chaintest` has had `Fault` since it was written, with `FaultError`,
`FaultTimeout`, `FaultNotFound` and more. **No test in `internal/reconciliation`
had ever injected one.** That is why a recovery engine whose entire job is
deciding what to believe when an observer disagrees or goes quiet had never been
asked what it does when one goes quiet.

**Evidence.** REAL_DB_INTEGRATION, observed failing then passing, against a real
database and the package's own chain simulator.

## F-109 · The binding covers the state column and nothing else · NEW · P1 · FIXED

**Found by** the money-out audit, enumerating what `cp_app` can actually do
rather than what the migrations say they granted.

Six findings in this register are about binding a state change to a transition
row: F-42, F-78, F-94, F-101, and the migrations 00603, 00726, 00731, 00732 that
implement it. All of them are about the STATE column, and **none of them says
anything about the other columns.** The binding cannot: a constraint trigger
declared `AFTER UPDATE OF status` fires only when `status` appears in the
statement's SET list.

So:

```sql
UPDATE withdrawals SET destination_address = '<attacker>' WHERE id = $1;
```

fires nothing at all — no transition row, no audit event, no refusal. And:

```sql
INSERT INTO withdrawal_transitions (...,'APPROVED','SUBMITTED',...);
UPDATE withdrawals SET status = 'SUBMITTED',
       destination_address = '<attacker>', quantity = quantity * 10
 WHERE id = $1;
```

commits cleanly. AU001 is satisfied, the edge is legal, and the trail records a
lawful state move while the payload was rewritten underneath it.

Confirmed against the live catalogue: `cp_app` held **table-wide** UPDATE on all
eighteen columns of `withdrawals`, twenty-three of `payout_requests`, seventeen
of `assets` and fifteen of `instruments` — including `quantity`,
`destination_address`, `approval_id`, `step_up_verified_at`,
`destination_validated`, `requested_quantity`, `account_id`, `destination_id`,
both idempotency keys, `mint_address`, `decimals` and `settlement_asset_id`.

**`assets` is the sharpest.** `cp_app` could write:

```sql
UPDATE assets SET risk_class = 'SETTLEMENT', is_stablecoin = true,
                  peg_currency = 'USD', decimals = 0 WHERE symbol = 'SOL';
```

and `internal/reconciliation` marks any `is_stablecoin AND peg_currency='USD'`
asset at **face value**, scaled by that same mutable `decimals`, with no status
and no risk-class check. That is the materiality test deciding whether a
reconciliation break is worth waking a human for. `internal/valuation` does the
same job correctly and is the model.

**Fix.** Migration 00733 applies the treatment this schema already uses six
times — 00604, 00701, 00719, 00720, 00723, 00730 — to the four tables where
the columns are money, a destination, or the definition of what counts as money:
`REVOKE UPDATE`, then `GRANT UPDATE (…)` on exactly the columns the application
writes today, read from its own UPDATE statements one at a time.

| table | granted |
|---|---|
| `withdrawals` | `status`, `step_up_verified_at` |
| `assets` | `status` |
| `instruments` | `status` |
| `payout_requests` | the fourteen the service writes across nine statements |

Everything else is written once at INSERT. If one ever legitimately needs to
move, that is a migration and a decision — which is the point.

**Observed, as `cp_app`, after the migration:**

```
a bare destination rewrite            ERROR: permission denied for table withdrawals
a rewrite alongside a lawful move     ERROR: permission denied for table withdrawals
any asset becomes settlement money    ERROR: permission denied for table assets
a payout's requested quantity         ERROR: permission denied for table payout_requests
the control: a lawful status move     permitted
```

**What this does and does not close of F-42.** F-42 is the standing finding
that `cp.transition.*` is a session variable any role can set, so the AU001
binding is satisfiable without a transition row, and its recorded remedy is the
`capability_gates` treatment applied to the remaining tables. This applies it to
four of them, chosen because their columns are money. F-42 stays OPEN: the other
audited tables still hold table-wide UPDATE on their state column, and the GUC
is still forgeable there.

**Evidence.** REAL_DB_INTEGRATION. The grants were read from
`information_schema.column_privileges` before and after, and each refusal was
observed as the real application role.
`TestIntegration_MoneyColumnsAreOutOfTheApplicationsReach` asserts the column
set per table from the catalogue, with a negative control so an empty query
result cannot pass by comparing nothing — so a later migration widening one
back to table-wide UPDATE fails there.

## F-110 · An automatic resolution did not make the checks its contract names · NEW · P2 · FIXED

**Found by** the money-out audit, comparing `ResolveAutomatic`'s doc comment
against its body.

The comment states two conditions:

> - FEE_DUST needs a difference at or below the asset's dust threshold …
> - OBSERVATION_CAUGHT_UP needs the recorded difference to be exactly zero.

Neither was in the function. The dust test lived inside `dustRepair`, which is
reached only when `policy.AutoPostDustAdjustment` is true — **false by default
and not settable from configuration** — so on the path the deployment actually
runs, FEE_DUST closed a record of any size with no amount test at all. The
caught-up rule existed nowhere.

The only gate was `!rec.Material`, a plain boolean on a table `cp_app` may
update.

Which direction it resolves in is what makes it matter: the repair writes a
customer's ledger balance **down** when the chain holds less, by the SYSTEM
actor, with no human.

**Fix.** `autoCauseFits` enforces both conditions where the resolution is
decided rather than inside the repair. A record with no difference recorded is
refused for both amount-bearing causes rather than treated as zero — "nobody
wrote down what the difference was" is not the same fact as "the difference was
nothing", and only one of them justifies closing a record without a person.
FINALITY_UPGRADE and DUPLICATE_PROVIDER_EVENT are untouched: they are statements
about how an observation was made, and the contract claims no amount condition
for them.

**The fixture is the finding's other half.** The existing
`TestIntegration_AutomaticResolutionIsNarrow` forces `AutoPostDustAdjustment =
true`, so it exercises the one branch that did check. The new test leaves it at
its default — the path the deployment runs, and the path nothing had ever
taken.

**Evidence.** REAL_DB_INTEGRATION, with a control: a genuine dust difference
still resolves automatically, so the conditions are not one step too strict.

## F-111 · The documents that certify reality · NEW · P2 · FIXED

**Found by** a documentation-truthfulness audit over 92 Markdown files, run
because §2 of the governing goal forbids overstated evidence and requires
existing documents that overstate it to be corrected.

### The worst one: fabricated evidence for "documentation reflects reality"

`MASTER_BUILD_STATE.md`'s stopping-criterion 12 read **MET**, with this
evidence:

> traceability re-derived from source: 264 VERIFIED / 69 IMPLEMENTED / 71
> IN_PROGRESS / 34 BLOCKED_EXTERNAL / 28 NOT_STARTED, all 609 test references
> resolving to declarations that exist

All five figures are wrong, and they sum to **466** against a **389-row**
document. Counted here: 225 VERIFIED, 60 IN_PROGRESS, 59 IMPLEMENTED, 18
DEFERRED_OUT_OF_SCOPE, 14 NOT_STARTED, 13 BLOCKED_EXTERNAL. The string
`264 VERIFIED` occurs nowhere else in the repository and matches no version of
the file in its history, which has never been anything but 225 or 226.

The phrase "re-derived from source" is the aggravating part: it names the method
that would have produced the right answer.

The *property* it asserts — that every cited test name resolves to a
declaration — **is** machine-checked and does hold. Only the numbers were
hand-written.

### The summary table has now been wrong twice, the same way

`REQUIREMENTS_TRACEABILITY.md` declared IN_PROGRESS 59 against 60 rows and
VERIFIED 226 against 225 — left behind when F-65 correctly lowered R-053-12.
The file's own change log records the first occurrence: *"the summary table
previously said 103/138; the rows actually said 102/139"*.

`TestDocs_TraceabilitySummaryMatchesItsRows` now derives the table from the
rows. Verified non-vacuous: restoring the stale 226 makes it fail with *"the
summary says 226 rows are VERIFIED; 225 are"*.

### The capability table listed ten of twenty

§10 omitted the entire internal economy — including `CREDIT_PURCHASE`, the
capability this deployment exists to activate — while §3 of the same file said
"capabilities: 20, of which 18 are high-risk" and that sentence is
machine-checked and passing. The document contradicted its own verified number
1,570 lines later, because only one of the two was derived. The row count is now
derived from `gates.AllCapabilities()`.

### Five rows of the test matrix asserted the opposite of the same file

`integration 32/32` (51 packages exist, and §6 already said "the 50-package
sweep"); `contract: helius/solanarpc/privy pending` (all three exist and pass,
and §5 lists them); `load: unmeasured — no API binary yet` (contradicted three
times elsewhere); `e2e/chaos: directories not yet created` (both exist and are
CI jobs); `CI: authored; never executed` (§3b records the first green run).
And §11 listed Redpanda, Temporal, S3 and ClickHouse as NOT_STARTED when all
four are pinned, wired, containerised and exercised under
`CP_TEST_REQUIRE_EXTERNAL_DEPS=1` — LOCAL_EXTERNAL_STACK is their honest class.

### The highest-traffic wrong sentence

The top of the file said `HTTP endpoints, frontend, cmd/api wiring | not
started`, and "Exact next action" item 2 was *"Wire the adapters in `cmd/api`;
expose the purchase endpoints; mount the webhook handler."* Two of those three
were done. It is the first instruction a resuming session reads.

### The two documents nobody checked

`SECURITY.md` and `THREAT_MODEL.md` are what a reviewer would use to score
security posture, and they were the only ones of their kind outside
`references_test.go`'s scope. Empirically they were also the two that decayed
furthest — both **understating** the system:

- THREAT_MODEL listed "gate state can be flipped by a direct table write" as
  residual risk #8. Migration 00701 closed it: `cp_app`'s only updatable column
  on `capability_gates` is `version`, confirmed against the live catalogue, and
  the sole writer of its transitions is a SECURITY DEFINER function requiring
  three distinct actors. A paragraph beneath it asked for work that had been
  done.
- SECURITY.md, dated 2026-09-05, says `internal/signing` does not exist (it is
  the most thoroughly built area of the system), `internal/admin` is absent,
  `cmd/api` is absent, and `infra/` is empty (63 Terraform files).

An understatement is the same defect as an overstatement when the document's
purpose is to be accurate: a reviewer scoring this system from these two would
mark down controls that hold.

**Both are now in the checked set** — and on the first run that check found
**six citations naming tests that do not exist**, two of which had never
existed in any form. All six are corrected, either to the real test name or to
words saying plainly that no such test exists.

**Evidence.** STATIC_PROOF, with every count recomputed here rather than taken
from the audit: the row states were tallied from the file, the capability list
read from `gates.AllCapabilities()`, and the `capability_gates` grants read from
`information_schema.column_privileges`. Three of the corrections are now
machine-derived rather than hand-written, which is the only kind of fix that
stops this recurring.

## F-112 · A cookie Domain removes the binding F-87 depends on · NEW · P1 · FIXED

**Found by** the identity/session audit, tracing what `CP_AUTH_COOKIE_DOMAIN`
actually does.

`httpmw.EffectiveCookieName` adds the `__Host-` prefix **only when the cookie is
secure AND host-only**, and `SetSessionCookie` sets `c.Domain` only when the
name is unprefixed. The login-state cookie is derived the same way. So naming a
domain silently turns both into ordinary domain cookies — writable by any host
that can set a cookie for a suffix of that domain.

`internal/config` had **no rule for `Auth.CookieDomain` at all**.

**Why it is a P1 and not a hardening note.** The `__Host-` prefix is the whole
of the binding: `httpmw` has nothing else tying either cookie to one host. And
the login-state cookie's value is `base64url(sha256(state))` with **no server
secret**, so an attacker who begins their own sign-in knows the digest for their
own state. Being able to write that cookie into the victim's browser is
sufficient to plant a callback that signs the victim in as the attacker —
which is exactly F-87, closed three commits earlier and re-opened by a
configuration value. The session-cookie half is classic fixation.

The victim normally has no login-state cookie at all, so the attack does not
even need to win a collision.

**It was shipped.** `infra/terraform/environments/prod/terraform.tfvars.pathb.example`
set `cookie_domain = "api-nodal.actorvia.xyz"`. The live Render deployment does
not set it, so the deployed service currently gets `__Host-`; the AWS path is
where this was waiting.

**And the suite agreed with it.** `prodEnv()` — the fixture that stands for "a
valid production configuration" — set `CP_AUTH_COOKIE_DOMAIN=example.com`. So
the test suite's own idea of a correct production deployment included the value
that removes the control. That is the second time in this batch a fixture
encoded the defect (F-103 was the first, with fourteen providers on live
credentials in DEV), and it is worth naming as a pattern: **a suite's
"known-good" fixture is a claim about what is safe, and nothing was checking
it.**

**Fix.** `RuleCookieHostOnly` refuses a non-empty `Auth.CookieDomain` in
STAGING/PROD. The terraform example sets it empty with the reason. The fixture
sets it empty. Below STAGING it is still permitted: a domain there is a
developer convenience on a host serving nobody's money, and the prefix requires
Secure in any case.

**Evidence.** STATIC_PROOF for the exploit chain — it was traced through
`EffectiveCookieName`, `SetSessionCookie`, `SetLoginState` and
`GetAuthCallback`, not executed against a browser. The rule and its refusals are
tested per environment, with the control that the production fixture still
validates with the value unset.

## F-113 · The refunded amount was computed twice and read never · NEW · P1 · FIXED

**Found by** the provider/reconciliation audit, grepping for readers of a field
it had just seen written.

`PurchaseSnapshot.AmountRefundedMinor` is populated by the Stripe adapter in two
places — from a charge's `amount_refunded` on the webhook path, and from a
payment intent's charges on the lookup path. Outside the struct definition and
one test assertion, **nothing read it.** Neither `Dispatch` nor `Reconcile`
consulted it.

**Why that mints money that was returned.** A PaymentIntent's `status` stays
`succeeded` after a refund and its `amount` does not change. So:

- the amount check (`snap.Amount` vs `f.PaidAmount`) passes,
- `FundingStateFor(succeeded)` maps to CAPTURED,
- and CAPTURED is the edge that mints.

Two ways in. On the **webhook path**, an out-of-order `payment_intent.succeeded`
whose embedded charge is already fully refunded. On the **reconciliation path**,
a sweep looking up a funding whose refund webhook was lost — which is the path
that applies less scrutiny by construction, because it never sees metadata or an
event type, so the one thing it must not skip is whether the money is still
there.

**Fix.** `refundedReason` is consulted on both paths. A snapshot reporting any
refund is not a capture to mint against, and a funding in that state is parked
for a person rather than advanced — the same treatment an amount disagreement
already gets, and for the same reason: the provider is authoritative about what
it charged, and equally authoritative about what it returned. This binary was
listening to half of that.

Partial refunds park too. A partly-refunded payment is a question about how much
value was actually bought, and this code has no answer for it that is not a
person's.

**One premise is BLOCKED_EXTERNAL and the fix does not rest on it.** That
Stripe leaves `status` at `succeeded` and `amount` unchanged after a refund
needs a live Stripe account to settle. But the code defect — a field computed
in two places and read in none — is settled from source, and the safe
behaviour is the same whichever way that premise falls: a provider reporting
money returned is not a provider reporting money received.

**Evidence.** REAL_DB_INTEGRATION for the webhook path, in both the full and
partial forms, with the control that an unrefunded capture still mints — so
the check is about the refunded amount and not about captures.

## F-114 · A revoked agent could come back · NEW · P1 · FIXED

**Found by** the state-machine audit, comparing what Go's transition table
permits against what the schema permits.

`State.IsTerminal` returns true for REVOKED and SUPERSEDED, and `CanTransition`
gives both an empty destination list. In Go, a terminal state has no outgoing
edge. **The schema said nothing about it**, and both promotion CHECKs on
`agent_lifecycle_transitions` open with the same clause:

```sql
CHECK (from_stage = to_stage OR to_stage NOT IN (...) OR <evidence>)
CHECK (from_stage = to_stage OR to_stage NOT IN (...) OR approval_id IS NOT NULL)
```

That short-circuit is correct for what it was written for: a pause, a resume or
a revocation does not move the STAGE, and demanding promotion evidence for one
would be demanding evidence for nothing happening.

It is wrong for coming back. `Lifecycle.Revoke` goes through `sideTransition`,
which sets `ToStage: a.Stage` — so a revoked LIVE agent keeps `stage='LIVE'`,
`mode='LIVE'` and its envelope. A row saying

```
from_state='REVOKED', to_state='LIVE', from_stage='LIVE', to_stage='LIVE'
```

satisfies **both** CHECKs through that first clause, so it needs no
`approval_id`, no `ir_hash`, no `risk_policy_hash` and no `evidence_hash`. The
entity's own `state = stage` CHECK is satisfied; the stage binding short-circuits
because the stage did not move; and the state binding sees the edge
`REVOKED>LIVE`, which the row itself flagged. It commits, and an agent that was
revoked is trading live capital again with nothing recorded about why.

Reachable by `cp_app`, which holds INSERT on the transitions table and UPDATE on
`agents`.

**00726 closed the promotion form of that short-circuit. This is the
resurrection form**, and it survived because returning from a side state does
not move the stage either — the same shape as F-101 and F-94 before it: a
control closed for the case it was written about, with a second case that
reaches the same place by a different route.

**Fix.** Migration 00734 states in the schema the rule Go already states: a
transition row may not claim to leave a terminal state.

Narrower than requiring evidence on every entry into an operating state, and
deliberately so — that would also demand an approval to resume a paused agent,
which is a different decision and not this finding's to make. On the transitions
table rather than on `agents`, because the AU001 binding means no state change
commits without a row: refusing the row refuses the change, with a message that
says why.

**Evidence.** REAL_DB_INTEGRATION. Both resurrection rows observed refused by
the named constraint as `cp_app`, an ordinary pause observed passing it, and an
integration test that revokes a real agent through the real lifecycle and then
attempts the row directly — directly, because no Go path offers it, which is
exactly why the schema has to be the one to refuse it.

## F-115 · "Do not resubmit" resubmitted · NEW · P1 · FIXED

**Found by** the money-out audit, reading `Submit`'s two phases against each
other.

Phase one's early return covers three states with the comment:

> Already claimed by an earlier attempt. Reconcile, do not resubmit.

Phase two then guarded on `req.State != StateSubmitted`. PROVIDER_PENDING and
PAYOUT_STATUS_UNKNOWN returned there. **SUBMITTED did not** — it fell straight
through to `provider.Submit`.

And SUBMITTED is precisely the state a crash between the phase-one commit and
`applyProviderResult` leaves behind, which is the case the whole two-phase
design exists for. The branch whose comment says do not resubmit was the one
that resubmitted.

**Two concurrent Submits reach it the same way.** The second blocks on
`FOR UPDATE`, is released by the first's COMMIT, re-reads SUBMITTED under READ
COMMITTED, and returns from phase one — with the row lock already released,
because phase two is outside any transaction. Nothing serialised the external
call.

**The test double hid it.** The only thing standing between this and paying
twice was the provider honouring `nodal-payout-<uuid>`, and `payouttest.Sandbox`
dedupes on that key by construction. The covering test
(`TestIntegration_AProviderTimeoutDoesNotDuplicateThePayout`) never reached the
branch at all: after a timeout the state is PAYOUT_STATUS_UNKNOWN, which
returns.

### And the provider was the caller's to choose

`providerName` is an argument and was **never compared** to the persisted
`r.Provider`. The re-entry branch did not overwrite it either. So
`Submit(id, "providerB")` on a request already claimed for providerA called
**providerB with providerA's idempotency key** — two providers, one key, two
disbursements, and neither of them able to dedupe the other.

**Fix.** A `claimed` flag says whether THIS call did the claiming, because the
state does not answer that question; phase two runs only when it did. And
`providerMatches` refuses a provider that is not the one the payout was claimed
for, treating an unclaimed request (no provider recorded) as free to go to any
registered one.

**Evidence.** REAL_DB_INTEGRATION, both observed failing before and passing
after. The resubmit case forces the row into SUBMITTED directly, because no Go
path offers that crash window — which is the same reason F-114's test writes
its row by hand: the states worth testing are the ones the happy path cannot
produce.

## F-116 · A stream outlived the session it belonged to · NEW · P2 · FIXED

**Found by** the identity/session audit, asking what ends an SSE stream.

Revocation in this system is per request, and correctly so: `auth.Manager`
re-reads the session row on every call, so a revoked session dies at the next
one. **An SSE stream is one request that never ends**, so the per-request model
had no purchase on it.

The streaming loop's only exits were client disconnect, the drain context, and a
slow-consumer drop. Nothing re-checked the session, and the principal it filters
events against is a snapshot copied once at subscribe time.

So a stolen cookie's stream kept delivering the victim's balance, order, deposit
and agent events:

- after the victim hit `POST /v1/auth/logout`,
- after an operator revoked the session,
- and past the session's own 12-hour absolute expiry.

The incident runbook presents revocation as the containment for a leaked cookie.
For the one surface that streams a user's financial activity continuously, it
was not.

**Fix.** `stream.Handler` takes a `StillLive` hook and calls it on every
heartbeat — the heartbeat is where a stream re-earns its right to exist,
because for a stream there is no next request. `auth.Manager.StillLive` answers
it through `ListForSubject` and the same `Check` every request already runs, so
the answer comes from the same rows `/v1/sessions` shows. **Absent is treated as
revoked**: a session that is not in the subject's list is not one they may
stream from. It is keyed by subject AND session id, so a session id learned
elsewhere answers nothing.

Ending the stream writes a `resync` event first, which is the same thing a
slow-consumer drop does, so a legitimate client that reconnects behaves the way
it already knows how to.

**Evidence.** TEST_DOUBLE_ONLY, and deliberately: the test drives a real HTTP
server and a real streaming loop with a stub validity function, because what is
being proven is that the loop asks and acts on the answer — not what the
session store would say. It fails without the check with "the session was never
re-checked; the stream would outlive any revocation", and the passing form reads
the body to EOF to prove the SERVER ended the stream rather than the client.

## F-117 · Two metric labels the caller chose · NEW · P2 · FIXED

**Found by** the observability audit, reading the label sets rather than the
instruments.

Two dimensions were unbounded, on two different label sets attached to the same
request:

**`method`**, in this repository's own middleware, was `r.Method` — the raw
request-line token, and Go accepts any RFC 7230 token as one. chi runs
middleware before routing, so even a 405 recorded it. The middleware's own
comment says "attributes stay low cardinality"; `route` and `status_class` do,
and this one did not.

**`server.address` / `server.port`**, from `otelhttp`, fell back to
`SplitHostPort(req.Host)` — the raw Host **header** — because the handler was
built with no `WithServerName`. Those go on `http.server.request.duration` and
both body-size histograms, and they **bypass `WithSafeAttrs` entirely**, because
that filters the attributes this code passes, not the ones the instrumentation
library adds. Bounded only by `MaxHeaderBytes`, which is 1 MiB.

**Why it is fixed now rather than when metrics are armed.** `CP_TELEMETRY_OTLP_ENDPOINT`
is unset, so the meter provider is a no-op and nothing is stored today. That is
exactly the argument for closing it now: the moment an endpoint is configured,
these become a memory leak on a 512 MB instance, and the fix would then be
competing with an incident.

**Fix.** `knownMethod` folds anything outside the method set into `_OTHER` —
the same spelling `otelhttp` uses eighty lines away, because two label sets on
one request should agree about what a method is. And the handler passes
`WithServerName(operation)`: this service serves one operation, and a metric
label is not where a deployment learns its own hostname.

**Not changed, and worth saying:** `route` is the chi template or the constant
`"unmatched"`, with no handler bypassing it, and `status_class` has four values.
Both were already right, and the audit checked them rather than assuming.

**Evidence.** STATIC_PROOF, with a unit test over the fold: every real method
survives unchanged, and an empty string, a lowercase spelling, a WebDAV verb and
a 4 KiB token all become one label.

## F-118 · Nothing was told anything · NEW · P2 · FIXED

**Found by** the observability audit, sweeping for anything in the tree that
reaches a human.

`Metrics.Raise` — the function every reconciliation alert goes through —
did three things, and in the deployed system **all three were dead ends**:

| what it did | why nothing came of it |
|---|---|
| incremented an in-process atomic | no exporter read it; the process discards it on exit |
| added to an OTel counter *if* a `FinancialMetrics` was attached | both composition roots passed `NoopMetrics()`, so `observability.NewFinancialMetrics` had **no caller outside its own tests** |
| called an observer *if* one was registered | `Metrics.OnAlert` has **zero callers in the repository, tests included** |

And it wrote **no log line at all**. So `ledger_integrity_violation`,
`unknown_submission`, `unauthorized_signing_candidate`,
`observer_disagreement` and `unknown_transaction` each incremented a number in
RAM and then vanished.

Three independent reasons, so fixing any one changed nothing — which is why
they survived. The `infra/terraform` alarms bound to those instruments compound
it: they set `treat_missing_data = "notBreaching"`, so a metric that never
arrives resolves to **OK**. Applying that Terraform today would show a wall of
green alarms over a system emitting nothing, which is worse than a blank
dashboard.

### What is fixed

**An alert is said out loud.** `Raise` logs every alert, SEV1 at ERROR so it is
separable from ordinary traffic. A log line is not paging and this does not
claim to be — nothing in this deployment pages. It is the difference between an
incident that is findable in the service's logs afterwards and one that left no
trace at all, and it is the only one of the three dead ends that needs no
external service.

**The instruments are constructed.** Both roots now build
`observability.NewFinancialMetrics` from the global meter. That meter is a no-op
while `CP_TELEMETRY_OTLP_ENDPOINT` is unset, and that is the point: this removes
the reason that is SOFTWARE and leaves the one that is a deployment decision,
rather than leaving both and calling the result instrumented. A failure to build
instruments logs and falls back rather than refusing to start — the alternative
to a metric is not a stopped service.

**Order mattered.** This is only safe because F-117 bounded the caller-chosen
metric labels first. Arming instruments on a 512 MB instance with an unbounded
label would have replaced a silent control with a memory leak.

### The larger half, done

**Something on a timer.** `cmd/api/reconverify.go` runs `VerifyInternal` —
Σ journal entries against `ledger_balances`, Σ active reservations against
`asset_reservation_totals`, envelope allocation against its flow — and then
`SweepEscalations`, once at start and every five minutes for as long as the
process is up: the same cadence `cmd/reconciliation-worker` uses on the paid
tier, in the process the launch tier actually runs. It is the pass that
produces the SEV1 this finding is about, and it needs only the database. It
deliberately does not run the execution and funding sweeps: this engine is
resolution-shaped — no attempts repository, no chain observers — because the
worker owns detection against the chain and the API owns the operator's answer
to it. Bolting observers onto the API to make those sweeps run there would be a
different architecture, not a ticker; and the launch tier enables
`CREDIT_PURCHASE` only, so there are no attempts to sweep.

**Somewhere to send it.** `internal/alert` is `Metrics.OnAlert`'s production
caller. A webhook and not a vendor, because the constraint is "$0 and it must
leave the process" and an outbound HTTPS POST is the only shape that satisfies
both without choosing for the operator: Slack and Discord incoming webhooks,
ntfy, a Cloudflare Worker all accept one on a free tier. Three rules it will
not break, each with a test that fails when it does: an alert never blocks a
money-moving transaction (bounded queue, non-blocking enqueue, a full queue
drops and says so); only allowlisted fields leave the process (`account_id`,
`attempt_id`, `blocks_new_risk`, `deposit_id`, `material`, `order_id`,
`signature` — derived from the raise sites, everything else dropped and
counted); and silence is reported (no destination logs WARN naming the
consequence, and `config.Validate` refuses STAGING and PROD without one —
`RuleAlertDestination` — so a deployment that forgets it does not boot
silently unalerted; it does not boot).

**In the shape the destination accepts.** The first draft posted the event as
JSON to whatever URL it was given, and Slack and Discord answer that with a
400: "accepts a POST" is not "accepts any POST". A destination that rejects
every alert is this finding with a URL attached. The sink now speaks
`{"text":…}` to Slack (and Mattermost), `{"content":…}` to Discord under its
2000-character cap, a text body with `Title`/`Priority`/`Tags` headers to
ntfy, and the event as JSON to anything else — chosen from the host unless
`CP_ALERT_WEBHOOK_FORMAT` says otherwise, and named in the startup line so an
operator who pasted a Slack URL can see `(generic)` and know why. ntfy's
acceptance of the headed text POST was checked against `ntfy.sh` with a
synthetic event (HTTP 200, title/priority/tags echoed); Slack's and Discord's
shapes are their documented ones and are pinned by test.

**The Terraform clause.** The five application alarms keep
`treat_missing_data = "notBreaching"`, and correctly — a mismatch counter that
never arrives is a system with no mismatches. What they could not tell apart
was that from a system whose instruments were never constructed, which for a
year they were not. `verification_passes` is a heartbeat: one count per
completed `VerifyInternal` pass, and a `verification-heartbeat-missing` alarm
with `treat_missing_data = "breaching"` (fifteen minutes, three passes, SEV2)
fires when nothing is verifying — pass broken, exporter down, or instruments
not built — and says that every counter-based SEV1 is blind while it does.
`test/infra` now also proves every alarm bound to the application namespace
names an instrument some Go file constructs, in either construction style the
tree uses; the first draft of that test saw only one style and reported the
four relay metrics missing, which was the test's gap, not the tree's.

**A smaller thing the same test found.** `Raise` logs through
`LoggerFrom(ctx)`, which falls back to `slog.Default` when nothing put a
logger on the context. Requests get one from the middleware; the tickers ran on
the root context, which had none — so the "alert is said out loud" line from
a background pass bypassed the JSON logger the deployment reads. The root
context now carries it.

**What is not code.** The URL. `NODAL_ALERT_WEBHOOK_URL` is a dashboard
secret in `render.yaml` (`sync: false`, like every other credential there)
and the next deploy of this build does not go live until it is set: the
process refuses configuration, never answers `/v1/healthz`, and Render keeps
the previous deploy serving. That is the safe direction and it is stated
rather than softened. The paid-tier inventory the original sweep also turned
up — `internal/notification` has no production caller, the Temporal
escalation workflow has nothing starting it, `reconciliation.record.transitioned`
has no subscriber — is not this finding's defect (nothing pages) and is not
runnable on a tier with no Temporal and no relay; it stays recorded under
F-69.

**Evidence.** STATIC_PROOF for the three dead ends, each confirmed by grep
for callers. TEST_UNIT: `internal/alert` (never blocks under a wedged sink,
drops and counts; allowlist drops and counts unknown keys; non-2xx is a
failure; severity floor; Close drains; nil dispatcher usable; each format's
body and headers; host detection; bounded summary), `cmd/api/alerts_test.go`
(a `Raise` reaches the sink through `OnAlert` with the allowlist applied; the
composition root calls `attachAlertDispatcher`, constructs the dispatcher,
closes it on both shutdown paths, and puts the logger on the root context; no
destination is said at WARN naming the variable; the delivery timeout is
trimmed to the shutdown budget and the destination is named without its
secret), `internal/config` (the variable set, the golden `.env.example`, the
STAGING refusal), `internal/observability` (the heartbeat instrument name),
`test/infra` (every application alarm names a constructed instrument; the
heartbeat alarm breaches on missing data). TEST_INTEGRATION:
`TestIntegration_DriftInTheDatabaseReachesTheWebhook` — a `ledger_balances`
row written behind the ledger's back as the schema owner, the API's own
`verifyOnce`, the seam, the dispatcher, and a webhook that receives a SEV1
`ledger_integrity_violation` naming the record, which exists in
`reconciliation_records` as MISMATCH; and the clean pass before it delivers
nothing. `terraform validate` in dev, staging and prod; `terraform fmt
-check`. LIVE: the deployed STAGING service observed healthy before this
change; the change has not been deployed, and the reason is stated above.

## F-119 · An as-of read that could look ahead · NEW · P3 · FIXED

**Found by** the point-in-time audit, checking which queries bound knowledge
time and which bound only event time.

`asset_prices` stores both: `observed_at` is when the price happened,
`received_at` is when this platform learned it. `PriceStore.Latest` bounded only
the first — `received_at` appeared solely as an `ORDER BY` tiebreaker — while
its own doc comment said:

> Observations after now are ignored so that an as-of valuation never looks
> ahead.

So a price observed at T-1m and **inserted** at T+5m was returned by
`Latest(..., now=T)`: an as-of read answering with something the platform did
not know yet.

**Latent rather than live**, and the finding is only worth its P3 because of
that: the sole production caller passes a live clock, and at a live `now` a row
with a future `received_at` does not exist. It stops being latent the moment
anything replays as of a past instant — which is the entire reason
`received_at` is stored at all. The same table's other reader,
`prediction/outcome.go`, already filters on it.

**Fix.** `AND received_at <= $4`. It changes nothing for a live clock, which is
what makes it safe, and closes the replay case.

### The existing test was asking the question the defect answered

`TestIntegration_PriceStore_LatestAndStaleness` recorded three observations —
all received at `now` — and then asked `Latest(..., now-1m)`, expecting an
answer. Under the knowledge-time bound the honest answer to "what did we know a
minute ago" is "none of this", so the test failed.

It failed correctly. The assertion was the defect in miniature: an as-of read
returning rows the platform had not yet received. Its actual point was about
ORDERING — that `observed_at` wins over `received_at` — and that point is now
made at an instant where the rows are known, with the fresher observation
excluded by the window rather than by the as-of bound. A second assertion states
the new rule directly: at an instant before these rows were received, nothing is
known.

**Evidence.** REAL_DB_INTEGRATION. The new test records an observation observed
before an as-of instant and received five minutes after it, requires the as-of
read to refuse it, and carries the control that the same row IS visible once the
as-of instant is past the moment it was learned — so the bound is on knowledge
time and not on the row.

## F-120 · Evidence that changed when you looked at it again · NEW · P3 · FIXED

**Found by** the point-in-time audit, the sibling of F-119 and the same
question asked of a different table.

`calibrationSourceSQL` bounds the window on `p.committed_at` — when the
prediction was made. `prediction_outcomes.resolved_at` is when this platform
learned how it turned out, and **nothing bounded it**. `Compute` already took a
`now` argument and used it only to stamp `ComputedAt`.

So recomputing a snapshot for the same scope and the same window a month later
folded in every outcome resolved in between, and produced a different answer.

**Why it matters more than a stale number.** `calibration_snapshots` rows are
cited as `CALIBRATION_SNAPSHOT` promotion evidence — the evidence an agent's
promotion to a higher stage rests on. Evidence that changes when you look at it
again is not evidence.

**Fix.** `AND o.resolved_at <= $7`, bound to `now`.

### Three fixtures were recording outcomes the platform could not have had

`Outcome.Validate` refuses a resolution before its horizon ends. Three
calibration tests committed a prediction with a one-hour (or one-minute)
horizon, advanced the clock by seconds, and stamped `resolved_at` at
`horizonEnd + 1m` — an hour or two in the **future** relative to the instant
they then computed at.

Nothing compared `resolved_at` to anything, so it went unnoticed. Bounding
knowledge time is what surfaced it, and the fix is the one reality imposes:
`resolveIt` now moves the fixture clock to the resolution instant, one test
advances past its own horizons before computing, and the windows widen from an
hour to a day because a prediction with a one-hour horizon cannot be resolved
two seconds after it is committed.

That is the third time in this batch that bounding a control revealed a fixture
describing something impossible — after F-103's providers on live credentials
in DEV and F-112's cookie domain in the "valid production" config.

**Evidence.** REAL_DB_INTEGRATION. The new test resolves one prediction
promptly and the second a month later, recomputes the SAME scope as of the same
instant, and requires the answer to be unchanged — with the control that asked
as of now the later outcome IS included, so the bound is on knowledge time and
the row is not simply invisible.

## F-121 · An approval born approved · NEW · P1 · FIXED

**Found by** the state-machine audit's INSERT-time sweep: which entities can be
born in a privileged state.

Dual control in this system **is** `admin_actions`. One principal proposes, a
different one approves, and the row is the record that both happened.

Every control on that table guards the UPDATE path:

| control | when it fires |
|---|---|
| `admin_actions_identity_frozen` | BEFORE DELETE **OR UPDATE** |
| `admin_actions_require_transition` | AFTER **UPDATE** OF status |
| `admin_actions_check` | `approved_by <> proposed_by` — satisfied by naming any second user id |

**None of them fires on an INSERT**, and `status` permits APPROVED and EXECUTED
among its seven values. So `cp_app`, which holds INSERT, could write a row born
APPROVED with an approver, an approval timestamp and an approver step-up already
filled in: two-person control forged outright, with no proposal, no second
person, and no transition row — because there was no transition.

**It is the missing half of a bigger forgery.**
`agent_lifecycle_transitions.approval_id` REFERENCES `admin_actions(id)`, so a
forged APPROVED row completes a fully-evidenced agent promotion to LIVE.

### The test that was deliberately not written

`TestVerifyApprovedReadsTheSpecNotTheRow` says so in its own comment:

> The behavioural version of this test is not written, deliberately. Reaching
> the branch means an APPROVED dual-control row with no approver … and that is
> exactly the forgery F-42 records, performed by a test. … A fixture that has
> to commit the exploit to reach the code is a fixture that should not exist.

That reasoning was right, and 00735 changes what it applies to. The forgery is
now refused at INSERT, so **testing the refusal is not committing the exploit**
— it is watching the exploit fail. Three forms are driven: born APPROVED with
an approver, born EXECUTED, and born PROPOSED with the approval chain pre-filled
(the subtle one, where the status is honest and the approval is not).

**Fix.** `cp_admin_action_born_proposed`, a BEFORE INSERT trigger: an admin
action is created PROPOSED, with no approver, no rejection and no execution
recorded. A trigger rather than a CHECK because a CHECK cannot tell an INSERT
from an UPDATE, and the UPDATE path is exactly where those columns are
legitimately filled in.

This is `cp_gate_born_disabled`'s treatment (00701) applied to the table dual
control actually lives in. `capability_gates` was the **only** entity in this
schema closed at birth; it is now the second of two, and `agents`,
`credit_fundings` and `payout_requests` remain open — recorded in
MASTER_BUILD_STATE's resume list rather than fixed here.

### It found a fixture committing the exploit

`internal/agent`'s `approveFor` INSERTed an `admin_actions` row **directly as
APPROVED** to stand in for a promotion approval, and six tests depended on it.
The birth control refused it, which is how the fixture was found.

That is the same shape `internal/admin`'s comment had already named — "a
fixture that has to commit the exploit to reach the code is a fixture that
should not exist" — sitting in a different package, where that comment could
not see it. It now proposes as one principal and approves as another, with the
transition row the move requires, written in SQL because the authority boundary
forbids `internal/agent` from importing `internal/admin`.

**That is the fourth fixture in this batch that encoded the defect it was meant
to guard against**, after F-103's fourteen providers on live credentials in DEV,
F-112's cookie domain in the "valid production" config, and F-120's outcomes
resolved before their horizons closed. The pattern is worth stating plainly: a
suite's known-good fixture is an assertion about what is safe, and nothing was
checking it. Closing a control is how you find out.

**Evidence.** REAL_DB_INTEGRATION. All three forgeries observed refused with
AD001 as the real `cp_app` role, and the control — an honest proposal created
and then approved through the real service by a second principal — observed
passing, which is what proves the birth control is not one condition too strict.

## F-122 · Nothing is born finished · NEW · P1 · FIXED

**Found by** the state-machine audit's INSERT-time sweep, the same one that
produced F-121.

Every binding in this schema is about CHANGES. 00603 tied a state change to a
transition row; 00726 and 00731 made that row name both endpoints; 00732 stopped
the endpoints being forgeable; 00734 closed the exit from a terminal state.

**A row INSERTED in a privileged state never changed, so none of it applied.**
Until 00735, `capability_gates` was the only entity in the schema that could not
be born decided. These are the remaining three where the birth state means money
or authority:

| table | born as | what that is |
|---|---|---|
| `agents` | LIVE/LIVE with an envelope | the entire promotion ladder skipped at INSERT — every evidence and approval CHECK on `agent_lifecycle_transitions` guards a **transition**, and creating an agent is not one |
| `credit_fundings` | SETTLED with a `lot_id` | `LotFinalityFor(SETTLED)` is what makes value payout-eligible; minted from nothing |
| `payout_requests` | SETTLED with `settled_quantity` | `settled <= reserved <= requested` is satisfied by naming all three |

Each is created by exactly one statement in the tree, and each names a literal
birth state — `StageDraft`/`StateDraft`, `'CREATED'`, `'ELIGIBILITY_CHECK'` —
so the constraint is what the code already does, and everything after it is a
transition with a row behind it.

### I got the first attempt wrong, and the suite said so

00736 added the agent trigger (correct) **and two CHECK constraints** asserting
that a funding's lot and a payout's money agree with its state. Both are wrong.
The integration suite failed within minutes, and the reason is a fact about the
money path worth recording:

- `MintFrom` calls `AdvanceFunding(REVERSIBLE)` **first** and writes `lot_id`
  **second**, so between those statements the row is REVERSIBLE with a NULL lot.
  The ordering is deliberate: the advance carries the transition row AU001
  requires, and the lot id is the idempotency marker that makes a second mint a
  no-op. Reversing them to satisfy a constraint would put the marker before the
  state change it marks.
- `reserve` posts the ledger entries and writes `reserved_quantity` while the
  request is still in ELIGIBILITY_CHECK, and transitions afterwards. Same shape.

A row-lifetime CHECK was the wrong instrument for a birth-time rule. 00738 drops
both with that reasoning written into it, rather than weakening them until they
passed.

**And the second attempt was still incomplete.** The dropped CHECK said a
SETTLED funding HAS a lot — which a row born SETTLED **with** one satisfies. It
refused a settlement of nothing; it never refused a settlement minted from
nothing. 00737's birth triggers are what close that.

**Fix.** Three BEFORE INSERT triggers (00736 for agents, 00737 for the other
two), each constraining creation and saying nothing about the orderings above.

### And the third attempt narrowed them, which is the part worth reading

Those triggers required each table's exact creation state — DRAFT/DRAFT,
CREATED, ELIGIBILITY_CHECK. That is what the code does, and "what the code does"
is a defensible default for a birth control. It is not the property being
protected, and the difference cost **seven integration packages**.

What those packages were doing is not the forgery: `internal/capital` seeds an
agent VALIDATED with no mode and no envelope; `internal/intent`,
`internal/nativemarket` and `internal/prediction` seed one SHADOW in SHADOW
mode; `internal/capacity` seeds a funding CAPTURED with no lot; and
`internal/httpapi` seeds a payout DRAFT — which is **earlier** than the state
the trigger demanded, not later. None of them mints value, moves real capital or
forges an approval.

Migration 00739 restates all three against lines this schema already draws:

| table | refused at birth | the line it uses |
|---|---|---|
| `agents` | CANARY/LIMITED/LIVE stage or mode, or an envelope | `agent_lifecycle_transitions_check1` already names those three as the approval-bearing stages; `Mode.RealCapital()` names the same three; `NewAuthority` already refuses a real-capital stage with no envelope |
| `credit_fundings` | REVERSIBLE, SETTLED, DISPUTED, REVERSED, REFUNDED, or a lot | `LotFinalityFor` returns a finality for exactly those states — they are the ones that assert value exists |
| `payout_requests` | anything past the four pre-reservation states, or anything reserved/settled/submitted | the reservation is where money moves |

Every forgery is still refused, and one more besides: an agent born SHADOW but
in LIVE **mode**, which the creation-literal rule caught only incidentally and
this one catches on purpose.

**What it deliberately gives up.** An agent can now be born SHADOW, and SHADOW
requires promotion evidence — strategy version, IR hash, risk policy hash,
evidence hash — which a row born there has none of. That is a **provenance**
gap: nothing records what the agent was validated against. It is not a **money**
gap, because a SHADOW agent cannot move real capital: the mode, the envelope and
every real-capital stage are each refused above.

Recorded as the residual rather than closed, because closing it means either
rewriting five fixtures to walk the promotion ladder or accepting that a test
seeding a shadow agent must carry evidence hashes — a decision about how this
suite is built, not a defect in the schema.

**Evidence.** REAL_DB_INTEGRATION. All four forgeries — the three here plus
F-121's — observed refused with AD001 as the real `cp_app` role, in a test that
writes them with random foreign keys **on purpose**: a BEFORE INSERT trigger
fires before the keys are checked, so a refusal is the birth control speaking
and not an accident of the fixture, and the assertions name both the SQLSTATE
and the message to be sure.

The test also carries a **negative control naming what is still open**:
`wallets`, `assets` and `instruments` have no birth control, and the assertion
fails if one gains one — so closing the next of them forces this list to be
updated rather than quietly diverging.

## F-123 · Six seeds pinning the property tier to bugs that are fixed · NEW · P3 · FIXED

**Found by** noticing an untracked `testdata/rapid/….fail` in this session's own
working tree, and then asking how many others were tracked.

`rapid` writes a `.fail` file recording the seed of a failing property run, and
**replays it on every subsequent run of that property**. That is exactly right
while a bug is open. Once it is fixed the file pins the suite to a degenerate
case forever, and nobody can read a timestamped seed to tell which.

Six were committed: one written this session by the over-broad birth control of
F-122 (it records `AGENT_BORN_PROMOTED: an agent is created DRAFT/DRAFT`, a rule
migration 00739 replaced, so it describes a refusal that no longer exists), and
five from 2026-09-06 recording failures that have since been fixed — confirmed
stale by the 51/51 integration pass, which runs every one of those properties.

**This repository had already decided the question**, in F-30, in this same
file:

> The stale `testdata/rapid/…fail` file rapid wrote is deleted: left in place it
> pins every future run to the degenerate case.

The decision was recorded and then not applied to the other five, and nothing
stopped a sixth arriving. All six are deleted and `**/testdata/rapid/**/*.fail`
is now ignored, with the reasoning in `.gitignore` beside it.

The rule the ignore states: **a property counterexample worth keeping is worth
writing as a named test**, not left as a timestamped seed file nobody can read.

**Evidence.** STATIC_PROOF for the mechanism, REAL_DB_INTEGRATION for the
staleness — all five older properties are integration-tagged and all five ran
and passed in this session's gate, so none of the seeds still reproduces.

## F-124 · The documented webhook path is not the registered one · NEW · P1 · FIXED

**Found by** the abuse-surface audit, and verified here against the deployment.

`openapi.yaml` declared the webhook provider parameter as
`enum: [stripe]`. `cmd/api` registers the port under
`stripecredit.ProviderName`, which is **`stripe_credit`**.

Nothing enforces the enum at runtime — no OpenAPI request validator is wired
anywhere in this repository, which is its own recorded observation — so a
delivery to the **documented** path reaches the handler, finds no provider in
the map, and answers 404 before writing anything.

**Observed against `https://api-nodal.actorvia.xyz` on 2026-09-10:**

```
POST /v1/webhooks/stripe                     -> 404
POST /v1/webhooks/definitely-not-a-provider  -> 404
```

The path the contract publishes is byte-identical in behaviour to an invented
one.

**What that costs.** Stripe retries a 404 for a few days and then gives up. A
delivery that is given up on is a payment whose outcome never reaches this
system — money captured and Credits never minted, with the reconciliation
sweep as the only backstop and no alert to say it happened. `cmd/api/wire.go`'s
own comment says the mounting exists to avoid exactly that.

**Whether the live Stripe endpoint is configured at the right path is
BLOCKED_EXTERNAL** — it is a value in Stripe's dashboard and cannot be read
from here. The defect is real regardless: anyone configuring it from the
published contract configures the wrong one.

**Fix.** The contract now declares the key the service registers. Corrected in
that direction, not the other, because `ProviderName` is the provider key
throughout — `provider_events.provider`, the evidence archive keys, the inbox
namespace — and changing it would change the meaning of stored rows.

`TestWebhookPathIsTheOneTheContractPublishes` reads the enum out of
`openapi.yaml` and the constant out of `stripecredit/wire.go` and requires them
to be the same set. Two strings that have to agree, in two languages, is the
defect class this register keeps recording; this is the test that makes them
agree. Verified non-vacuous by putting `stripe` back and watching it fail.

**Evidence.** LIVE_OBSERVED for the 404s, STATIC_PROOF for the divergence,
BLOCKED_EXTERNAL for what Stripe is actually configured with.

## F-125 · Every race claim rests on CI · NEW · P3 · FIXED

**Found by** trying to run the race detector, which the goal document names as
available on this host.

It is not. `go test -race` fails to LINK for every package in `RACE_PKGS`:

```
ld.exe: error: linker script file 'C:/Users/Mihir (C:\Users\MIHIRM~1\...\fix_debug_gdb_scripts.ld)'
        appears multiple times
collect2.exe: error: ld returned 1 exit status
```

The GCC installation path contains a space (`\Users\Mihir Modi\...`) and this
binutils — GCC 16.1.0, recent — splits the linker-script argument on it.
Attempted and rejected: `GOTMPDIR` on a space-free path (the split is on GCC's
own location, not the temp dir); `-ldflags=-w` to skip DWARF (the script is
still emitted); and a directory junction at `C:\Dev\mingw64` (GCC resolves
through it to the real path). The junction was removed afterwards.

**Why it is recorded rather than shrugged at.** `make race` and the per-package
integration-tagged race sweep both run in CI on Linux, so the coverage exists
— but it means **no race claim in this repository can be checked from this
host**, and a session working here cannot verify a concurrency fix it makes.
That is worth knowing before someone assumes otherwise, and the goal document
assumed otherwise.

The fix is a GCC whose path has no space. It is a host change, not a repository
one, which is why this is OPEN rather than fixed.

**Evidence.** LIVE_OBSERVED on this host — the failure above is the actual
output, reproduced for three packages and three workarounds. STATIC_PROOF that
CI runs both race tiers (`ci.yml` lines 128 and 312-327).


### Fixed 2026-09-10: a COPY works where a junction did not

The rejected workarounds were rejected for the right reasons, and one of them
was nearly right. A directory junction at a space-free path failed because a
junction RESOLVES: GCC follows it back to its real location and the space
returns. A plain file copy does not resolve.

    robocopy "<WinGet packages>/.../mingw64" C:/toolchain/mingw64 /E     (0.89 GB)
    CC=C:/toolchain/mingw64/bin/gcc.exe  CXX=.../g++.exe  go test -race

An 8.3 short path was tried first and does NOT work, for the same reason the
junction did not: the short-path gcc.exe runs, then reports its own location as
`C:/Users/Mihir Modi/...` and splits the linker-script argument on the space
exactly as before. The distinguishing property is not "a path without spaces"
but **"a path GCC resolves to without spaces."**

**Both race tiers now run on this host and both are clean.**

    make race               7 packages, unit,             0 data races
    make integration-race   12 packages, a database each, 7m27s, 0 data races

So the sentence this finding existed to force into the open — "no race claim in
this repository can be checked from this host" — is no longer true, and the race
claims in the checkpoint are LIVE_OBSERVED here rather than resting on CI alone.

Reproducing it needs one environment-variable pair pointing at a toolchain copy,
recorded in `docs/operations/BACKUP_RESTORE.md` with the other host facts a
fresh session needs. It is deliberately not wired into the Makefile: `CC` is
host-specific and CI's Linux toolchain needs no such thing.

## F-126 · Five spellings of one archive key · NEW · P2 · FIXED

**Found by** the fuzz tier, unprompted — `go run ./scripts/fuzzall
-fuzztime=10s`, on the last gate run of this session, discovered and persisted
a new failing input for `internal/archive.FuzzParseKey`:

```
string("0/0/0/v00001/2026/09/05/13/1788613200000000000-0.0")
```

The round-trip property says anything `ParseKey` accepts must re-render to a key
containing the same partition. `v00001` parsed to `SchemaVersion: 1`, which
`PartitionKey` renders as `v1`:

```
layout_test.go:157: partition "0/0/v1/2026/09/05/13"
                    not in key "0/0/0/v00001/2026/09/05/13/1788613200000000000-0.0"
```

Enumerated from there, `strconv.Atoi` accepted **five** spellings of schema
version 1 — `v1`, `v01`, `v0001`, `v00001` and `v+1` — and there are infinitely
many more.

**What that costs.** An archive key IS the identity of the object. Dedup
compares keys; retention lists a prefix; an audit reconstruction lists a prefix.
A second spelling of the same key is an object that dedup does not see, that a
retention sweep listing `.../v1/` does not delete, and that an auditor asking
for schema version 1 does not find. This is the "same object, two keys" hazard
that `SegmentPattern`'s own comment already refuses for casing, arriving through
the one segment that comment does not cover.

**Not exploitable through any writer in this tree.** `PartitionKey` is the only
renderer of archive keys, and `%d` emits the canonical spelling; a grep for
other key writers found none. The exposure is a key arriving from outside — a
restore from a mirror, a hand-written retention argument, a migrated bucket —
which is exactly the direction `ParseKey` exists to face.

**Fix.** `canonicalUint` requires the digits after `v` to be the one spelling
`%d` produces: no sign, no leading zeros. It is the same rule the date partition
below it already gets, and the comment says so, because these are one defect
found twice.

`TestParseKey_OneObjectHasOneKey` states the rule the corpus file only
illustrates: eight aliases of one canonical key, four schema spellings and four
unpadded dates, each required to be refused. Verified non-vacuous by disabling
the guard and watching it fail on the first alias.

**Why P2 and not P3.** The archive is the evidence of record for provider
truth, and `POINT_IN_TIME.md` §2 makes prefix listing the reconstruction
mechanism. A silently unlistable object is a hole in the evidence, not a
cosmetic parse bug. It is not P1 because nothing in this repository writes such
a key today.

**Evidence.** LIVE_OBSERVED — the failing input was produced by the fuzzer on
this host, preserved as a corpus entry before the fix, and re-run against the
fix. STATIC_PROOF that no writer in the tree emits a non-canonical key.

## F-127 · The ADR got the fact its decision rested on wrong · NEW · P2 · FIXED

**Found by** closing F-105's remaining half, and only because the schema was
checked before the migration was written rather than after.

ADR-0020's classification table places `security_events` under OPERATIONAL_LOG
and records that it does **not** refuse DELETE — grouping it with
`login_attempts`, whose retention is a plain DELETE on a ticker (F-79).

It does refuse. The trigger is `BEFORE DELETE OR UPDATE`, and it was reproduced
as the table OWNER, which is the strongest caller there is:

```
DELETE FROM security_events WHERE kind = 'TEST_PROBE';
ERROR:  immutable row: DELETE on public.security_events is forbidden
CONTEXT:  PL/pgSQL function forbid_mutation() line 3 at RAISE
```

**What that costs.** ADR-0020 decision 2 says retention on a table that refuses
DELETE must be partition detachment. Decision 1 says a retention scheme must
never disable a trigger or grant DELETE on such a table, permanently. An
implementer following the uncorrected table would have written the DELETE
version — which fails at runtime, and whose obvious next step is the one
decision 1 forbids. The document that exists to prevent that outcome was
pointing at it.

**Why P2 and not P3.** The ADR is the standing instruction for three retention
classes that are still unimplemented. A wrong fact in it does not decay; it
waits.

**Fix.** The row is corrected, and the correction is written into the ADR with
the reproduction rather than silently edited, because "the table was wrong" is
less useful to the next reader than "this is how it was caught."

**The transferable part.** It was not caught by review — the ADR was reviewed
and accepted the day before. It was caught by checking the schema before writing
the migration, which is the only thing that would have caught it. The same
sentence covers the six fixtures this session found: **a document, a fixture and
a comment are all claims, and this register keeps finding that nothing was
checking them.**

**Evidence.** `REAL_DB_INTEGRATION` — reproduced against PostgreSQL 16 as the
owner, before anything was changed. `STATIC_PROOF` for the trigger definition.

## F-128 · The application could mint a transition flag from a table of its own · NEW · P1 · FIXED

**Found by** trying to close F-42, and only because the fix was attacked before
it was written.

F-42 records one route to forging the audit binding: set the `cp.transition.*`
or `cp.edge.*` GUC by hand. There is a second, and it is worse. `cp_app` holds
`TEMPORARY` on the database, and the flag setters were EXECUTE-able by PUBLIC,
so it could attach the real setter to a table it owns:

```
CREATE TEMP TABLE forge (account_id uuid, from_status text, to_status text);
CREATE TRIGGER forge_flag AFTER INSERT ON forge FOR EACH ROW
  EXECUTE FUNCTION cp_flag_transition_edge('account_id','from_status','to_status','accounts');
INSERT INTO forge VALUES ('<id>', 'ACTIVE', 'FROZEN');
UPDATE accounts SET status = 'FROZEN' WHERE id = '<id>';   -- committed
```

Reproduced as `cp_app` against this schema. The account froze;
`account_status_transitions` gained no row.

**Why it is P1 when F-42 is P2.** Not because the outcome differs — both forge a
state change on seventeen audited tables with no audit row. Because of what it
does to the fix. **The obvious repair for F-42 is to make the flag's VALUE
unforgeable**, and this route forges the value using the real setter, so that
repair would have looked complete, passed every test written for F-42, and been
bypassed in three lines. A defect that defeats the fix for another defect is
worth more than either.

**Fix.** `REVOKE EXECUTE ... FROM PUBLIC` on all five transition functions and
on the new tag helper (00741). Existing triggers are unaffected because
PostgreSQL checks EXECUTE on a trigger function at **CREATE TRIGGER** time, not
when it fires — verified by applying the revoke and then driving a legitimate
transition through as `cp_app`, rather than by reading the manual.

`TestIntegration_TheApplicationCannotMintATransitionFlag` asserts no role but
the owner holds EXECUTE on any of the six, and then drives the route itself.
Proven non-vacuous by granting EXECUTE back and watching all three assertions
fail.

**And one thing the test found that the migration had missed.** The key table
arrived readable by `cp_readonly` and `cp_ops` with no GRANT written anywhere:
`ALTER DEFAULT PRIVILEGES` in this schema grants SELECT on every table
`cp_migrate` creates. **Writing no GRANT is not the same as granting nothing**,
and only the assertion caught it.

**Evidence.** `REAL_DB_INTEGRATION` — both the exploit and the refusal
reproduced against PostgreSQL 16 as `cp_app`.

## F-129 · A restore that passes every check and then refuses everything · NEW · P2 · FIXED

**Found by** the restore drill, within the hour of the fix that caused it.
00741 closed F-42 by computing the transition flag from a secret in
`cp_transition_key`, and in doing so made every state change on seventeen
tables depend on one row.

**What a lost key looked like.** `cp_transition_tag` was `LANGUAGE sql` and read
the secret with a scalar subquery. With no row that is NULL, so the tag is NULL,
so every comparison against it is NULL, so **every state change is refused.**
Failing closed is the right direction and was not the problem. What it said was:

```
ERROR: AUDIT_TRANSITION_REQUIRED: accounts <id> changed status
       ACTIVE -> FROZEN without a transition row describing that change
```

The caller wrote the transition row. The message says it did not. An operator
reading that goes looking for a bug in the code that writes transitions, and the
fault is a missing row in a table they have never heard of and cannot read.

**Why it is P2 and not P3.** The failure is total — no account can be frozen, no
funding can settle, no agent can be promoted — and it arrives at the worst
moment, immediately after a restore, with a diagnosis pointing away from the
cause.

**How it was found, which matters more than the fix.** Not by review. The
restore drill compared row counts, ledger balances and journal hashes, and **a
restore that brought back every row but lost this one table would have passed all
three.** None of them proves the restored database can still be USED, and until
00741 that was implied. It is not implied any more.

**Fix, in two parts.**

`cp_transition_tag` is now plpgsql and raises when the key is absent, naming the
fault and the remedy. Deliberately **not** SQLSTATE AU001: this is not an audit
failure, and a handler catching AU001 must not treat it as one.

The row is now undeletable — `forbid_mutation`, the same protection fifty-three
append-only tables have, refusing DELETE for every role including the owner.
UPDATE is left open so the key can be rotated; a rotation committed between a
setter and its verifier invalidates tags in flight, so it is a brief window in
which some transactions fail closed and retry. That is acceptable for a key.
Losing it is not.

**And the drill now proves the restored database works, not only that it
matches.** It drives one real audited state transition on the restored database
as `cp_app`, forcing the deferred trigger with `SET CONSTRAINTS ALL IMMEDIATE`
and rolling back. `CP_DRILL_BREAK=lose_the_transition_key` empties the table so
the probe can be watched firing — and the first attempt at that break failed
with `permission denied for table cp_transition_key`, which is its own small
confirmation that the application role cannot cause this fault. The second
failed on the undeletable trigger, which is the fix working against its own
test; the break now disables that trigger as the owner, which is the only way to
simulate a restore that never had the row.

**Evidence.** `REAL_DB_INTEGRATION` — the failure and both fixes reproduced
against a genuinely restored PostgreSQL 16 database, not a simulated one.

## F-130 · The register did to itself what it keeps finding · NEW · P3 · FIXED

**Found by** re-reading this register at the start of a session to recompute
which findings were still open, rather than trusting the numbers a previous
checkpoint carried.

Every finding's status appears **twice**: in the summary table at the top, and
in the heading of its own section. Nothing compared them. Four disagreed:

```
F-42   table=fixed  heading=open
F-85   table=fixed  heading=open
F-100  table=fixed  heading=open
F-105  table=fixed  heading=part
```

All four stale in the same direction — the work was done, the row was updated,
the heading was not. So a reader who scrolled to the detail of the audit
binding, the buffered request body, the un-parked funding or the unprunable
security trail was told the fix had not landed. **In the document this
repository keeps in order to be the thing that does not overstate.**

F-85's section went further than a stale heading: its body still read "Left open
deliberately," with the reasoning for leaving it open, under a row that said
fixed. The reasoning was true when written; the entry now says both, because
"we said we would not do this yet and then did it at the start of the next
batch" is more useful than either half alone.

**And five findings have a summary row and no section at all** — F-05, F-06,
F-08, F-09 and F-40. All fixed, all BASELINE-era, all predating the convention
that a finding carries its evidence. Reconstructing that evidence now would be
archaeology rather than audit, and inventing it would be worse than the gap, so
they are named in a closed list rather than tolerated by a rule.

**Why P3.** Nothing in the running system is affected. It is recorded because of
what it is an instance of: **a list duplicated in two places diverges, and the
copy nobody greps is the one that rots.** That is the defect class this register
names more often than any other, and it had it.

**Fix.** `TestDocs_EveryFindingSaysTheSameThingTwice` holds the table against
the headings in both directions — a heading may not disagree with its row, and a
section may not outlive its row. `TestDocs_EveryFindingCarriesItsEvidence`
requires a section per finding and fails if a new finding is added without one,
or if a name on the exception list ever gains a section or leaves the register.

Both were proven non-vacuous by reintroducing the defect: putting F-85's heading
back to OPEN, and deleting F-129's section.

**Evidence.** `STATIC_PROOF` — the register is a file, and the check is a
comparison of it against itself.

## F-131 · A fixture wrote rows the application cannot read · NEW · P3 · FIXED

**Found by** 00750's work on `agents`, and only because two packages happened to
share a database.

`internal/nativemarket/stage15_integration_test.go` built its scaffold ids with
`uuid.NewString()`, which is **version 4**. Every id in this system is version 7
(`id.New`), and `id.Parse` refuses anything else, so the fixture inserted agent
rows that the application's own scanner cannot read back:

```
agent: scan agent: can't scan into dest[0] (col: id):
  id: scan: id: not an rfc 9562 version 7 uuid: version 4
```

**Why nothing caught it, which is the part worth keeping.** `scripts/inttest`
gives every package its own database. This fixture's rows were therefore only
ever read by the package that wrote them, and that package never scans them as
agents. The isolation that makes the integration tier reliable is the same
isolation that hid a fixture writing unreadable rows, and it surfaced the first
moment two packages shared one database.

**Why P3.** No production path can produce a v4 id — `id.New` is the only
constructor — so nothing running is affected. It is recorded because it is the
fixture-shaped defect this register keeps finding, in a new place: **a fixture
that creates state the system could never create itself is not a fixture, it is
a different system.**

**Fix.** The scaffold uses `id.New[id.Any]()`. The other 232 `uuid.NewString()`
calls in the test tree were left alone deliberately: most produce idempotency
keys, correlation ids and provider references, where v4 is correct and no typed
id parses them. Changing them all would be churn, and would obscure the one that
mattered.

**Evidence.** `REAL_DB_INTEGRATION` — the scanner's refusal, observed against a
database holding the fixture's rows.

## F-132 · A public-route assertion that a 404 satisfied · NEW · P2 · FIXED

**Found by** reading `authz_test.go` while scoping F-84, and noticing that its
public-route list still named `POST /v1/webhooks/stripe` — the path F-124
established is **not** the one the service registers.

`TestNoRouteIsUnintentionallyUnauthenticated` walks every mounted route and, for
routes on an explicit public list, asserts the anonymous response is **not 401**.

A 404 is not 401. And the webhook probe was a 404, for two compounding reasons:

1. The route-probe substitution replaced `{provider}` with `"stripe"`.
2. The harness registered its fake webhook under `"stripe"` as well.

`stripecredit.ProviderName` is `"stripe_credit"`. So the harness and the probe
agreed with each other and **both disagreed with production** — and when the
probe was corrected to the real provider, it still 404'd, because the harness
had the old key too. The assertion had therefore never measured what it claims:
whether that route is reachable without a session.

**Why P2.** This is the test that exists to catch a newly generated route nobody
wrote an auth policy for. It works for every other route; it was blind on the one
route that is deliberately unauthenticated and takes money instructions from an
external party. A future change making that route require a session — or a
future route landing on the same blind spot — would not have been caught.

**Fix, in three parts.**

- The harness registers its webhook under `stripecredit.ProviderName` rather
  than a literal, so the key cannot drift from production again. Three router
  tests that posted to the literal path now build it from the same constant.
- The public branch asserts the probe **reached** the route:
  `assert.NotEqual(http.StatusNotFound, ...)` with the reason in the message. A
  declared-public route that 404s is a claim nobody checked.
- Proven by construction: the strengthened assertion fails against the previous
  provider key, naming the route and saying the earlier assertion was vacuous.

**The transferable part is the assertion shape, not the provider name.**
`NotEqual(401)` is satisfied by every status except one. An assertion whose
negative space is that large is not measuring the thing it is named for. This is
the same defect as "a test that passes because a different guard fired", written
in the language of HTTP status codes.

**Evidence.** `REAL_DB_INTEGRATION` — the 404 and the corrected pass both
observed in this package's harness.

## Findings deliberately NOT raised

Stated so their absence is a decision rather than an oversight:

- ~~**The chart of accounts is duplicated between Go and SQL.**~~ **RAISED, twice.** This entry said
  the duplication was "a maintainability risk, not a defect". It became both: F-43, where MARKETPLACE
  was high-risk in Go and not in SQL so the database evidence check never fired for it, and F-49,
  where the negative-balance policy was in Go and unenforced in SQL. Both halves are now compared by
  a test that drives each language's copy — `TestIntegration_GoAndSQLAgreeOnEveryCapabilitysRisk` and
  `TestIntegration_GoAndSQLAgreeOnTheChartOfAccounts`. The account-code and value-domain lists were
  already covered by `TestIntegration_GoAndSQLAgreeOnEveryOrderedDomainPair` and
  `TestIntegration_AccountDomainDerivationAgreesWithGo`; what remains uncovered is the transaction
  kinds, the credit origins and the finality transition table, and two of those carry comments
  claiming a test that does not exist.
- **Capability activation is not enforced in the database.** Migration 00710 says so explicitly and
  gives the reason: a gate is keyed by environment, and a connection carries no environment the
  application could not simply assert. Adding a session GUC would look like a control and be none.
- **`internal/compliance` is 248 lines with one test.** It is the thinnest financial package in the
  tree and was already flagged in the KEEP/MODIFY matrix as needing a real `ComplianceDecision`. It
  is under-built rather than wrong.
- **`internal/instruments` has 593 lines and one test function** for a package with status authority
  over what may be traded. Under-tested for its authority; not observed to be incorrect.

## F-133 · Expired sessions were never purged · BASELINE · P2 · FIXED

**Found by** the F-47 work, checking what `cp_ops` actually does with
`sessions` before deciding what it may read.

Migration 00011 granted `cp_ops` DELETE on `sessions` and said *"Expired-session
purge is an operations job (auth/pgstore.PurgeExpired)"*. `PurgeExpired`
exists, is tested, and had **no caller outside its own test** on any tier —
not `cmd/api`'s retention loop, not `cmd/audit-worker`. So every session ever
issued — token hash, roles, `break_glass_until`, IP, user agent — was kept
forever, on a database whose ceiling halts every financial action when it
fills (F-105), readable by two roles that had no use for it (F-47).

The same shape as F-79 (login attempts) and F-105 (security events): a job
that exists, is documented as someone's, and is nobody's. Three times in one
subsystem is a pattern, and the pattern is that "operations job" named a role
and not a process.

**Fix.** A third pass in `runOpsRetention`: `PurgeExpired` after the
login-attempt window, on the same hourly ticker as the login-attempt purge and
the partition maintenance. The window is reused rather than a new variable
added — both are transient authentication rows whose only afterlife is
forensic, and one number for "how long after expiry does an auth row survive"
is easier to reason about than two. Migration 00754 narrows `cp_ops` to SELECT
on `expires_at` alone, which is exactly what the DELETE filters by, and the
purge is proven to still run under that grant.

**Evidence.** STATIC_PROOF: `grep -rn "PurgeExpired(" cmd/ internal/` — the
definition, nothing else. TEST_INTEGRATION:
`TestIntegration_ExpiredSessionsArePurgedAsOps` — a session expired beyond
the window is deleted by `cp_ops` under 00754's column grant, one inside the
window survives, `SELECT expires_at` as `cp_ops` works and `SELECT token_hash`
is refused with `permission denied`.

## F-134 · The Go e2e suite could not sign in, so every test behind a sign-in had failed since F-87 · PRODUCTIZATION · P2 · FIXED

**Found by** the productization test-matrix run (`make e2e`) on the merged
tree, then reproduced on the audited checkpoint `024c691` to tell a regression
from a baseline defect: it is the baseline.

F-87 bound `GET /v1/auth/callback` to the browser that began the flow by a
login-state cookie set on the login redirect. `test/e2e`'s client holds no
cookie jar by design (each hop's Set-Cookie is read explicitly), and its
`signIn` helper never carried that cookie to the callback, so every sign-in was
refused with `UNAUTHENTICATED: this sign-in did not start in this browser` and
all six e2e tests failed. CI's last run on `main` (16cba60) is red on exactly
this; the checkpoint's local matrix did not include `make e2e`.

Three further expectations had gone stale behind the sign-in failure and were
found once it was fixed: an oversized body is refused by the transport budget as
`413 BODY_TOO_LARGE` (the test expected 400); the stream's first frame is the
`retry:` hint (the test accepted only a comment or an event block); and the
reconciliation resolution the "unwired capability" probe aimed at has been wired
since the probe was written.

**Fix.** The client carries the login-state cookie by hand from the login
redirect to the callback, as a browser would; the three expectations follow the
contract as it is; the unwired probe aims at `GET /v1/credits/pricing`, which the
e2e API child genuinely runs without a provider for. Commit 2521945.

**Evidence.** TEST_E2E: `make e2e` — 6/6 on a fresh database (was 0/6).
STATIC_PROOF: `test/e2e/client_test.go` (`loginState`), `errors_test.go`,
`stream_test.go`.

## F-135 · The chaos purchase world set a platform fee the service overwrites · PRODUCTIZATION · P3 · FIXED

**Found by** the same matrix run (`make chaos`), reproduced on `024c691`.

`commerce.CreateProduct` assigns the service's platform fee to every product
(`DefaultPlatformFeeBPS` is 0) and ignores the literal's `PlatformFeeBPS`; the
chaos world set 1,000 bps on the literal and asserted seller proceeds of 900 on
a 1,000 price, so `TestChaos_APurchaseDiesMidTransaction` failed on its final
assertion after proving everything it was written to prove about the killed
transaction. No product behaviour was wrong.

**Fix.** The world sets the fee on the service (`SetPlatformFeeBPS(1_000)`)
before creating its product. Commit 2521945.

**Evidence.** TEST_CHAOS: `make chaos` — green on a fresh database.

## F-201 · The withdrawal bar drew one Credit in two segments · PRODUCTIZATION · P1 · FIXED

**Found by** the frontend audit (goal §54), reproduced in
`apps/web/e2e/audit-frontend.spec.ts` — "the withdrawal bar does not count one
Credit in two segments", which sums the shares the bar states in its own
accessible label and finds them over 100%.

`/withdraw` drew three segments — `payout_eligible`, `ineligible` and `frozen` —
against `gross`. The first two ARE the whole: `internal/eligibility/withdrawal.go`
computes `Ineligible` as `Gross.Sub(PayoutEligible)`. And `frozen` is a disputed
lot, which no payout policy permits, so every frozen Credit was already inside
the second segment. The picture therefore claimed more than the total it was
drawn against, and it did so in the flattering direction: the frozen Credits
appeared as value held back twice, on the one screen a customer opens to work
out what can actually leave.

**Fix.** Two segments against `gross`, which account for every Credit exactly
once. `frozen` is a labelled field beside the bar, next to `spendable`, with the
sentence saying it is inside the ineligible part rather than beside it — because
"may be used inside Nodal" and "may leave Nodal" are two permissions and this
page exists to keep them apart.

**Evidence.** `apps/web/src/pages/withdraw/Withdraw.tsx`; the audit
reproduction passes against a sandbox-tier API on a fresh database.

## F-202 · A fill left the portfolio, the holdings and the activity feed stale · PRODUCTIZATION · P1 · FIXED

**Found by** the frontend audit, reproduced twice: in
`apps/web/src/lib/audit-frontend.test.ts` against the source, and in
`e2e/audit-frontend.spec.ts` by delivering the `data.changed` event the server
sends and watching whether `/v1/me/portfolio` is refetched.

`StreamStatus.tsx`'s `SCOPE_KEYS` was a list of hand-copied query prefixes.
Five of them — "buying-power", "holdings", "activity", "native-asset",
"native-assets" — named reads no page has made since D-077 removed the hosted
rail, and an invalidation against a prefix nothing reads matches nothing and
raises nothing. Meanwhile the two keys a fill actually changes,
`["me","portfolio",…]` and `["me","activity",…]`, were in no scope at all. A
fill emits `position` and `balance`; the position it changed, the Credits it
spent and the row it wrote were the three things on screen that did not refresh.

It survived because `MarketDetail.tsx` carried its own four-line copy of the map
beside its mutation, which made the trade screen look correct and hid the defect
exactly where a reviewer would have looked for it.

**Fix.** The map is derived from the key factories in `api/queries.ts` (D-112)
and holds no string of its own, so renaming a key moves the map with it and
deleting one stops the build. `MarketDetail` imports `invalidateScopes` instead
of reproducing it. The three scopes the follower emits and the app had never
heard of — verification, eligibility, agent — are mapped rather than falling
through to invalidating the entire cache.

**Evidence.** `apps/web/src/components/StreamStatus.tsx`,
`apps/web/src/lib/stream-scopes.test.ts` (four assertions, including one that
reads the scope constants out of `internal/notifications/follower.go`); both
audit reproductions pass.

## F-203 · A refused order was answered about its idempotency key · PRODUCTIZATION · P1 · FIXED

**Found by** the frontend audit, reproduced in `e2e/audit-frontend.spec.ts` —
"a second confirmation of the same trade is evaluated, not refused for its key".

The ticket minted its key at confirmation and kept it, which is right for a
replay of the same request. But every route back to the confirm button goes
through a new quote, and `quote_id` is in the body: the key was unchanged and
the body was not, so the backend answered `INVALID_IDEMPOTENCY_REUSE`. A
customer who had just asked the market to price their order again, and
confirmed, was told something about a header — with nothing on the page that
could fix it, because the page was the thing holding the stale key.

**Fix.** `apps/web/src/lib/idempotency.ts` holds one key per command against a
signature of the body the backend compares: the same body returns the same key,
a changed body mints a new one, and it survives the sign-in round trip so
scenario J is still a retry rather than a second request. Taking a new quote
clears it explicitly as well. `INVALID_IDEMPOTENCY_REUSE` joins `lib/errors.ts`
with a `requote` recovery, so if this is ever got wrong again the customer is
not left at a dead end on top of an order that did not happen.

**Evidence.** `apps/web/src/pages/markets/Ticket.tsx`,
`apps/web/src/lib/idempotency.ts`, `apps/web/src/lib/errors.ts`; the audit
reproduction passes.

## F-204 · Signing out left the previous customer's form state in the tab · PRODUCTIZATION · P2 · FIXED

**Found by** the frontend audit, reproduced in `e2e/audit-frontend.spec.ts` —
"signing out forgets what the previous customer typed".

`survives-sign-in.ts` says in its own header that the mirror "is scoped to the
one tab and dies with it, so a shared computer does not hand the next person a
form". Signing out is exactly the shared-computer case, and it does not kill the
tab: the handler revoked the session, reloaded the page and cleared no storage,
so every `nodal.form.*` key stayed for the thirty minutes of the TTL —
a withdrawal amount and a stashed payout destination id among them.

**Fix.** `clearAllFormState()` empties the in-tab map and every
`nodal.`-prefixed key in the mirror, called from the sign-out handler before
the reload, on `onSettled` rather than on success: the decision to stop using
this tab is the customer's and does not depend on the backend answering.

**Evidence.** `apps/web/src/lib/survives-sign-in.ts`,
`apps/web/src/components/AppShell.tsx`; two unit tests including the
storage-denied path; the audit reproduction passes.

## F-205 · The Buy Credits key was kept across a change of amount · PRODUCTIZATION · P2 · FIXED

**Found by** the frontend audit reading `BuyCredits.tsx` beside the ticket.

The amount and the key were two independent stashes. Changing the amount
changed the body and not the key, so the backend compared the recorded body,
found a different `amount_minor` and answered 409 — on the payment screen, about
a header, with no recovery on the page, to somebody who had done nothing but
change their mind about how much to buy.

**Fix.** The key is held against the amount it was minted for, through the same
`useIdempotencyKey` the ticket uses, and is dropped in the same handler that
changes the amount — both the typed field and the preset buttons. `onStartOver`
clears it.

**Evidence.** `apps/web/src/pages/credits/BuyCredits.tsx`,
`apps/web/src/lib/idempotency.ts`; scenario B green.

## F-206 · The public site never said the deployment is a rehearsal · PRODUCTIZATION · P2 · FIXED

**Found by** the frontend audit, reproduced in `e2e/audit-frontend.spec.ts` —
"the public site says whether this deployment is a rehearsal".

`SiteChrome` read `GET /v1/version` for the build tag in its footer and
discarded `sandbox_tier`. `USER_JOURNEY.md` §0 promises the label, and the
public site is where it matters most: somebody signed out is deciding whether
this is a real product, and every sentence on the marketing pages read as one.

**Fix.** `SandboxLine` moves out of `AppShell` into its own module and both
shells render it. `honesty.spec.ts` holds every public route to it in both
directions — a real deployment wearing a sandbox label is the same lie as a
sandbox deployment without one.

**Evidence.** `apps/web/src/components/SandboxLine.tsx`,
`apps/web/src/pages/public/SiteChrome.tsx`, `apps/web/e2e/honesty.spec.ts`; the
audit reproduction passes.

## F-207 · One deployment, two temperatures · PRODUCTIZATION · P2 · FIXED

**Found by** the frontend audit comparing Home and `/markets` on a sandbox tier.

`/markets` read the version, desaturated every figure and said why. Home,
`/activity` and `/agents` never read it, so the same market moved by the same
amount was the internal economy on one screen and a simulation two clicks away.
That teaches a reader that the desaturation is a fact about the market rather
than about the deployment, which is the opposite of what the three temperatures
are for.

**Fix.** All four pages read it once and pass it down. The standing sentence
moves to `lib/honesty.ts` as `SANDBOX_TIER_NOTE`, so there is one of it rather
than four that can drift.

**Evidence.** `apps/web/src/pages/home/Home.tsx`,
`apps/web/src/pages/activity/Activity.tsx`,
`apps/web/src/pages/agents/AgentsList.tsx`,
`apps/web/src/pages/markets/Markets.tsx`, `apps/web/src/lib/honesty.ts`.

## F-208 · Every press minted a new idempotency key · PRODUCTIZATION · P2 · FIXED

**Found by** the frontend audit reading `Products.tsx`, and then the same shape
in three more places.

`Products.tsx`, `AgentDetail.tsx`, `AgentNew.tsx` and `CreateAsset.tsx` all
called `newIdempotencyKey()` inside the click handler. A reply lost in transit
and a second press were therefore a second purchase, a second lifecycle
decision, a second agent and a second asset — the exact failure the header
exists to prevent. Three of them carried a comment promising the opposite;
`CreateAsset.tsx` said in words that "a retry of the same confirmation must
never make a second asset", which was the one thing it could not deliver.
`Withdraw.tsx`'s confirmation had the other half of the defect (F-203's shape)
on the most sensitive command in the product.

**Fix.** `useIdempotencyKey` at every one of those sites, keyed by the thing
acted on, with a signature over the fields the backend compares, cleared on
success.

**Evidence.** `apps/web/src/lib/idempotency.ts` and the five pages;
`e2e/scenarios/i-concurrency.spec.ts` and `j-session-expiry.spec.ts` green.

## F-209 · The application sweeps asserted nothing that identifies the page · PRODUCTIZATION · P2 · FIXED

**Found by** the frontend audit, reproduced in `e2e/audit-frontend.spec.ts` —
"the APP_ROUTES sweeps assert nothing that identifies the page".

`accessibility.spec.ts`, `controls.spec.ts` and `honesty.spec.ts` each walked
`APP_ROUTES` asserting one thing about what rendered: exactly one `h1`. The 404
page satisfies that, and so does a sign-in page a gate diverted to — and this
404 page is a tidy list of live links, so it also passes the dead-control walk
and carries no forbidden vocabulary. Fifteen routes could have stopped existing
and all three suites would have stayed green while checking the same not-found
screen fifteen times. `RouteUnderTest.heading` existed for this and only the
public sweep read it.

**Fix.** Every application sweep asserts `route.heading` before it asserts
anything else, as the public sweep already did.

**Evidence.** `apps/web/e2e/accessibility.spec.ts`, `controls.spec.ts`,
`honesty.spec.ts`.

## F-210 · The reversal test passed on the account that disproves its name · PRODUCTIZATION · P2 · FIXED

**Found by** the frontend audit reading `g-refund.spec.ts` against the API's
`reversed` bucket.

The test asserted that the Credit panel contains the words "Not payout-eligible"
— a heading the panel carries unconditionally, on every account, reversal or
not. So on an account whose balance reports a non-zero `reversed` bucket that
the panel does not draw at all, it passed.

**Fix.** It asserts the figure: a field labelled for the bucket whose `title` is
the API's own string for it, reconstructed in the spec by moving the point
through the base units rather than parsing them.

**Note.** This test FAILS on the branch that fixed it, deliberately. The Credit
panel learning the `reversed` bucket is a separate finding owned by the
credits-and-payments branch; the test is written against the contract
(`reversed` on `GET /v1/credits/balance`) rather than against what Home draws
today, because the contract is the part that has to be true and a test that
waited for the page to catch up would be the same test that had been passing
wrongly all along.

**Evidence.** `apps/web/e2e/scenarios/g-refund.spec.ts`.

## F-211 · No focus management on a route change · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit, reproduced in `e2e/audit-frontend.spec.ts` —
"changing route moves focus into the new page".

A client-side navigation replaced the document's content and left focus on the
link that was clicked. A screen-reader user was told nothing had happened; a
keyboard user's next Tab resumed in the rail, so reaching the first control on
the page they had asked for meant walking the whole navigation again.

**Fix.** `AppShell` focuses `#main` — which already has `tabIndex={-1}` for the
skip link, so this is the same landing place "Skip to main content" gives — on
every `location.pathname` change after the first render. The first is skipped
deliberately: a fresh page load already has focus at the top of the document,
and stealing it there would fight the browser's own restore on a back
navigation.

**Evidence.** `apps/web/src/components/AppShell.tsx`; the audit reproduction
passes.

## F-212 · The one generic sentence the design system forbids by name · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit, reproduced in
`apps/web/src/lib/audit-frontend.test.ts` — "no error state says 'Something went
wrong'".

Every problem code without a case in `explain()` fell through to a heading
`UI_UX_SYSTEM.md` §4 forbids by name. It tells the reader nothing, and it was
not even true: the backend had said what happened, in a `title` the app was
discarding.

**Fix.** The backend's title where it gave one that is more than the code
repeated back; otherwise "The backend did not complete this", which says what is
actually known. The stable code and the correlation id are unchanged.

**Evidence.** `apps/web/src/api/problem.ts`. The audit reproduction files join
the guard tests in `scan.ts`'s exclusion list (D-114) for the reason that list
exists: a file asserting a sentence appears nowhere has to write the sentence
down.

## F-213 · `/agents/:agentId` was in no route list · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit enumerating the route lists against the router.

`APP_ROUTES` cannot hold it — the sweeps walk that list by navigating to each
`path` and would ask the API for an agent called ":agentId" — and
`DYNAMIC_APP_ROUTES` did not name it either. Its only coverage was its not-found
state: the one shape of the page that renders no figure, no limit, no lifecycle
control and no authority level, which is to say the one shape that cannot fail
the checks a sweep would apply.

**Fix.** It joins `DYNAMIC_APP_ROUTES`, and `d-agent` — the scenario that owns
it — runs axe, the 375px reflow check and two honesty rules against a real
agent. This build produces none, because the strategy service is wired with no
compiler, so the skip says that in those words rather than "no agent".

**Evidence.** `apps/web/e2e/routes.ts`, `apps/web/e2e/scenarios/d-agent.spec.ts`.

## F-214 · Three components nothing imports · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit, reproduced in
`apps/web/src/lib/audit-frontend.test.ts` — "every component in the tree has a
caller".

`Money.tsx` was a second money formatter, with its own absent and malformed
states, written before `Figure` and the number ladder in `UI_UX_SYSTEM.md` §6.
Two formatters is one more than a codebase this careful about exact values can
afford: "every figure goes through `Figure`" is only true while there is nothing
else to go through. `MintIdentity.tsx` belonged to the hosted rail D-077
removed. `Tabs.tsx` was a correct WAI-ARIA tablist no screen ever rendered.

**Fix.** All three deleted, with `TabButton` — which nothing else can use —
and `UI_UX_SYSTEM.md` §7 records it rather than describing a component that is
not there (D-113).

**Evidence.** `docs/product/UI_UX_SYSTEM.md` §7 and §11; the audit reproduction
passes.

## F-215 · `Figure` dropped options its callers passed · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit, reproduced in
`apps/web/src/lib/audit-frontend.test.ts` — two tests, on the count's symbol and
on the stablecoin band.

`Figure` accepts `symbol` on every kind, `stablecoin` on money and a value in
either exact form. Three combinations were silently dropped. `formatCount`
ignored the symbol, so the Credits-per-dollar rate on Buy Credits rendered as a
bare "100" with nothing to say a hundred of what. `formatUnits` had no
stablecoin branch, so the depeg band applied to a value sent as a decimal and
not to the same value sent as base units. And the percent kind read
`{base, scale}` as though the point had already moved, which renders five per
cent as five hundred.

**Fix.** All three honoured. A dropped option is worse than a missing one: the
caller asked, the type accepted, and the screen said something else.

**Evidence.** `apps/web/src/lib/format.ts`,
`apps/web/src/components/Figure.tsx`; two new unit tests in `format.test.ts`;
both audit reproductions pass.

## F-216 · The paused-market refusal test had never run · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit reading the skip in `c-trade.spec.ts`.

It skipped on "every market in this deployment is open", which is true of every
seeded deployment, so the assertions under it had executed zero times while
reading like coverage in a report.

**Fix.** The test drives the real control. `NATIVE_MARKET_CLOSE_ONLY` is
single-operator with a step-up (`internal/admin/kinds.go`), so an operator signs
in through the development provider's MFA link, proposes the action and executes
it, and the test then asserts the refusal the ticket renders. Nothing is faked
and no row is written behind the API's back. It stops the LAST market in the
discovery ordering, because coming back is `NATIVE_MARKET_RESUME` — dual control
with an approve-side permission no standing role holds — and every other spec
reaches for the first ACTIVE market. Where the deployment declares no operator
it still skips, and the skip now names the setting that would change that
(`CP_AUTH_BOOTSTRAP_OPERATORS=devidp|dev:operations=OPERATIONS`) rather than
naming the weather.

**Evidence.** `apps/web/e2e/scenarios/c-trade.spec.ts`; verified against a
sandbox-tier API with that declaration — the market moves to CLOSE_ONLY through
`/v1/admin/actions` and the ticket renders the refusal.

## F-217 · The agent commands minted a key per press under comments promising otherwise · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit reading `AgentNew.tsx` and `AgentDetail.tsx`.

The same defect as F-208, recorded separately because of the comments: each site
explained that the key was minted at confirmation "so a retry after a stronger
sign-in replays this decision instead of making a second one", and then minted a
fresh one on every press, which makes every retry a second decision.

**Fix.** `useIdempotencyKey` keyed by agent and by action, with the reason in the
signature, so pausing and then disabling the same agent are two requests and
pressing pause twice is one. The compile step keeps a key per attempt — asking
again IS a new decision, and the attempt number the backend records is the
evidence — but keeps it long enough for a lost reply to be re-sent as the same
attempt. The comments now describe what the code does.

**Evidence.** `apps/web/src/pages/agents/AgentNew.tsx`,
`apps/web/src/pages/agents/AgentDetail.tsx`.

## F-218 · A malformed stash blanked the whole application · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit.

There was no error boundary anywhere. React unmounts the entire tree when a
render throws and nothing catches it, so any exception on any page replaced the
application with an empty document: no heading, no message, no control, and the
same result on every reload. The audit reached it by the most ordinary route
there is — a recovered draft whose amount was no longer a string, handed to the
BigInt constructor — which meant one bad value in a tab's own storage made the
product unusable until somebody thought to clear site data.

**Fix.** Two lines of defence. `survives-sign-in.ts` checks a recovered draft
against the shape the page declared before casting it, and drops what does not
match exactly as it drops an expired one. `ErrorBoundary` sits inside the shell
and around the page, so the navigation, the account menu and the sandbox label
survive a fault on one screen; it renders the same `Explanation` a backend
failure gets, plus a control that forgets what the tab has saved and reloads,
which is the way out of a stash the page cannot read.

**Evidence.** `apps/web/src/components/ErrorBoundary.tsx`,
`apps/web/src/lib/survives-sign-in.ts` (`sameShape`), `apps/web/src/App.tsx`;
three unit tests.

## F-219 · The quote-fee scale in the API description was wrong · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit checking the ticket's scales against the
description it is built from.

`NativeQuote.asset_decimals` said "Fees and prices are always Credits, at
price_scale". `internal/nativemarket/curve.go` takes both fees out of the Credit
amount, so they carry the Credit asset's six decimals, while `effective_price`
is a ratio computed at `PriceScale`, which is eighteen. A reader who believed
the description would render a fee twelve orders of magnitude too small.

**Fix.** The description says which figures are on which scale, and names the
consequence of confusing them. Description only — no behaviour, no schema — and
both generated outputs re-run so the committed files match.

**Evidence.** `openapi/openapi.yaml` (`NativeQuote`),
`internal/gen/api/api.gen.go`, `packages/generated-client/src/schema.d.ts`.

## F-220 · Two honesty checks settled the network after reading the page · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend audit.

"a simulated surface on the public site says it is an example" and "a page
showing Credits says what a Credit is" both read `document.body.innerText` and
then waited for `networkidle`. Both therefore tested the page as it was during
its own fetches — an example composition the live list was about to replace, or
a Credit figure that had not arrived. The harmless-looking direction is the
dangerous one: a check that passes because the thing it polices had not rendered
yet leaves a real page unchecked.

**Fix.** The wait moves above the read in both.

**Evidence.** `apps/web/e2e/honesty.spec.ts`.

## F-221 · Adding a payout destination minted an idempotency key per press · PRODUCTIZATION · P3 · FIXED

**Found by** the frontend fix agent while fixing F-203/F-205/F-208 (one key per
body), and left for the orchestrator because the page sits in the withdrawal
area another branch was touching.

`Destinations.tsx` called `newIdempotencyKey()` inside `add.mutate`'s argument,
so every press of "Add destination" was a new command to the backend. The one
retry the form is built for -- a network failure or a step-up round trip on the
first press, whose answer never reached the browser -- therefore registered the
same destination a second time under a second key, and the customer saw two
rows for one bank account. (Removing a destination, accepting terms and saving
the onboarding profile also mint per press; each of those is idempotent by its
own nature -- a second removal is a no-op, an acceptance is keyed by document,
an update applied twice is one state -- and they are left as they are.)

**Fix.** `useIdempotencyKey("destinations.add.<account>")` with a
`requestSignature` over the six fields the backend compares: a retry of the
same body sends the key it sent before, an edited body mints a new one, and the
key is cleared on success. The same helper the frontend fix put under the trade
ticket, Buy Credits, the product purchase, the agent actions and the payout
confirmation (F-203).

**Evidence.** `apps/web/src/pages/withdraw/Destinations.tsx`,
`apps/web/src/lib/idempotency.ts`; the e2e-browser audit observes the network
for the retry shape.

## F-222 · Finishing onboarding raced the profile refetch · PRODUCTIZATION · P2 · FIXED

**Found by** the merged tree's Playwright run under load (the full Go
integration matrix was running beside it): the suite's own sign-in setup, which
had passed on every quiet run, failed at its last step and the other 157 cases
never ran.

`useAcceptTerms` and `useUpdateProfile` fired the `/me` invalidation and forgot
it (`void qc.invalidateQueries`), so the terms page navigated to "You're in" the
instant the acceptance returned, while the refetch that would say onboarding is
complete was still in flight. The dashboard gate (`OnboardingGate` in
`App.tsx`) reads `onboarding.complete` off that query: with the API answering in
84 ms instead of 20, the customer's click on "Go to dashboard" beat the refetch,
the gate read the stale `next_step: TERMS`, and sent them to `/welcome/terms`,
which then read the fresh terms state and rendered "Nothing outstanding" with a
Continue button. Not data loss; a wrong screen after the last step of arriving,
for anybody on a slow connection.

**Fix.** Both success handlers return their invalidation, so the mutation stays
pending until the fresh `/me` has landed and the page that navigates on success
navigates to a gate that already knows (TanStack Query awaits a promise returned
from the hook-level `onSuccess` before the `mutate` callbacks fire). The
source-scan test `the onboarding mutations await the profile refetch before
they succeed` refuses the fire-and-forget form in either hook.

**Evidence.** `apps/web/src/api/queries.ts`; `apps/web/src/lib/audit-frontend.test.ts`;
the API log of the failing run (POST `/v1/me/terms-acceptances` 200 at
22:58:30.686, the `/v1/me` refetch at 30.856, the diverted page's terms read at
30.887); the merged Playwright run after the fix.

## F-223 · The demodata command gave its ledger no capability resolver · PRODUCTIZATION · P3 · FIXED

**Found by** the same run: `go run ./scripts/demodata` created six objects and
then refused its first trade with `CAPABILITY_NOT_APPROVED: moving value from
INTERNAL_CREDIT to INTERNAL_NATIVE_ASSET requires capability
NATIVE_MARKET_TRADING to be ACTIVE`, on a database whose six gates the API had
just sandbox-activated at boot.

The command built the ledger, the Credit service and the native market exactly
as `cmd/api` does, and never called `ledger.SetCapabilityResolver`. A ledger
with no resolver fails closed on every cross-domain posting -- correct for a
process that has not been told otherwise -- so the demo catalogue's markets were
created and none of its trades were, and the command's own documentation ("where
a gate is missing its trades fail with the capability's own refusal") described a
refusal that had nothing to do with the gates. The resolver that answers the
question lived as a private type in `cmd/api`, so a second composition root had
no way to read the same source.

**Fix.** The resolver is `internal/capresolver` (the type and the list moved
unchanged; `cmd/api` keeps its name for it and its two tests against
`settlement.AllRequiredCapabilities()`). The command builds a `gates.Checker`
from `CP_ENV` and `CP_API_ENABLED_CAPABILITIES`, marks it a sandbox reader when
the deployment is one, and installs the resolver on its ledger -- so a
sandbox-active gate now permits the trade and a missing one refuses it in the
capability's own words. CI's browser job and the local recipe additionally seed
through the API's own boot path (`CP_API_DEMO_DATA=true`, D-115), and the recipe
surfaces the command's exit code instead of piping it into `tail`.

**Evidence.** `internal/capresolver/resolver.go`, `cmd/api/capabilities.go`,
`scripts/demodata/main.go`, `.github/workflows/ci.yml`;
`TestCapabilities_ResolverAnswersEverythingTheCompilerCanRequire`,
`TestCapabilities_EveryAnsweredKeyIsADeclaredGate`; the merged Playwright run's
`DEMODATA_EXIT=0`.

## F-235 · Condition 4 named four capabilities and the gate code enforces it for eighteen · PRODUCTIZATION · P2 · FIXED

**Found by** the docs-vs-reality audit (goal §54), reproduced on
`audit/docs-vs-reality` @ `d4c43e6`.

`POLICY_AUTHORITY.md` §1 is the contract for who may do what, and its condition 4
parenthesised the capabilities that need all four evidence references as
"LIVE_* and WITHDRAWALS". The predicate `Propose` and `cp_gate_is_high_risk`
actually consult is `gates.IsHighRisk`, which is true for **eighteen of the
twenty** declared capabilities: the whole internal economy (`CREDIT_PURCHASE`,
`NATIVE_MARKET_TRADING`, `MARKETPLACE`, `PAYOUT_RESERVE`, `PAYOUT_SETTLE`,
`HOSTED_TRADING`, `HOSTED_FUNDING`) and agent authority levels 4–6. A reviewer
scoping the activation ceremony from this document would under-plan every one of
them, and `CREDIT_PURCHASE` is the gate `LAUNCH_GATE_MATRIX.md` says holds the
money path shut.

The same section printed a seven-state machine. `SANDBOX` has been the eighth
since migration 00755, ADR-0023 cites this section as the thing it constrains,
and migration 00701 cites §1 as its own authority — so the document a reader
would check the sandbox tier against did not know the state existed.

**Fix.** Condition 4 now names the predicate, states the count, and names the
**exceptions** — `NATIVE_ASSET_CREATION` and `SOCIAL_DATA_PERSISTENCE`, each with
the reason `internal/gates/capability.go` gives — because a list that changes
when a capability is added is a list that has to be written by its complement.
The state machine gains `DISABLED|REVOKED|EXPIRED → SANDBOX` and
`SANDBOX → DISABLED`, and says that a sandbox row carries no approval chain, no
approval version, no evidence references and no validity window, is refused in
PROD by a table CHECK, and is never a step towards ACTIVE. `SUSPENDED → ACTIVE`
is corrected to `SUSPENDED → APPROVED` in the same sentence: the transition
table has always required a distinct principal to activate a resumed gate.

**Evidence.** `docs/architecture/POLICY_AUTHORITY.md` §1;
`internal/gates/capability.go`, migrations 00701/00714/00716/00755/00791;
`TestAuditDocs_ThePolicyAuthorityDescribesTheGateTheCodeEnforces`, which derives
the count and the exception set from `gates.IsHighRisk` rather than reading them.

## F-236 · A documented call that no route answers · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, by matching every `/v1/...` a product document names
against the route table the generated chi server mounts.

`VERIFICATION_AND_WITHDRAWAL.md` §2's diagram sent a caller to
`POST /v1/me/terms/accept`. No route matches that path with any method; the route
is `POST /v1/me/terms-acceptances`. It is the call the withdrawal-disclosure step
makes, so a client written from the diagram fails on its first attempt at the one
step that gates a payout.

**Fix.** The diagram names the served path, with the ASCII art's column alignment
preserved.

**Evidence.** `docs/product/VERIFICATION_AND_WITHDRAWAL.md:75`;
`internal/gen/api/api.gen.go`;
`TestAuditDocs_EveryRouteAProductDocumentNamesIsServed`.

## F-237 · The onboarding row named a method the path is not served with · PRODUCTIZATION · P2 · FIXED

**Found by** the same check. `USER_JOURNEY.md` §1's first onboarding step said
`PUT /v1/me/profile`. The path is mounted, but only for POST — so the failure is
a 405 on the first call a new customer's client makes, which is worse than a 404
because the path resolving looks like the route is right.

**Fix.** The row says POST.

**Evidence.** `docs/product/USER_JOURNEY.md:40`;
`TestAuditDocs_EveryRouteAProductDocumentNamesIsServed`.

## F-238 · The security architecture reported nine present controls as missing, and called eight passing suites planned · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reading `SECURITY.md` against the tree.

`SECURITY.md` is one of the two documents a reviewer scores security posture
from. It told that reviewer that `test/security`, `test/contract`, `infra/` and
`docs/runbooks/` do not exist (20 test files, 8 test files, 66 `.tf`, 19 runbooks
and an index); that `internal/strategy` and `internal/model` are absent; that the
CI contract step is vacuous and **"the security step passes vacuously"**; and, in
bold, that **"no writer exists"** for `security_events` — while
`REQUIREMENTS_TRACEABILITY`'s R-130-1, in the other document a reviewer reads,
listed the four packages that write them. §14 then handed an operator incident
fallbacks "until they exist" with nineteen runbooks sitting in the directory it
said was missing.

For five days the document carried a blanket caveat at the top saying much of it
was now wrong in the understating direction. A warning is not a correction: a
reviewer reads the sentences.

The **PART 155 matrix** was the sharpest instance. Eight of its rows named an
adversarial suite in the "Planned (traceability)" column by the filename it was
going to take; the suite had landed in the same directory under a different name
and passes today — `TestIDOR_CoversEveryAccountScopedRoute`,
`TestIDOR_CrossTenantAccountReadsAreRefused`,
`TestPromptInjection_UntrustedTextNeverEntersTheInstructionChannel`,
`TestAgentPrincipalPermissionSetIsClosed`,
`TestDualControl_AProposerCannotApproveItsOwnAction`,
`TestProductionRefusesFakeProviders`, `TestLocalDefaultsAreNotProductionValid`,
`TestReplay_ForgedSessionCookiesAreRefused`. The control that should have caught
the left-hand column going stale could not see the table at all:
`references_test.go` exempted any **paragraph** containing the word "planned"
from its path check, and a markdown table is one paragraph, so the header cell
excused every path in all seventeen rows in both columns. That is how
"IDOR … (primitive only; no HTTP handlers exist)" survived `cmd/api`.

**Fix.** Every false absence is replaced by what is there, with the path. The
matrix is rewritten: the eight rows name their passing proof in "Exists today"
and plan nothing; the four rows that are genuinely unwritten — CSRF end-to-end,
SSRF, webhook forgery, dependency compromise — keep their planned suite and each
gains a sentence saying what would prove it. `test/security/doc.go`'s comment,
which still said API-level tests "join this package once `cmd/api` exists",
says what joined and what has not.

The control is narrowed rather than trusted. `plannedPhrase`'s blanket exemption
is gone: the excuse is now evaluated per **cell**, scoped to the column a table
headed "Planned" (or "Required before …"), and it excuses only an **absent**
path — a plan for something already on disk is a claim that went stale, and now
fails. `absencePhrase` gains "never existed", which a cell has to be able to say
about itself once the paragraph no longer says it for it.

§5's import-rule cell also stated plainly what the audit's notes raised: the
`internal/risk/policy` deny in `.golangci.yml` and `scripts/lintfin` names a
package that has never existed, so that one entry can never fire. It is **not**
removed — see the residual note below — and the cell now names what does hold the
line: the `risk_policies.created_by_actor_type` CHECK (00152), `risk:policy_write`
outside the agent permission set, and the two authority-boundary tests.

*Residual:* the dead depguard entry stays. Pointing it at `internal/risk` would
deny `internal/strategy/validate.go` an import it legitimately makes — reading the
kernel's limits to validate a strategy is not mutating a policy, and depguard
cannot express the difference — and deleting it would leave
`MASTER_BUILD_STATE.md:1216`, `REQUIREMENTS_TRACEABILITY.md` R-009-1 and
`PRODUCTION_READINESS_REPORT.md:158–159` citing a rule that no longer exists,
one of which this brief forbids editing. The honest move was to say so where a
reviewer reads it.

**Evidence.** `docs/security/SECURITY.md` §§0, 2, 5, 11, 12, 13, 14;
`test/security/doc.go`; `test/docs/references_test.go` (`citationUnits`,
`plannedHeader`); `TestAuditDocs_NothingListedAsPlannedIsAlreadyWritten`, which
now reads the matrix as rows and holds both halves of the rule;
`TestAuditDocs_NoDocumentDeclaresAnAbsenceTheTreeContradicts`;
`TestDocs_EveryPathTheyNameExists`.

## F-239 · Two closed residual risks kept at the top of the list, and a boundary table that predates twelve packages · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit.

`THREAT_MODEL.md` §8's residual risk 9 said `make security`, `make contract` and
`make iac-scan` "pass on empty directories" and that there is "no git remote";
risk 10 said "the reconciliation, settlement and execution code that must keep
running does not exist; no Terraform, IAM task roles, private network, WAF …".
The tree closed both: 20 files in `test/security`, 8 in `test/contract`, 66 `.tf`
across three environments, an `origin` remote, ten CI jobs, 67 Go files across
`internal/{reconciliation,settlement,execution}` with both worker binaries, and
per-service task roles in `infra/terraform/modules/ecs-service`. The document's
own risk 8 states the rule this breaks: "Listing a closed control among the top
ten residual risks understates the system in a document a reviewer uses to score
its posture, which is a truthfulness defect in the same way an overstatement is."

§2 — "the boundary every table below is scored against" — was worse, because
every STRIDE row below is scored against it. It listed `cmd/api`, the Terraform,
`internal/risk`, `internal/admin`, `internal/funding`, the withdrawal domain, the
buying-power engine, `cmd/audit-worker`, both adversarial test trees and
`internal/{signing, wallet, execution, reconciliation, settlement, quote,
instruments, intent, agent, strategy, model, prediction}` plus every worker
binary as **absent**.

**Fix.** 9 and 10 are struck in place, the way F-111 struck 8, each with the
evidence that closed it and with the narrower part that genuinely remains: no
workflow run has ever applied the Terraform, and the `aws-sm://` resolver is
still missing. Two real risks are promoted into slots 11 and 12, both already
named elsewhere in the tree and neither on this list: the native-market **circuit
breaker is built, enforced, tested and disarmed** in the compiled-in conservative
policy (`circuit_breaker_move_bps: 0`, D-065, ADR-0027), so markets never pause
automatically; and **authentication has no per-identity rate limit and raises no
`login_anomaly` event** — `httpapi.RateLimits.Auth` is a transport class keyed by
principal-or-address, so credential stuffing spread across addresses meets no
ceiling and nothing records that it happened. The section states the numbering
convention it now follows, because runbooks cite these risks by number.

§2's eight rows are re-derived from the tree in the same pass.

**Evidence.** `docs/threat-model/THREAT_MODEL.md` §2 and §8;
`internal/nativemarket/safety.go`, `internal/httpapi/middleware.go`;
`TestAuditDocs_NoDocumentDeclaresAnAbsenceTheTreeContradicts`, whose rows for
this document are now presence claims — the corrected sentence is pinned and the
tree has to keep bearing it out.

## F-240 · The example environment named a settlement pair the seed does not register · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, by resolving `.env.example` against `scripts/seed`.

`.env.example` is the file README tells a developer to copy. It set
`CP_API_SETTLEMENT_CHAIN=solana` and `CP_API_SETTLEMENT_MINT` to the **mainnet**
USDC mint `EPjFWdd5…`. `scripts/seed` is the only thing that registers a
settlement asset locally, and it registers `solana-devnet` with the devnet mint
`4zMMC9srt…`; `cmd/api`'s composition calls `repo.GetByMint` on the configured
pair and returns an error when it does not resolve. Following the documented
setup produced a service that refuses to start, with
`settlement asset solana/EPjF…: not found`.

It is also the claim `render.yaml` refuses in as many words — "Devnet, not
mainnet … pointing at mainnet USDC would claim a settlement path this deployment
does not have" — and the same mainnet pair sat in
`infra/terraform/environments/prod/terraform.tfvars.pathb.example`.

**Fix.** The defect is in the CODE that generates the file: the two `Example`
strings in `internal/config/load.go` now carry the devnet pair and say why, and
`.env.example` is regenerated from them
(`go test ./internal/config -run TestExampleEnv -update`). The Path B tfvars
example carries the devnet pair with the same note.

**Evidence.** `internal/config/load.go`, `.env.example:176–183`,
`infra/terraform/environments/prod/terraform.tfvars.pathb.example`;
`TestAuditDocs_TheExampleEnvironmentNamesTheSettlementAssetTheSeedRegisters`,
which derives both sides from the two files; `TestExampleEnv`; `test/infra`.

## F-241 · A verification level reported as a capability gate · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, matching every name a document calls a capability
against `gates.AllCapabilities()`.

`PROVIDER_BOUNDARY.md` §4 listed `WITHDRAWALS`, `PAYOUT_RESERVE`, `PAYOUT_SETTLE`
and `PAYOUT_KYC` as "capabilities, all DISABLED". `internal/gates` declares
twenty and `PAYOUT_KYC` is not one of them: it is a verification **level**, the
one `valuedomain.SandboxPolicy` requires before reserved value may be released.
A capability name is what an operator types into
`POST /v1/admin/gates/{capability}/{action}` and what a reviewer looks for in
`capability_gates`, so the sentence described a control that is not there.

**Fix.** `PAYOUT_KYC` is out of the capability list and named as the verification
level it is.

**Evidence.** `docs/product/PROVIDER_BOUNDARY.md:106–107`;
`internal/gates/capability.go`, `internal/valuedomain/sandboxpolicy.go`;
`TestAuditDocs_EveryCapabilityADocumentNamesIsDeclared`.

## F-242 · Three documents described an activity feed of seven kinds that has carried eighteen since D-081 · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, deriving the kind list from `internal/activity`.

`CREDIT_ECONOMY.md` §7 exists to say what is not built "so nobody has to discover
it", and one of its five bullets read "**No verification, profile, security or
agent events in the activity feed**", citing `internal/activity/doc.go` as the
extension point for them. `doc.go` says the opposite in as many words: those
kinds "have landed (D-081)". `CURRENT_SYSTEM_INVENTORY.md` said the §16 timeline
has seven kinds; there are eighteen. And R-PG-016-1 was held at `IN_PROGRESS` by
an evidence cell saying those kinds "are not declared yet" when ten of them are —
a row kept open by a stale sentence, in a table whose summary counts are computed
from statuses.

**Fix.** The bullet states the one absence that is real — security events — and
says why it is deliberate: `internal/notifications` already tells a person about
a new sign-in, and a page over `security_events` is a different surface with a
different retention policy from a timeline of what happened to an account's
value. The inventory says eighteen. R-PG-016-1 moves to `VERIFIED` on the tests
it already named, and the summary counts move with it (IN_PROGRESS 61 → 60,
VERIFIED 269 → 270).

**Evidence.** `docs/product/CREDIT_ECONOMY.md:335`,
`docs/build/CURRENT_SYSTEM_INVENTORY.md:259`,
`docs/build/REQUIREMENTS_TRACEABILITY.md` R-PG-016-1 and the summary table;
`internal/activity/{activity,doc}.go`;
`TestAuditDocs_TheActivityFeedIsDescribedAsItIsBuilt`, which derives the count
from the package on both sides rather than pinning the word "seven";
`TestDocs_TraceabilitySummaryMatchesItsRows`.

## F-243 · The design system described a shell the app does not ship, and taught the forbidden word · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, reading `UI_UX_SYSTEM.md` §§10–11 against
`apps/web`.

§10.8 said search and notifications are not in the shell; `AppShell.tsx` declares
a Notifications destination and renders its unread count. §10.9 said "No endpoint
in this deployment sells Credits — that is a decision, not a bug — so the shell
offers **'Add funds'**". `POST /v1/payments` is mounted, `GET /v1/credits/pricing`
publishes the rate, `AppShell.tsx` labels the action "Buy Credits", and
`USER_JOURNEY.md` forbids "add funds" **by name**. This is the document a
copywriter opens to look the product's vocabulary up, so the defect taught the
banned phrase from the place that exists to prevent it. §11 opened by saying the
sixteen existing screens were not rewritten — `apps/web/src/pages` holds
forty-one page modules over thirty-one routes — and its remaining-work list still
asked for two things that are done: `Refusal` replaced the generic `Explanation`,
and `Home.tsx` renders a `SegmentedBar`.

**Fix.** §10.8 and §10.9 describe the shell that shipped, with the reason each
control is or is not there. §11's premise is corrected and the two done items are
struck. `USER_JOURNEY.md`'s vocabulary sentence is reflowed so the forbidden
phrase sits on one line — a phrase forbidden by name that no grep can find is not
enforceable — and the reproduction walks `docs/product/` for it.

**Evidence.** `docs/product/UI_UX_SYSTEM.md` §§10.8, 10.9, 11;
`docs/product/USER_JOURNEY.md:9–15`; `apps/web/src/components/AppShell.tsx`,
`Refusal.tsx`, `apps/web/src/pages/home/Home.tsx`;
`TestAuditDocs_TheDesignSystemDescribesTheShellThatShipped`.

## F-244 · A domain table pointing at two packages that do not exist · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit. The existing path check resolves citations that name
a Go **test** file or something under `test/`; a domain table that points a
reader at the package owning a domain is the other citation a reader follows, and
nothing resolved it.

`PRODUCT_ARCHITECTURE.md`'s domain table sent a reader to `internal/credits` for
Credits and `internal/payments` for Payments, and ADR-0027 to `internal/credits`.
The package is `internal/credit`, and there is no payments package at all: a
PaymentIntent lives beside the mint it causes, in `credit_fundings`, with
`internal/provider/stripecredit` for the provider half.

**Fix.** Both documents name the packages that exist, and the Payments row says
there is no separate package rather than leaving a reader to go looking.

**Evidence.** `docs/product/PRODUCT_ARCHITECTURE.md:79–80`,
`docs/adr/0027-a-position-is-the-sum-of-its-fills.md:14`;
`TestAuditDocs_EveryPackageAProductDocumentNamesExists`.

## F-245 · The first document anybody reads described a different repository · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit.

`README.md` called `apps/web` a Next.js app; it is React 19 on Vite with
react-router and has no `next` dependency. Its test-strategy paragraph reported
the API e2e, Playwright UI e2e and chaos tiers as **pending** while
`.github/workflows/ci.yml` declares a job for each and all three directories are
on disk, and it explained the unmeasured load tier by "until an API binary
exists" — `cmd/api` has existed and been deployed for days. A true claim with a
false reason is one nobody can act on.

"What V1 is" listed the hosted real-capital rail and said nothing about the
product the tree now builds: Credits, native assets and their markets, the
marketplace, the activity timeline, the customer web app, agents at authority
levels 0–3 with 4–6 visible and disabled, and the sandbox tier. "What V1 is not"
still listed the marketplace, which `internal/commerce` builds behind the
`MARKETPLACE` gate — a gate that is high risk precisely because it moves Credits
between users and mints the creator-earning provenance a payout policy may one
day permit to be withdrawn.

**Fix.** All four passages are corrected. The "is not" list keeps every entry
that is still true and says, for the two that moved, what exists and which gate
holds it closed.

**Evidence.** `README.md` §§"What V1 is", "What V1 is not", "Architecture",
"Test strategy"; `apps/web/package.json`, `.github/workflows/ci.yml`,
`internal/commerce`, `internal/agentauthority/authority.go`;
`TestAuditDocs_TheReadmeDescribesTheTreeItShipsWith`.

## F-246 · Two written documents marked "(pending)", under a check that could not see the marker · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit.

`BACKUP_RESTORE.md` §4 listed `docs/operations/DISASTER_RECOVERY.md` and
`docs/runbooks/database-corruption.md` as "(pending)". Both are in the
repository, and the second is the runbook an operator opens during the incident
this document describes — reading "(pending)" beside it costs minutes nobody has
during a database corruption.

`test/docs/runbooks_test.go` exists to catch exactly this and could not:
`markerRe` matched only the bare uppercase `PENDING`, so the lowercase
parenthesised form was invisible, and `pathRe` resolved only Go packages, `cmd/`
binaries and make targets, so a document path was never looked up at all.

**Fix.** Both markers are gone. `markerRe` matches `(pending)` as well, `pathRe`
resolves a `docs/**.md` citation, and `existsInRepo` answers for a file as well
as a directory. §2's heading said "pending Terraform" for the same reason; the
Terraform is written and has never been applied, which is a different claim and
now the one the heading makes.

**Evidence.** `docs/operations/BACKUP_RESTORE.md:44,53,69`;
`test/docs/runbooks_test.go`; `TestDocs_NothingMarkedPendingAlreadyExists`,
`TestAuditDocs_NoDocumentDeclaresAnAbsenceTheTreeContradicts`.

## F-247 · A count of nine over a table of ten rows · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, counting the rows under each "Routes added" header.

`CURRENT_SYSTEM_INVENTORY.md` said the agent wave added nine routes and then
listed ten; the admin pause route was written into the table and not into the
count. Same defect as F-111's "this table lists N capabilities": a number
recalled beside the rows that would have produced it.

**Fix.** The count says ten, and the reproduction derives it from the rows.

**Evidence.** `docs/build/CURRENT_SYSTEM_INVENTORY.md:430`;
`TestAuditDocs_EveryRoutesAddedCountMatchesItsTable`.

## F-248 · The decision register's citations were checked by nothing, and one pointed at nothing · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit.

`references_test.go`'s `inScope` list is "the documents a reviewer would use to
decide whether the system is ready". `docs/build/DECISION_REGISTER.md` was not on
it — and it is the document that records **why** every control is shaped the way
it is, the one a fixer opens before changing one. Its test citations had
therefore never been resolved by anything, and D-024's "Test change (PART 235)"
line named `TestIntegration_RelayRetriesWithBackoff`, which has never existed
under that name. The test is
`TestIntegration_RelayFailedPublishBacksOffAndRetries` in `internal/event`. This
is the F-25 shape — a citation pointing at nothing — surviving four audits by
living one document outside the check's scope.

**Fix.** The citation names the test and the file it is in, and the register
joins `inScope`. Adding it surfaced no other broken citation: every other test
name in the register resolves.

**Evidence.** `docs/build/DECISION_REGISTER.md:151`;
`internal/event/relay_integration_test.go`; `test/docs/references_test.go`
(`inScope`); `TestDocs_EveryTestTheyNameExists`,
`TestAuditDocs_TheDecisionRegisterCitesTestsThatExist`.

## F-253 · The journey told a caller to read a field the contract does not declare · PRODUCTIZATION · P3 · FIXED

**Found by** the browser end-to-end audit.

`USER_JOURNEY.md` §1 said `GET /v1/me` answers `onboarding.state: NEW` at the
callback and `onboarding.state: ONBOARDED` at the end of the welcome flow. There
is no such property and there never was: D-053 settled the question in the other
direction — onboarding is **timestamps**, the steps are independent, may be done
in any order and cannot be undone — and `openapi.yaml`'s `Onboarding` schema is
`started_at`, `complete`, `completed_at`, `next_step` and `steps[]`. A client
written from that table branches on a field that is always undefined, which reads
as "not onboarded" forever, on the two rows that decide where a new customer
lands.

**Fix.** The callback row says `onboarding.complete: false` with
`next_step: PROFILE`; the done row says `onboarding.complete: true` with a
`completed_at`, every entry in `onboarding.steps` complete, and no `next_step`.

**Evidence.** `docs/product/USER_JOURNEY.md:41,44`; `openapi/openapi.yaml`
`Onboarding`; D-053;
`TestAuditDocs_EveryOnboardingFieldADocumentNamesIsDeclared`, which reads the
property names out of the contract so renaming one fails here rather than in
somebody's client.
## F-251 · The agent-detail sweep skipped with a reason that was not the reason · PRODUCTIZATION · P3 · FIXED

**Found by** the browser end-to-end audit (F-e2e-3).

`apps/web/e2e/audit-frontend.spec.ts` skipped its `/agents/:agentId` axe and
reflow sweep with "no agent on this account to open". The reason is the one
`scenarios/d-agent.spec.ts` states: `cmd/api/wire.go` builds the strategy
service with no compiler, so no strategy version is produced and no agent can
exist on any deployment of this build. Both sweeps of that route therefore skip
on every fresh-database run, which is F-216's shape recurring on the very route
F-213 added to the lists -- it has no axe, 375 px or honesty coverage anywhere.

**Fix.** The skip names the real reason. The route still has no subject: giving
it one is a product change (a compiler backend on the sandbox tier, or a fixture
agent from the demo seeder), recorded as a residual rather than done here.

**Evidence.** `apps/web/e2e/audit-frontend.spec.ts`; `apps/web/e2e/scenarios/d-agent.spec.ts`.

## F-252 · Two webhook-driven scenarios counted as passes while proving only an unsigned refusal · PRODUCTIZATION · P3 · FIXED

**Found by** the browser end-to-end audit (F-e2e-2).

`scenarios/g-refund.spec.ts` "the refund webhook produces a reversal the
customer can see" and `scenarios/h-dispute.spec.ts` "the dispute webhook freezes
value the customer can see is frozen" both `return`ed early when the Stripe
webhook secret and captured charge were absent, after asserting only that an
unsigned delivery is refused. Those two variables are set nowhere in the
repository -- CI's `web-e2e` job included -- so the early-return branch is the
only one that had ever run, and the suite counted two passes for a refund and a
dispute nobody had ever driven. Separately, `scenarios/b-buy-credits.spec.ts`
registered four tests only when a publishable key was present, so they were
absent from the counts entirely and nothing said so.

**Fix.** The unsigned-delivery check is its own always-running test in each
file; the driven test skips with the exact precondition, so it lands in the
skip column. The four purchase tests are registered on every run and skip with
the reason when the build has no key; the refusal-branch test skips, with its
reason, when it has one.

**Evidence.** `apps/web/e2e/scenarios/g-refund.spec.ts`, `h-dispute.spec.ts`,
`b-buy-credits.spec.ts`; the merged Playwright run after the change (the skip
column grows by the cases that were never proven).

## F-185 · Any authenticated person could end the API process by closing a stream · PRODUCTIZATION · P1 · FIXED

**Found by** the agents-notifications adversarial audit (goal §54), reproduced
on `audit/agents-notifications` @ `8a69aaa`.

`Hub.Publish` snapshots its subscribers under `h.mu`, releases the lock, and
then sends on each `Subscriber.ch`. `Hub.Unsubscribe` — which every SSE request
runs on its way out (`defer h.hub.Unsubscribe(sub)`) — closed that same channel
under the lock. A send on a closed channel panics, and the panic lands in the
**publishing** goroutine, which in `cmd/api` is the notification follower's
ticker. Nothing recovers it: one free instance, one process, gone.

The window is not exotic. It is open for the whole 50ms grace whenever a
subscriber's buffer is full, and for the length of one channel send otherwise,
on every publish to every departing client. A person with a browser tab and a
reload key needs no privilege beyond having an account.

**Fix.** The channel a publisher sends on is never closed, by anybody. A
`Subscriber` gains `done`, closed exactly once by `Unsubscribe` **after** the
subscriber has left the hub's map, so a publish that started earlier is released
and one that starts later cannot see it at all. `Publish` selects on the send,
on `done`, and on a single timer reset per subscriber — replacing a `time.After`
per subscriber per publish, each of which held a runtime timer for the full
grace even when the send succeeded. The slow-consumer drop, which used to be
signalled by closing the channel, is now an explicit flag the SSE loop reads to
decide whether the client is told to resync. Commit 3a5f99f.

**Evidence.** TEST_UNIT: `TestAuditAgnot_PublishSurvivesASubscriberDisconnectingMidSend`
(the auditor's reproduction, inverted — Publish must return, having delivered
nothing, and must not panic), `TestAuditAgnot_PublishRacesAnOrdinaryDisconnect`
(2,000 connect/disconnect cycles against a live publisher),
`TestHub_SlowSubscriberDropped` (the property is unchanged: one stalled client
cannot back-pressure the publisher). RACE_DETECTOR: `go test -race
./internal/stream/` passes on this host with `CC=C:/toolchain/mingw64/bin/gcc.exe`,
a GCC whose path has no space — F-125 records why the default one cannot link,
and this is the first race claim in this repository checked from this desk.

## F-186 · Three clocks for one notification, and the resume compared two of them · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit.

A notification had three instants and the resume path treated them as one:

- `notifications.created_at` is the **occurrence** — `Producer.Emit` binds it to
  `Notification.OccurredAt`, which the follower copies from the source row. A
  capture from five minutes ago that the follower writes now is stamped five
  minutes ago.
- the id a **live** event carries is assigned by `Hub.Publish` from the hub's
  clock: the instant the pass published it.
- `notifications.Since` — the durable half of `Last-Event-ID` — answered with
  `created_at > since`, comparing the second against the first.

So a client whose last live event was published at P asked for everything after
P, and every notification whose occurrence sat at or before P but which was
**written** after it was never returned. That is every row the two-minute lap
picks up and every row from the tick the client was away for. `sse.go` then
skipped the buffer's copies of the same notifications because the hook had
"succeeded", so the in-memory replay did not cover it either. The person is
never told, and nothing anywhere reports a failure.

**Fix (D-104).** Migration 00801 adds `notifications.inserted_at` — when the row
became a fact, from `clock_timestamp()`, immutable under the guard, indexed as
`(user_id, inserted_at, id)` — and `Since` filters and orders on it. A replayed
event's id encodes the same instant, so a client that disconnects mid-replay
resumes from where it got to instead of being sent backwards to an occurrence
five minutes old. The buffer's notifications are replayed alongside the table's
rather than suppressed: the live id is a third clock that always sits at or
after the insert, and the overlap between them is exactly the gap a person falls
into. A notification delivered twice costs the client one query invalidation.
Commit 18657cf.

**Evidence.** REAL_DB_INTEGRATION:
`TestAuditAgnot_LastEventIDSkipsNotificationsWrittenBehindThePublishInstant`
(the auditor's reproduction, unchanged),
`TestIntegration_NotificationResumeReadsTheTableNotTheBuffer` and
`TestIntegration_SinceIsOldestFirstAndReportsTruncation`, both of which now
resume from an insert instant and assert that a cursor past every occurrence
still misses nothing.

## F-187 · An agent was granted authority over a strategy version its owner never owned, named or accepted · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit.

`agents.Service.Create` validated that `StrategyID` and `StrategyVersionID` were
non-empty, and nothing else. Three consequences, each proven separately:

1. **Somebody else's strategy.** A stranger could create an agent on their own
   account bound to the owner's private compiled version. The `agent_grants` row
   — the document an audit of "what did this person agree to" reads — recorded
   authority over IR the grantor cannot read. Goal SS18 requires the compiled
   strategy to be the thing the owner READ and APPROVED before anything is
   activated; ADR-0029 says an agent "is created only from" a compiled version.
2. **A version of a different strategy.** `agents` carries two independent
   foreign keys and had no consistency check between them, so `strategy_id` and
   `strategy_version_id` could name two different strategies and every read that
   joined through either key answered a different story.
3. **A version nobody accepted.** `strategy_versions.accepted_by_user_id` /
   `accepted_at` is the schema's only record of the approval step, and `Create`
   read neither column.

**Fix (D-105).** `Create` loads the version with its strategy inside the
transaction that writes the agent and the grant, `FOR SHARE` on both rows, and
refuses unless the strategy's owner is the requesting account, the version
belongs to the named strategy, and the version is ACCEPTED with a non-null
acceptance. The first two answer NOT_FOUND, identically, so the refusal is not
an oracle about somebody else's strategy; the third answers
INVALID_STATE_TRANSITION, because the version is the caller's own and what is
missing is the approval. Migration 00802 adds `UNIQUE (strategy_id, id)` on
`strategy_versions` and a composite foreign key from `agents (strategy_id,
strategy_version_id)`, so a writer that is not this service cannot record the
mismatch either; MATCH SIMPLE keeps a DRAFT agent with no version legal.

The audit's note about the ownership helpers is fixed with it:
`agents.ownerActor`, `requireOwnership` and the strategy equivalents used
`security.RequireAccount`, which admits `account:read_any` — a READ capability —
and now use `RequireAccountOwner`. `Act`, `Get` and reading somebody else's
strategy answer NOT_FOUND rather than FORBIDDEN, so a stranger cannot tell a
real id from a guessed one. Commit ef38244.

**Evidence.** REAL_DB_INTEGRATION:
`TestAuditAgnot_AnAgentCannotBindToAnotherAccountsStrategyVersion`,
`TestAuditAgnot_TheStrategyVersionMustBelongToTheStrategy` (which also drives
the composite foreign key directly, as the application role, and reads back
SQLSTATE 23503), `TestAuditAgnot_AnAgentIsRefusedFromANeverAcceptedVersion`
(refused, then accepted, then allowed — so the refusal is about the approval and
nothing else), `TestIntegration_OnlyTheOwnerActsOnTheirAgent`,
`TestIntegration_AStrangerCannotReadOrCompileSomebodyElsesStrategy`.

## F-188 · A sign-in's IP address and User-Agent were copied into the one table nothing can purge · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit.

The follower copied `security_events.ip` and `user_agent` verbatim into
`notifications.data`. The two tables have opposite retention properties, and
that is the whole finding:

- `security_events` is partitioned by month **so that a month can be DROPped**
  (00740), and `cmd/api`'s retention pass drops them.
- `notifications` refuses DELETE from every role including the table's owner
  (`notifications_guard`, SQLSTATE LG003), and no retention pass anywhere names
  it. `cp_readonly` and `cp_ops` both held SELECT on it.

So a person's address and a fingerprintable header were copied out of the place
built to forget them into the place that cannot, where two analytics roles could
read them forever. And `privileges_test.go` listed `notifications` under
`opsHousekeeping` — "the only tables the operations role may DELETE from:
transient records whose retention is an operational policy" — asserting a
retention capability the table's own trigger refuses. A grant nobody can
exercise, asserted by a test, is the shape of a control that reports success
having done nothing.

**Fix (D-106).** The notification carries a /24 (or /48 for IPv6) locality,
masked **in SQL** so the exact address never enters the process, a two-word
device summary parsed from the User-Agent (browser and platform; no version, no
build, no device model), and `exact_address_at: /v1/me/audit` — the caller's own
security trail, which serves the exact address and can be dropped. Nothing is
lost; it is read from the copy that has a retention policy. Migration 00803
revokes SELECT from `cp_readonly` and `cp_ops` under ADR-0021 §4 ("a new table
holding personal data joins that list and gets its own REVOKE"), and revokes the
unusable DELETE with it; `opsHousekeeping` no longer claims it. Commit b55fd7c.

**Evidence.** REAL_DB_INTEGRATION:
`TestAuditAgnot_ALoginsIPAddressDoesNotLandInATableNothingCanPurge` (the
auditor's reproduction, inverted — no `ip`, no `user_agent`, a /24 prefix, the
exact address still in `security_events`, neither role holding SELECT or DELETE,
and the guard still refusing the owner's DELETE, which is why the content is
bounded), `TestIntegration_ANewSessionTellsThePersonWhoSignedIn`,
`TestIntegration_ApplicationRolePrivileges`. TEST_UNIT:
`TestDeviceSummary_NamesTheBrowserAndThePlatformAndNothingElse`,
`TestNewSessionBody_AlwaysSaysWhereTheExactAddressIs`.

## F-189 · A compiler backend's answer was written verbatim, including whose strategy it was under · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit.

`StrategyService.persist` wrote whatever the `CompilerBackend` returned.
`CompilerBackend` is a declared seam — ADR-0029 names it, `*strategy.Compiler`
satisfies it today, and whatever a later deployment wires satisfies it tomorrow
— so the backend could return a version naming a **different account's**
strategy, at a version number nobody reserved, already `ACCEPTED`, with an
`ir_hash` that describes no document, and this service would insert it and point
the victim's strategy at it. Attempts were unchecked in the same way.

P3 rather than P2 because today's only backend is `*strategy.Compiler`, which
does not lie. The finding is about what the seam admits, and a seam is a
contract with whoever arrives next.

**Fix.** `checkVersion` refuses a version whose `StrategyID` is not the strategy
that was compiled, whose `Version` is not the number `nextVersionNumber`
reserved, whose `Status` is not `COMPILED` (ACCEPTED is the record of a **person**
approving, and a backend that could set it would be approving on the user's
behalf), or whose `IRHash` is not `ir.SemanticHash` of the IR it came with.
`checkAttempt` refuses an attempt at another strategy or numbered outside
1..`HardMaxAttempts`. Both run before the transaction opens. What a backend is
trusted for is the IR; every identifier around it is this service's. Commit
ef38244.

**Evidence.** REAL_DB_INTEGRATION:
`TestAuditAgnot_ACompilerBackendCannotWriteAVersionUnderSomebodyElsesStrategy`,
which drives a lying backend through all four refusals one at a time, asserts
nothing was written and the victim's `current_version_id` is untouched, and then
proves the control is not blanket by compiling the honest version successfully.

## F-190 · Every data.changed signal in the lap was republished on every pass · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit.

`runSource` appended `c.Signal` unconditionally while the notifications it
travelled with were deduplicated by the unique index. The follower re-reads a
two-minute lap on every pass, so one fill's invalidation signals were broadcast
once per pass for the whole lap — eight times at the fifteen-second tick — and a
market pause's signal, which is `Broadcast`, went to **every connected client**
eight times rather than once.

**Fix.** A change's signals are collected only when at least one of its
notifications was created: the `Created` branch is already the branch that
decides whether anybody is told at all, which is the same question a "this is
now stale" signal answers. One `if`, kept deliberately small because
`fix/platform` is changing the cursor logic in the same function. Commit
84ccd7d.

**Evidence.** REAL_DB_INTEGRATION:
`TestAuditAgnot_TheLapRepublishesTheSameSignalsEveryPass` (the auditor's
reproduction, whose assertions were already written in the fixed direction),
`TestIntegration_TheFollowerNotifiesOnceForACapturedPurchase`.

## F-191 · Two documented mechanisms the code does not implement · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit.

**(a) An in-flight marker nothing can observe.** Migration 00783 gave
`notification_follower_cursors` a `pending_at` column and said it "is written
BEFORE the notifications of a pass are committed and cleared after, so a crash
between reading and emitting leaves the earlier position on record". Both
sentences are true of the code and the mechanism they describe does not exist:
`markPending` and `saveCursor` run in the **same transaction** as the emits, so
no other session can ever observe a non-NULL `pending_at`, and a crash rolls the
write back with everything else. The recovery it claims is really provided by
the transaction and by the dedup key, which is what 00783's own closing argument
says. The only reader of the column was a test asserting it is NULL.

**(b) A comment that outlived its subject by forty lines.** `cmd/api/wire.go`
still said "No producer is attached: the event bus adapter lives in a package
this binary does not yet depend on, so the stream carries heartbeats only until
it is wired" — immediately above the block that attaches the producer, in the
file that does the attaching. ADR-0028 quotes that sentence as evidence for the
state of the system.

**Fix.** 00803 drops the column and the write goes with it; the follower's own
tests assert the property that is actually true (a re-read pass leaves the
cursor no further back than it found it) rather than that a fixture column is
NULL. The wire comment says what the code does. Commits b55fd7c, 87f5c4d.

**Evidence.** REAL_DB_INTEGRATION:
`TestIntegration_ACrashBetweenReadingAndEmittingDuplicatesNothing`,
`TestIntegration_EverySourceQueryRunsAgainstTheRealSchema`,
`TestIntegration_Migrations`. STATIC_PROOF: the comment and the wiring are forty
lines apart in one file.

## F-192 · The market-pause fan-out was bounded by a variable that may be zero · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit.

`readMarketPauses` notifies everyone who has ever traded the market, and said
so: "the fan-out is bounded by the launch tier's own account ceiling
(`CP_CAPACITY_MAX_ACCOUNTS`), so it is not capped here". That variable may be
zero — `capacity.Budget` documents zero as "no ceiling of this kind", which is
how a paid tier turns the infrastructure ceilings off — and config validation
requires it to be stated only in STAGING and PROD. On any other tier the bound
was nothing at all, and one pause on a popular market was one transaction
holding a pool connection while it wrote a notification per holder. The failure
mode is not slowness: a pass that cannot finish inside the statement timeout
fails, and the next pass fails the same way, so the source stops.

**Fix (D-107).** `MaxPauseFanOut` caps one pass at five hundred, and the
participant query skips whoever already has this pause's notification and
whoever switched the kind off — the two exclusions that make the cap safe to
page, so consecutive passes drain the remainder while the transition row is
still inside the lap and the dedup key makes the re-read free. Compiled in
rather than configured, for D-086's reason. Commit 84ccd7d.

**Evidence.** REAL_DB_INTEGRATION:
`TestIntegration_ABreakerTripTellsTheMarketsTraders`,
`TestIntegration_EverySourceQueryRunsAgainstTheRealSchema`.

**What this entry does not claim.** The paging is proven at one holder, not at
five hundred and one: a fixture with that many holders needs that many balanced
journal transactions, which is not affordable in this suite. What is proven is
the query the cap uses and both exclusions in it; the arithmetic above the cap
is the same code path. The residual is stated rather than hidden.

**The renamed reproductions.** Six of the auditor's tests asserted the defect
and could only pass while it existed. Each keeps its narrative, its controls and
its `TestAuditAgnot_` prefix, and asserts the fixed behaviour under a name that
says so, so the six originals —
`TestAuditAgnot_PublishPanicsWhenASubscriberDisconnectsMidSend`,
`TestAuditAgnot_AnAgentBindsToAnotherAccountsStrategyVersion`,
`TestAuditAgnot_TheStrategyVersionNeedNotBelongToTheStrategy`,
`TestAuditAgnot_AnAgentIsCreatedFromANeverAcceptedVersion`,
`TestAuditAgnot_ACompilerBackendWritesAVersionUnderSomebodyElsesStrategy` and
`TestAuditAgnot_ALoginsIPAddressLandsInATableNothingCanPurge` — do not exist
under those names any more. The names cited in the eight entries above are the
same tests.
## F-193 · A market whose marginal price truncates to zero can be opened, and the impact, slippage and breaker controls then fail open · PRODUCTIZATION · P1 · FIXED

**Found by** the independent adversarial audit of the `markets` area (goal §54),
reproduced on `audit/markets` as
`TestAudit_AMarketWhoseSpotTruncatesToZeroIsNotOpenable`.

The marginal price is `(V+R)·10^18 / Y`, truncated, and nothing bounded `Y`
against `V`. `SupplyModel.Validate` has no ceiling on max supply,
`POST /v1/native-assets` accepts any integer string at up to eighteen decimals,
and `checkOpeningLiquidity` bounded only `V`, from below, at 1,000 Credits. So
ten billion units of an eighteen-decimal asset — `Y0 = 10^28` — launched at that
floor priced at `10^27/10^28`, which truncates to ZERO, and stayed zero as it
traded: on this curve `x = V+R` never falls below `V` and `y` never rises above
`Y0`, so the price can only rise from the opening one, and an opening price of
zero is a price of zero for the life of the market.

Four readers then read that zero and not one of them reported a failure.
`PriceImpactBPS` and `Fill.SlippageBPS` divided by it, returned 0, and passed an
order whose true impact was 12,500 basis points through a 9,000 basis point
ceiling. `applyBreaker` returned early on a zero reference, so no circuit
breaker could ever arm on such a market. `MarketValueCredits(q, 0)` is zero, so
`GET /v1/me/portfolio` marked every holder at nothing and reported
`unrealized_pnl_credits` of minus the whole basis on a position bought seconds
earlier.

**Fix (D-108).** Three changes, at three different heights.

`checkOpeningLiquidity` additionally requires the OPENING price to be at least
`MinSpotUnits` = 10^6 units of price scale. Every §47 control is a ratio quoted
in basis points, and one basis point of a price `S` is `S/10,000` units, so
below 10^4 a basis point is not representable at all and 10^6 leaves two further
digits. It is checked once, at creation, because the price cannot fall below the
opening one; it is compiled in rather than a policy value, because a deployment
may decide how deep a market must be and may not decide that a price of zero is
measurable. At the default six decimals it is not a constraint anybody meets (a
billion tokens on the 1,000-Credit floor open at 10^13); at eighteen decimals it
binds, and a creator who wants more supply has to say what pool prices it.

`PriceImpactBPS` is computed from the RESERVES — `|Y² − Y'²| / Y'²`, which is
exactly `(Y/Y')² − 1` for both directions — rather than from two rendered
prices, so it cannot lose the measurement to a rendering. A fill that does not
carry both reserves saturates. `SlippageBPS` and `moveBPS` saturate on a
reference below `MinSpotUnits` instead of reporting zero, and `applyBreaker`'s
early return on a zero reference is gone, so an armed breaker on an unmeasurable
market pauses it to CLOSE_ONLY (holders can still sell) instead of concluding
nothing happened.

`ratioScaled` reports one unit rather than zero for a positive ratio: zero is
not a small price, it is "free". Migration 00805 moves the database's copy of
that arithmetic the same distance through `cp_native_market_price`, and
`discovery.go`'s PRICE and CHANGE_24H sort keys call the same function, so the
three statements of this arithmetic still agree to the unit.

**Evidence.** TEST_UNIT: `go test ./internal/nativemarket/ -run TestAudit_` —
green (was four failed assertions). TEST_UNIT:
`TestCheckOpeningLiquidity_RefusesAPriceTooSmallToMeasure`,
`TestTheSafetyReadersFailClosedOnAPriceTheyCannotMeasure`,
`TestPriceImpactBPS_IsTheMarketsMoveNotTheCallersCost` (restated on reserves).
TEST_INTEGRATION: `internal/nativemarket` — green, including
`TestIntegration_TheSafetyPolicyRefusesWhatItSaysItRefuses` and
`TestIntegration_TheCircuitBreakerPausesToCloseOnly`. STATIC_PROOF:
`internal/nativemarket/{curve,safety,discovery}.go`, `migrations/00805_*.sql`.

## F-194 · Four SECURITY DEFINER functions written after F-48 repeat F-48 · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAudit_APositionCannotBeForgedThroughAnUnpinnedSearchPath` and
`TestAudit_TheNM001PrintCheckReadsOnlyTablesTheCallerCannotControl`.

Migration 00717 pinned `pg_catalog, public, pg_temp` on five SECURITY DEFINER
functions and stated the hazard in prose: TEMP on a database is granted to
PUBLIC by default and is revoked nowhere in this tree, so any caller may create
a temp relation, and with `pg_temp` unpinned it is searched FIRST for relation
names. It left no guard. Migrations 00771 and 00772 then wrote four more
functions with `SET search_path = public` — `cp_native_market_check_print`,
`cp_native_position_allocate`, `cp_native_position_apply_fill` and
`cp_native_positions_unreconciled`.

Two of them bite. ADR-0027 §1 and D-063 say "cp_app holds SELECT on
native_positions and nothing else ... there is no application path by which a
cost basis can be set to something the trades do not support". cp_app really
does lack INSERT on the table — and it may open a market, and
`cp_native_position_allocate` reads `native_assets` to decide whose creator
allocation to write and how large it is. Shadow that relation in `pg_temp` and
the trigger writes a position with an owner and a quantity of the caller's
choosing. The foreign keys resolve by OID and cannot be shadowed, so the market
still points at the real asset; only the trigger's view of it is forged. The
NM001 print check has the same shape against `native_market_fills`: the fill's
foreign key still refuses the fabricated print, so the control that refuses it
is a key rather than the price check written to enforce the claim.

**Fix (D-109).** Migration 00804 pins all four, in 00717's shape. The part that
matters is the guard: `test/integration/migrations` now asserts over the
catalogue that EVERY `prosecdef` function in `public` names `pg_temp` in its
`proconfig`, with a negative control naming the functions the rule exists for,
so the next migration to forget it fails in the suite rather than in an audit.

**Evidence.** TEST_INTEGRATION: both audit reproductions green — the forged
position no longer exists, and the print is refused with NM001 by the check
rather than by the foreign key. TEST_INTEGRATION:
`TestIntegration_EverySecurityDefinerPinsPgTemp`. STATIC_PROOF:
`migrations/00804_*.sql`, `test/integration/migrations/privileges_test.go`.

## F-195 · Every order response reports state_version_after 0 · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAudit_AFillReportsTheStateVersionItProduced`.

`NativeFill.state_version_after` is a REQUIRED field of the order response and
the trade ticket prints it ("Recorded at state version N",
`apps/web/src/pages/markets/Ticket.tsx`). `curve.go` never set
`Fill.StateAfter.Version` — its own comment said "the persistence layer assigns
it" — and `Execute` wrote `st.Version+1` straight into the fill row without ever
putting it back on the Fill. So every trade in every deployment reported 0, and
every ticket said "Recorded at state version 0".

**Fix.** `Execute` assigns it once, before the INSERT, and the fill INSERT and
the print both take it from there, so the three cannot disagree. The replay path
reads the fill's own `seq` — which is the version 00712's apply trigger moved
the market to — rather than recomputing from current state, which would report
today's version for yesterday's trade.

**Evidence.** TEST_INTEGRATION:
`TestAudit_AFillReportsTheStateVersionItProduced` green;
`TestIntegration_ExecutionIsIdempotent` asserts the version on both the original
and the replay. STATIC_PROOF: `internal/nativemarket/{service,repository,curve}.go`.

## F-196 · The public market list carries creator_account_id and accepts it as a filter · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAudit_ThePublicMarketListCarriesNoIdentity`.

D-080 made `GET /v1/native-markets` public (unauthenticated) on a stated ground:
"the list carries product data only — no balance, position, holder or identity".
Every row carried `creator_account_id`, which `openapi.yaml` marked REQUIRED on
`NativeMarketSummary`, and the route accepted `?creator_account_id=` as a
filter. So an anonymous caller could both read the identifier and enumerate one
account's creations by it. The premise was true of what the list was FOR and
false of what it returned.

**Fix (D-110).** `creator_account_id` is off `NativeMarketSummary` and the
filter is off the public route. One flag in the compiled-in projection drives
both halves — the column is blanked and the predicate that would read it matches
nothing — so a creator question on this surface returns an EMPTY page rather
than a different question's answer, and the two cannot come apart. The creator
is a field of `NativeMarketDetail`, served by the gated
`GET /v1/native-markets/{id}/summary`, where a signed-in caller is looking at a
market they are about to trade. `authz.go`'s comment and D-080's "why derived"
sentence now describe the response rather than the intention, and the web's
"Created by" row reads the detail's field and renders nothing when it is absent.

**Evidence.** TEST_INTEGRATION:
`TestAudit_ThePublicMarketListCarriesNoIdentity` green;
`TestIntegration_MarketDiscoveryFiltersSortsAndSearches` restated — the list row
carries no creator and the detail row does. TEST_UNIT:
`TestGetNativeMarkets_PassesEveryFilterThroughAndSaysWhetherPagingIsStable`
asserts the rendered body contains no `creator_account_id`. WEB:
`pnpm --filter @controlplane/web typecheck|test|build` green. STATIC_PROOF:
`internal/nativemarket/discovery.go`, `internal/httpapi/{handlers_native_markets,authz}.go`,
`openapi/openapi.yaml`, `docs/build/DECISION_REGISTER.md` (D-080 correction).

## F-197 · The market summary hands every top holder's account id and exact holding to any signed-in customer · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAudit_NoSignedInAccountLearnsAnotherAccountsHolding`.

`GET /v1/native-markets/{id}/summary` and `GET /v1/native-markets/{id}` returned
`top_holders[]` as `{account_id, quantity}` to any caller holding
`native_asset:read`, which every customer role has. So any signed-in stranger
could read a market's largest positions by account and watch them move trade by
trade. `prints.go` states the opposite rule for the tape this package owns —
"who did it is not the public's business (§16's 'never expose ... internal
sensitive evidence' applied to other people's positions)" — and a position is a
stronger version of the same fact than a print.

**Fix (D-111).** A holder row is a rank, a quantity and a share of the units
accounts hold. No account id reaches the wire at all, not even the caller's own,
because a field that is sometimes an identity is one a client will eventually
render as one; the caller's row carries `is_you`, resolved from an optional
`account_id` through the existing ownership check, so a person can still find
themselves on a page that names nobody. The rank and the share's denominator are
computed over EVERY holder rather than over the returned page. A named holder
list is surveillance and belongs behind `native_market:surveil` on a route of
its own; none is added here, and the permission stays unrouted.

**Evidence.** TEST_INTEGRATION:
`TestAudit_NoSignedInAccountLearnsAnotherAccountsHolding` green. TEST_UNIT:
`TestGetNativeMarketsMarketIdSummary_NamesNoHolder` — the rendered `top_holders`
contains no `account_id`, an account the caller does not own is 403. WEB:
typecheck, test and build green. STATIC_PROOF:
`internal/nativemarket/repository.go`, `internal/httpapi/handlers_native_markets.go`,
`openapi/openapi.yaml` (`NativeAssetHolder`),
`apps/web/src/pages/markets/MarketDetail.tsx`.

## F-198 · Content a moderation verdict REJECTED stays on the public list · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAudit_RejectedContentLeavesThePublicMarketList`.

`listMarkets` had no predicate on `a.content_moderation_state` and no default
status filter, and `GET /v1/native-markets` is public (D-080). So a REJECTED
moderation verdict removed nothing: the asset's creator-supplied name, symbol,
description and image kept being served to anonymous visitors, with
`moderation_state: REJECTED` printed beside the content it rejected.
`nativeasset.SetModeration` deliberately does not halt the market ("a signal for
an operator to halt it, not an automatic halt"), and halting did not remove it
from the list either.

**Fix.** Two always-present, NULL-disabled exclusions in the same compiled-in
statement: a moderation state to exclude, and a market status to exclude. A
discovery list excludes REJECTED and, when the caller named no statuses,
DELISTED; `?status=DELISTED` reaches it. FLAGGED stays visible, because D-065
made flagging a signal for a person to look rather than a verdict, and hiding on
a flag would make reporting an asset a delisting anybody could trigger. Both
exclusions are on the LIST: a read of one named market returns it whatever its
state, because a holder of a rejected or delisted asset still has to be able to
open it and sell.

**Evidence.** TEST_INTEGRATION:
`TestAudit_RejectedContentLeavesThePublicMarketList` green;
`TestIntegration_TheDefaultPageIsWhatIsStillTradable`,
`TestIntegration_AFlaggedAssetStaysOnTheList`. TEST_SECURITY:
`TestSQLInjection_EveryStatementIsBuiltFromConstants` green — the predicates are
parameters of one constant statement, not assembled text. STATIC_PROOF:
`internal/nativemarket/discovery.go`.

## F-199 · A crafted discovery cursor is a 500 from a public route · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, reproduced as
`TestAudit_AMalformedDiscoveryCursorIsRefusedNotAFiveHundred`.

`decodeListCursor` validated the cursor's sort key with `big.Rat.SetString`,
whose grammar is wider than PostgreSQL numeric's: it accepts a quotient (`1/3`)
and a hexadecimal mantissa with a binary exponent (`0x1p2`). Both passed, reached
`$8::numeric`, failed with SQLSTATE 22P02, and came back through `mapError`'s
default branch as `CodeInternal` — a 500 from a public, unauthenticated route on
input the caller chose.

**Fix.** The key is matched against `^-?[0-9]+(\.[0-9]+)?$`, which is what the
cast will accept and is every shape this package writes (exponent notation is
deliberately refused: numeric would take it and this package never produces it).
And 22P02 maps to VALIDATION_FAILED wherever it arrives, because a value that
does not parse as the type it is cast to is the caller's input being wrong. The
message names no value — the database's own text quotes the input back, and a
public error that echoes caller bytes is a reflection surface for nothing.

**Evidence.** TEST_INTEGRATION:
`TestAudit_AMalformedDiscoveryCursorIsRefusedNotAFiveHundred` green. TEST_UNIT:
`TestListCursor_IsOpaqueAndRefusesTampering` extended with the two crafted keys,
NaN, Infinity, exponent notation, whitespace and a bare point, plus the four
shapes this package actually writes. STATIC_PROOF:
`internal/nativemarket/{discovery,service}.go`.

## F-200 · Four claims in the markets area the code does not support · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, by reading each claim against the code it describes.

1. ADR-0027 said "Seven routes" and listed six.
2. ADR-0027 said demo Credits are `PROMOTIONAL`, which no payout policy in this
   build releases. True of the grant a demo trader is funded with, and not of
   the seeder: the seeded trades also issue `MARKET_TRADING_PROCEEDS` and
   `MARKET_CREATOR_EARNING`, and `valuedomain.SandboxPolicy` — the policy a
   sandbox tier actually runs — marks BOTH of those origins withdrawable,
   because rehearsing a payout of earned value is what a sandbox is for. What
   holds them is the other half of the rule: `Execute` issues them at
   `REVERSIBLE` finality, and `FundingFinality.PayoutEligible` is a floor
   beneath every origin policy that only `SETTLED` and `UNFUNDED` value clears.
3. `cmd/api/marketsurfaces.go` says the seeder runs where the deployment "is a
   sandbox tier and asks for it", and nothing asked.
4. `internal/httpapi/middleware.go` said the General rate limiter "applies to
   every authenticated request". Since D-080 it is also the ANONYMOUS budget for
   `GET /v1/terms` and `GET /v1/native-markets`, keyed by IP at 600/min.

**Fix.** (1) Corrected. (2) The paragraph names both controls and says which
holds which, and `TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave` now
asserts the load-bearing disjunction — for every lot in every account the seeder
touched, either its origin is refused or its finality is not payout-eligible —
with a check that `MARKET_CREATOR_EARNING` is among the lots covered, so the
assertion cannot quietly stop testing what it names. (3) Left alone: it is being
fixed on `fix/config-deploy` with a `CP_API_DEMO_DATA` variable, and two
branches editing one file is a merge conflict for no gain. (4) The comment says
what the budget covers and why 600/min per IP is the right number for two reads
of small bounded tables; the `?q=` search's unanchored `ILIKE` costs a scan of
`native_assets`, which has one row per launched asset — the same order of work as
the list beside it — and D-080 records that a trigram index is the fix if that
table ever stops being small.

**Evidence.** TEST_DOCS: `go test ./test/docs/` — green but for the restore-drill
claim, which the orchestrator's drill updates. TEST_INTEGRATION:
`internal/demo` green. STATIC_PROOF:
`docs/adr/0027-a-position-is-the-sum-of-its-fills.md`,
`internal/httpapi/middleware.go`, `internal/demo/seed_integration_test.go`,
`docs/build/DECISION_REGISTER.md` (D-080).
## F-166 · Any caller behind the platform router chose its own rate-limit key and its own audit address by writing one header · PRODUCTIZATION · P1 · FIXED

**Found by** the platform-hardening audit reading `httpapi.clientIP` against
`render.yaml`, reproduced in `internal/httpapi/audit_platform_test.go`.

`clientIP` took the LEFT-most `X-Forwarded-For` entry whenever the immediate peer
was one of `CP_HTTP_TRUSTED_PROXY_CIDRS`, and `render.yaml` trusts the whole
private space because that is the only way the container is reachable. A reverse
proxy APPENDS to whatever `X-Forwarded-For` the client sent — nginx's
`$proxy_add_x_forwarded_for` is the canonical form — so the header the app reads
is "«whatever the client wrote», «the address the proxy saw»", and only the
right-most entry is the proxy's word for who called.

So a caller chose its own transport rate-limit key for every unauthenticated
class, and its own address in `sessions`, `login_attempts`, `security_events`
and `terms_acceptances.source_ip`. The reproduction drove the real `rateLimit`
middleware with a 3-a-minute auth budget and took 300 of it by rewriting one
header.

**Fix.** The list is walked from the RIGHT, discarding entries that are
themselves inside a trusted network (a two-hop chain appends twice) and anything
unparsable, and falls back to `RemoteAddr` when the list says nothing —
`RemoteAddr` being the peer, which no caller can write. The list is truncated to
its last 32 entries before a single one is parsed, so one request cannot ask for
a walk of its own length. "Untrusted peer ⇒ ignore the header" is unchanged.
Commit 1b789a3.

**Evidence.** TEST_UNIT: `TestAudit_ForgedXForwardedForChoosesTheRateLimitKeyAndTheAuditAddress`,
`TestAudit_TheAuthBudgetIsBypassedByRewritingOneHeader`,
`TestRateLimitKeyDistinguishesCallersBehindAProxy` (extended: a multi-entry
header, a chain of balancers, an all-trusted list, an unparsable entry, and the
parse bound), `TestClientIPIsAPlainAddress`. STATIC_PROOF:
`internal/httpapi/middleware.go` (`clientIP`, `isTrustedProxy`,
`maxForwardedForEntries`); `docs/threat-model/THREAT_MODEL.md` §3.9, whose
residual for this row said "middleware absent".

## F-167 · The notification follower's cursor could move backwards, so one ordinary burst stalled a source for the life of the deployment, silently · PRODUCTIZATION · P2 · FIXED

**Found by** the platform-hardening audit reading `Follower.runSource`,
reproduced in `internal/notifications/stall_audit_test.go`.

The pass read from `cursor - lap`, took at most `batch` rows, and made the LAST
of them the new cursor. The new cursor was therefore the 200th row counted from
a point two minutes BEHIND where the cursor already stood — so it could move
backwards, and did as soon as one source produced 200 rows inside one two-minute
lap. Every later pass re-read the same 200 rows, deduplicated all of them and
wrote the same instant back: the source was pinned at that instant forever, and
every later row — a reversal, a failed payout, a frozen account, a new sign-in —
sat behind a cursor that would never reach it. The dedup index made the repeated
re-read produce nothing, so nothing logged and nothing alerted. One trader
spending their own 120/min command budget reaches it.

**Fix.** The two jobs that single read was doing are two reads. The DRAIN reads
forward from exactly where the last pass stopped and is the only thing that
moves the cursor, which is monotone by construction. The LAP re-reads the window
behind the cursor and moves nothing; it runs only when the drain left room in
the batch, because a pass still draining a backlog has not reached the head
where a late commit hides. Two things fell out of it: the cursor may not advance
more than half a lap past the database's own clock (one row carrying a wrong
clock would otherwise drag it into the future and skip everything written
between now and then), and a signal now rides with the fact it accompanies, so
the lap no longer republishes an invalidation every fifteen seconds for every
row of the last two minutes. A full drain that produced no notifications is
logged as a WARN — legitimate after a restore winds a cursor back, and the shape
the stall had (D-097). Commit a1379b0.

**Evidence.** TEST_INTEGRATION:
`TestAudit_FollowerStallsForeverAfterABatchSizedBurst` (260 transitions in one
lap window; the cursor advances, all 260 are told, and a capture an hour later
is reported), plus the package's existing
`TestIntegration_TheFollowerFindsARowThatCommittedBehindItsCursor`,
`…ACrashBetweenReadingAndEmittingDuplicatesNothing`,
`…TheFollowerNotifiesOnceForACapturedPurchase`. STATIC_PROOF:
`internal/notifications/follower.go` (`runSource`, `notAfterCursor`,
`futureHorizon`).

## F-168 · The sanctions screen that gates a payout was writable by one UPDATE with no transition row, one statement after the migration that protected the state beside it · PRODUCTIZATION · P2 · FIXED

**Found by** the platform-hardening audit reading migration 00761, reproduced in
`test/integration/migrations/audit_sanctions_test.go`.

00761 states the rule it enforces: "an application statement could move a
profile from UNVERIFIED to VERIFIED with no recorded edge, no actor and no
provider reference, and the trail a regulator would read was whatever the last
writer said." It fixes that for `identity_state` and then grants
`UPDATE (… sanctions_state …)` back in the very next statement.
`sanctions_state` is not an attribute: `httpapi.WithdrawalDeps.complianceFacts`
reads it into both the eligibility explanation and `payout.EligibilityInput`, so
a screen that is not clear blocks a conversion request outright, and
`verification.sanctionsStateFrom` derives it from the provider's sanctions and
PEP checks. (This sentence used to name `internal/eligibility/evaluate.go` as
the reader. That was wrong when it was written and is recorded in F-226:
`eligibility.Evaluate`, `eligibility.Policy` and the whole `Input`/`Decision`
engine have no production caller on any reachable path, and the surface that
does read the screen is `eligibility.ExplainWithdrawal`. D-120 gave the
conversion path the same reader, which is what makes this sentence true.) The reproduction cleared a screening HIT as `cp_app` in one
statement, in the same session in which the state beside it was refused.

**Fix.** Migration 00796 carries the screen on the transition row that already
exists (`from_sanctions_state`, `to_sanctions_state`; both NULL when the row
says nothing about it), has `cp_compliance_apply_state_transition` write the
column, drops it from the column grant, and adds the constraint trigger that
refuses a change no row describes. `compliance.Upsert` writes that row when the
screen changes, carrying the actor, reason and correlation id it already
required, and re-reads the profile so a caller still gets the truth back. A
profile BORN holding a screen records it through a trigger, because an INSERT is
not a change — the hole F-122 found in the state next to it (D-098). The apply
function also stops a same-state row erasing `verified_at` and `expires_at`,
which is a row shape this migration is the first to create. Commit a6b11bb.

**Evidence.** TEST_INTEGRATION:
`TestAudit_SanctionsStateIsWritableWithNoTransitionRow`,
`TestIntegration_Compliance_ASanctionsScreenIsRecordedAsADecision`,
`TestIntegration_Compliance_AProfileBornScreenedRecordsIt`,
`TestIntegration_TheVerificationStateIsNotTheApplicationsToWrite` (the refused
list now includes the screen), `test/integration/enums` (both new CHECKs paired
against `compliance.AllSanctionsStates()`), `test/security`. STATIC_PROOF:
`migrations/00796_a_sanctions_screen_is_a_decision_not_an_attribute.sql`,
`internal/compliance/profile.go` (`Upsert`, `screen`).

## F-169 · The in-process rate-limit store documented a bound nothing enforced · PRODUCTIZATION · P2 · FIXED

**Found by** the platform-hardening audit reading `ratelimit.MemoryStore`,
reproduced in `internal/httpapi/audit_platform_test.go`.

`MemoryStore` documents itself as having "bounded memory: expired windows are
dropped lazily on access and by Sweep". Neither half held for a key that is
never revisited: the lazy drop replaces the entry for a key that COMES BACK, and
nothing in `cmd/` or `internal/` called `Sweep`. The map therefore retained one
entry per distinct (limiter, key) pair for the life of the process — and with
F-166 above it, the key cardinality was an unauthenticated caller's to choose,
which is the F-117 failure (an unbounded dimension on a 512 MB instance) wearing
a different label.

**Fix.** Two halves. `cmd/api` sweeps the store on a ticker at the largest
configured window for as long as the store exists, and `Sweep` decides per entry
using the window that entry was written with, so a one-minute counter is not
kept alive by a ten-minute ticker. And the store caps itself at
`DefaultMaxKeys`: when one window holds more distinct keys than that, every
counter is dropped and the rest of that window is counted against one shared
budget, reported as a WARN because callers sharing a budget is a refusal
somebody did not earn. The two alternatives are worse — evicting the oldest
hands an attacker a way to evict the counter watching them, and admitting
uncounted keys removes the limit at the moment it is being tested (D-099).
Commit 49fbdaf.

**Evidence.** TEST_UNIT: `TestAudit_TheMemoryRateLimitStoreIsNeverSwept`,
`TestMemoryStore_StopsGrowingAtItsCap`,
`TestMemoryStore_SweepUsesEachEntrysOwnWindow`,
`TestRateLimitSweepInterval_IsTheLargestConfiguredWindow`,
`TestRateLimitSweeper_RemovesExpiredCountersAndStops`,
`TestRateLimitStore_TheMemoryStoreIsSweptForAsLongAsItExists`. STATIC_PROOF:
`internal/ratelimit/ratelimit.go`, `cmd/api/ratelimitstore.go`.

## F-170 · Four expiry passes existed and nothing called them · PRODUCTIZATION · P2 · FIXED

**Found by** the platform-hardening audit grepping for callers of
`gates.Admin.ExpireDue`, `admin.Service.ExpireDue` and
`capital.Service.ExpireDue`, and for anything that expires a verification
session.

Three of them were written, tested by their own packages, documented as "meant
for a periodic worker", and called by nothing in any binary. Nothing expired a
verification SESSION at all. Three of the four are not safety controls, and
saying so is the point: the gate checker already treats a closed window as
inactive, `admin.checkPending` refuses to execute an expired action, and a
reservation past its window is not counted as buying power. What did happen is
that every surface disagreed with its own row.

The fourth is why this was not deferred again. Migration 00762 permits exactly
one OPEN verification session per person. A hosted link that ran out is not open
in any sense the person can use, and nothing moved its status, so its owner
could not start verification again — ever. §20 says "your link expired, start
again", and the product could not honour that sentence.

**Fix.** One ticker in `cmd/api` at the five-minute cadence the other passes
use, each sweep in its own transaction and its own error path, the whole pass
under a timeout, a nil service skipped rather than panicked on.
`verification.Service.ExpireOverdueSessions` closes an expired hosted link and
an attempt the provider was never told about (after `UnstartedSessionGrace`),
through the transition row that is the only thing that writes a session status,
`FOR UPDATE SKIP LOCKED` and batch-bounded (D-099). Commit d3279a6.

**Evidence.** TEST_INTEGRATION:
`TestIntegration_TheExpirySweepsRunInThisProcess` (all four passes against the
real schema; the expired link is closed and its owner can start again),
`TestIntegration_TheSessionSweepClosesWhatCannotBeDecided`,
`TestIntegration_TheSessionSweepLeavesADecidedSessionAlone`. STATIC_PROOF:
`cmd/api/expiresweeps.go`, `internal/verification/expiry.go`;
`docs/product/VERIFICATION_AND_WITHDRAWAL.md` §4 and ADR-0025 §2, which now
describe what runs.

## F-171 · BODY_TOO_LARGE is emitted on every route and was absent from the registered code set · PRODUCTIZATION · P3 · FIXED

**Found by** the platform-hardening audit reading `internal/errs/codes.go`,
reproduced in `internal/httpapi/audit_platform_test.go`.

`CodeBodyTooLarge` is declared, mapped to 413 in the status registry, and
emitted by `captureBody` on every route — and was not in `allCodes`. So
`errs.AllCodes()` returned 53 of the 54 declared codes and everything that
enumerates the set skipped it, including
`TestHTTPStatus_EveryCodeExplicitlyMapped`, whose independent `expectedStatus`
table omitted it too and whose length assertion therefore passed. The published contract's `Problem.code` description did not
name it and `apps/web/src/api/problem.ts` had no case for it.

**Fix.** Registered in `allCodes`, added to the independent expectation table,
named in the OpenAPI description, and given a sentence a person can act on in
the web client ("send less, rather than sending it again"). Commit d7b9447.

**Evidence.** TEST_UNIT:
`TestAudit_BodyTooLargeIsMissingFromTheRegisteredCodeSet`,
`TestHTTPStatus_EveryCodeExplicitlyMapped`, `TestAllCodes_UniqueAndWellFormed`,
`pnpm --filter @controlplane/web test`. STATIC_PROOF: `internal/errs/codes.go`,
`openapi/openapi.yaml` (`Problem.code`), `apps/web/src/api/problem.ts`.

## F-172 · /v1/strategies published timestamps in the server's local offset · PRODUCTIZATION · P3 · FIXED

**Found by** the platform-hardening audit reading `handlers_strategies.go`
against openapi.yaml's own "Timestamps are RFC 3339 UTC", reproduced in
`internal/httpapi/audit_platform_test.go`.

pgx hands a `timestamptz` back in the process's zone. Every other converter in
the package lands `.UTC()` on the way out; `toAPIStrategy` and
`toAPIStrategyVersion` did not, so `created_at`, `updated_at` and `built_at` on
that one route published whatever offset the server ran in.

**Fix.** The three conversions, and an invariant so a third converter cannot
forget: `harness.do` scans every JSON response it records for an RFC 3339
instant carrying a zone offset, which makes every route test in the package a
test of this too. A per-converter table would have been sixty fixtures that the
sixty-first converter is not in. Commit d7b9447.

The finding understated itself, and the invariant is how that was found: the
audit called `/v1/strategies` "the one route that publishes the server's
timezone offset", and the first integration run with the scanner in place
failed on eleven more conversions across five files — the internal commerce
product, seller and order; the native asset, its economics lock and its
activation; a native market, its printed price and its activation; an agent's
creation, last run, grant and pause; a credit purchase's creation, reversibility
and settlement; a portfolio position's first acquisition and last trade; an
admin action's approval and execution; an activity item; a gate transition; and
a kill-switch release. Every one is now UTC. Commit 9b87a8d.

**Evidence.** TEST_UNIT: `TestAudit_StrategyTimestampsAreNotUTC`,
`TestStrategyConverters_PublishUTC`,
`TestTimestampScanner_SeesAnOffsetAndAcceptsZ` (so the scanner cannot pass
vacuously), and the whole `internal/httpapi` suite through `harness.do`.
STATIC_PROOF: `internal/httpapi/handlers_strategies.go`,
`internal/httpapi/timestamps_test.go`.

## F-173 · The body limit was justified by a number that had been wrong since the strategy schemas landed · PRODUCTIZATION · P3 · FIXED

**Found by** the platform-hardening audit reading `bodyLimitFor`'s comment
against the spec.

The comment justified 64 KiB with "the largest field any request schema declares
is 5,000 characters". `CreateStrategyRequest.description` is 8,000. The limit
itself was never wrong; the sentence that says why it is right was, and a
sentence is checked by nothing.

**Fix.** The number is a constant beside the limit and a test derives it from
`openapi/openapi.yaml`, so a schema that declares a larger field fails there
rather than in production. Commit d7b9447.

**Evidence.** TEST_UNIT:
`TestBodyLimit_IsDerivedFromTheLargestFieldTheContractDeclares`. STATIC_PROOF:
`internal/httpapi/middleware.go` (`largestSchemaFieldChars`).
## F-151 · The pricing policy minted a count of Credits into a field that means base units, so $10 bought 0.001 Credits · PRODUCTIZATION · P0 · FIXED

**Found by** the credits-payments audit of goal §54, reading `internal/credit/pricing.go` against every consumer of the quantity it produces.

`PricingPolicy.creditsFor` returned `minor × CreditsPerMajorUnit / MinorUnitsPerMajorUnit` with no `10^decimals`. `CreditsPerMajorUnit` is a count of whole CREDITS per dollar; a `money.Quantity` is asset BASE UNITS everywhere it appears — `internal/money` documents it, the OpenAPI `Quantity` schema documents it, `credit_fundings.credit_quantity` stores it, the `credit_lots` row holds it and the ledger entry moves it. The CREDIT asset is registered with six decimals (`scripts/seedeconomy`, `cmd/api/sandboxtier.go`, `internal/demo`, `CREDIT_ECONOMY.md` §1), so the shipped policy issued a millionth of what it charged for: $10.00 at 100 Credits per dollar minted 1,000 base units — 0.001 Credits — and `BuyCredits.tsx` rendered "100 Credits per 1 USD" above "0.001000 Credits" without either half being able to see the other was wrong.

Nothing in the system could catch it. Every row agreed with every other row, `VerifyProvenance` reconciled, and `PricingPolicy.Validate`'s own guard — "it would take money and give nothing" — only asks whether the minimum purchase issues a POSITIVE quantity, which 100 base units is. The defect was in the unit, not in the arithmetic between the rows. `internal/payout` had the conversion right in the other direction the whole time (`creditsToMoney`, `moneyToCredits`, both scaled), which is what made the omission legible once the two were read together.

**Fix (D-093).** `PricingPolicy` carries `Decimals`, hashed like every other field so a scale change is a new version; `creditsFor` multiplies by `10^Decimals`. `NewPurchaseService` takes a querier, reads the registered CREDIT asset's scale and refuses to build a service whose policy prices a different one — the check `Validate` cannot make because it never sees a deployment — and `cmd/api` moved its Credit-asset registration above the purchase path so there is something to compare against. `GET /v1/credits/pricing` publishes the whole conversion (`decimals`, `minor_units_per_major_unit`, `rounding`) and `GET /v1/credits/balance` carries `credit_decimals` with its figures, so the browser reads the scale rather than hardcoding six.

**Evidence.** TEST_UNIT: `TestAudit_PricingPolicyIssuesBaseUnitsAsIfACreditHadNoDecimals`, `TestAudit_TheSeededCatalogueIsBuyableAtTheShippedRate`, `TestAudit_PricingValidateSeesTheScaleItPricesAt`, `TestPricingPolicy_TheScaleIsTheAssetsAndNotAConstant`. TEST_INTEGRATION: `TestIntegration_APurchaseServiceRefusesAPolicyAtTheWrongScale`, `TestIntegration_APurchaseMintsWhatTheFundingPagePromised` — $10.00 mints a lot of 1,000,000,000 base units and `VerifyProvenance` holds. STATIC_PROOF: `internal/credit/pricing.go`, `internal/credit/purchase.go`, `apps/web/src/lib/credits.ts`.

## F-152 · A chargeback destroyed whichever lots sorted first and stranded the units it reversed · PRODUCTIZATION · P1 · FIXED

**Found by** the same audit, reading `Service.reverseTo` against `openLotsQuery`'s consumption order.

`reverseTo` computed `covered = min(account balance, lot.Quantity)` and handed it to `Consume` with no `AllowedOrigins` and no lot restriction. `Consume` walks an account's lots in CONSUMPTION order — promotional first, competition reward second, purchased fourth — so the units a chargeback destroyed were whichever lots sorted first, not the lot the chargeback was about. The reversed funding's own lot was then stamped REVERSED with its remaining quantity untouched.

Reproduced against a real database: a 300-unit promotional grant and a 1,000-unit card purchase, nothing spent, chargeback of the purchase. The grant ended at zero, 300 units of the charged-back purchase survived inside a REVERSED lot, and the account was left holding `CREDIT_BALANCE` it could never spend, never withdraw and never clear — while `VerifyProvenance` reconciled perfectly, which is why nothing caught it.

**Fix.** The units a funding minted are in exactly two places: still in its lot, or already spent. `reverseTo` destroys the first (`lot.Remaining`) and books the second (`lot.Quantity − lot.Remaining`) as the existing DEFICIT posting, so it needs no balance read to split them. `ConsumeRequest` gains a `LotIDs` restriction — an origin filter is not enough, because two purchases produce two lots of the same origin — and the reversal asserts it consumed exactly its own lot. A provenance check stands in front of the posting: a lot holding more than the account's `CREDIT_BALANCE` is a reconciliation incident, not something to absorb.

**Evidence.** TEST_INTEGRATION: `TestAudit_ChargebackDestroysTheLotItReverses` (the grant survives, the reversed lot ends at zero, `reversed` is 0 and `spendable` is 300), `TestAudit_ChargebackOfSpentCreditsBooksADeficitAndTakesNoOtherLot` (nothing to destroy, 1,000 owed, the grant untouched). STATIC_PROOF: `internal/credit/funding.go` (`reverseTo`), `repository.go` (`openLotsQuery` `$5`).

## F-153 · A failed provider call or an abandoned checkout held the money-at-risk ceiling forever · PRODUCTIZATION · P1 · FIXED

**Found by** the same audit, following every exit from the pre-capture funding states.

`StartPurchase` commits the funding row in phase 1 — deliberately, so a lost response leaves a record to reconcile against (F-96) — and calls the provider in phase 2. A phase-2 failure returns an error to the caller and leaves a committed row in CREATED with a NULL `provider_reference`. `CREATED` and `AUTHORIZATION_PENDING` are both in `capacity.atRiskFundingStates`, and nothing ever moved a funding out of one: `SettleDue` selects `REVERSIBLE`; the reconciliation sweep's `stale()` required `provider_reference IS NOT NULL` and `Reconcile` returned early for an empty reference anyway; nothing else in the binary wrote CANCELED or FAILED. An abandoned checkout was the same hole reached the ordinary way — the provider reports `requires_payment_method` forever, which maps to an at-risk state, so reconciliation had nothing left to do on every subsequent pass.

Two abandoned $1,000 checkouts exhaust `CP_CAPACITY_MAX_AT_RISK_MINOR=200000` and every honest purchase after them is refused `AT_CAPACITY` with no remedy: F-90's failure — "the deployment would have refused every Credit purchase with AT_CAPACITY, permanently" — reached through a different door.

**Fix.** `PurchaseService.ExpireInFlight` ends a pre-capture funding older than `DefaultInFlightLifetime` (24h) through the real CANCELED transition, run by `cmd/api` on the credit ticker and by `cmd/reconciliation-worker`. It asks the provider first, so a payment that got somewhere is reconciled instead of cancelled, and it cancels the PaymentIntent AT THE PROVIDER before cancelling the funding — a funding marked CANCELED over a live intent is a card that can still be charged against a terminal funding that will never mint. `CancelPurchase` joined `PurchaseProvider` for that reason: without it an unfinished purchase is permanent. `CAPTURE_PENDING` is deliberately not expired — once the provider is processing, cancelling is not ours to do and the money really is at risk.

**Evidence.** TEST_INTEGRATION: `TestAudit_AFailedProviderCreateIsCancelledAndReleasesTheCeiling` (the ceiling recovers and an honest purchase is admitted again), `TestAudit_AnAbandonedCheckoutIsCancelledWithTheProviderAndLeavesTheCeiling` (the PaymentIntent is CANCELED at the provider), `TestAudit_ExpiryNeverCancelsAPaymentTheProviderSaysSucceeded`. STATIC_PROOF: `internal/credit/purchase.go` (`ExpireInFlight`, `preCaptureStates`), `internal/provider/stripecredit/client.go` (`CancelPurchase`), `cmd/api/creditsettle.go` (`expireOnce`).

## F-154 · The sweep that recovers a swallowed provider event had no caller in the deployed topology · PRODUCTIZATION · P1 · FIXED

**Found by** the same audit: `grep -rn "\.Reconcile(" cmd/ internal/` named only `cmd/reconciliation-worker`, and `render.yaml` declares two services, both `type: web`, neither of them that worker.

The window is real. `StartPurchase` commits the provider reference in phase 3, AFTER the provider call returns, and a provider can deliver `payment_intent.succeeded` before that commit lands. An event naming a reference no funding holds is `Ignored`, the webhook pipeline records the event id as processed, and the provider's redelivery of the same event id is answered `Duplicate` and never reaches `Dispatch` again. `Reconcile` is the recovery and it was the only one — `cmd/api` deliberately runs `SettleDue` itself for exactly this reason ("the launch tier has no worker tier", F-90) and did not run `Reconcile`. So on the deployed tier a card was charged and no Credits were ever minted.

The listing the recovery depended on made it worse: `stale()` required `provider_reference IS NOT NULL`, which excludes precisely the fundings a crash between phases leaves behind.

**Fix.** `PurchaseService.ReconcileDue` moves the sweep's discipline into the package that owns the state machine — one transaction per funding with the provider call outside it (F-27), rows taken `FOR UPDATE SKIP LOCKED`, idempotent — and `cmd/api` runs it beside `runCreditSettlement` on the same ticker. The provider-reference filter is gone; the listing measures staleness from `created_at`, because `updated_at` is trigger-maintained and any unrelated write would restart it, the same reasoning `SettleDue` gives for measuring from `reversible_at`. `cmd/reconciliation-worker` calls the same method, so the two tiers cannot drift.

**Evidence.** TEST_INTEGRATION: `TestAudit_ASwallowedSucceededEventIsRecoveredByTheInProcessPass` (the funding mints, and a second pass mints nothing more), `TestAudit_TheInFlightListingDoesNotSkipFundingsWithNoProviderReference`. TEST_UNIT: `test/reachability` — `internal/credit.Reconcile` now names its deployment caller instead of failing. STATIC_PROOF: `cmd/api/creditsettle.go` (`reconcileOnce`), `internal/credit/purchase.go` (`InFlightFundings`, `ReconcileDue`).

## F-155 · A card-network inquiry closing settled a funding before its reversibility window · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reading `stripecredit.disputeStatus` against `FundingStateFor`.

`charge.dispute.closed` with status `warning_closed` — the close of an early-fraud-warning INQUIRY — mapped to `PurchaseDisputeWon`, which mapped to `FundingSettled`. `SettleFunding` promoted the lot REVERSIBLE → SETTLED, which is what `FundingFinality.PayoutEligible()` reads. Stripe's documentation is explicit that an inquiry is not a dispute and that a chargeback may still follow one; the adapter's own comment agreed — "an early-warning notice that closed without becoming a dispute". So the money was exactly as reversible as it had been before the notice arrived, and the funding was now in the state `FundingState` documents as "the money is ours", that `internal/capacity` stops counting as money at risk, and that `CREDIT_ECONOMY.md` calls final.

`ManualResolution` refuses to let an operator assert SETTLED for precisely this reason — "an operator who could assert it by hand could make value payout-eligible by closing a ticket". A card network inquiry could.

**Fix (D-094).** `warning_closed` becomes its own status, `PurchaseDisputeLifted`, and both it and `PurchaseDisputeWon` map to `FundingReversible`: winning a dispute is evidence the money is ours, not the fact SETTLED records, which is that the window CLOSED. `DISPUTED → REVERSIBLE` joins the transition table and `apply` unfreezes the lot with it. 00743's trigger coalesces `reversible_at`, so the funding returns to the window it was already in rather than restarting it, and `SettleDue` is the only thing left that settles anything.

**Evidence.** TEST_INTEGRATION: `TestAudit_AnInquiryThatClosesUnfreezesTheFundingAndDoesNotSettleIt` (payout-eligible 0, spendable restored, `settled_at` nil, `reversible_at` unchanged), `TestAudit_AWonDisputeReturnsTheFundingToItsWindowRatherThanSettlingIt` (and a second dispute is still representable). TEST_UNIT: `stripecredit` dispute-status table. STATIC_PROOF: `internal/credit/purchaseprovider.go`, `internal/credit/funding.go` (`UnfreezeFunding`).

## F-156 · The reversed balance bucket was returned by the API and rendered nowhere · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, comparing `credit.Balances` against every page that renders it.

`GET /v1/credits/balance` and `GET /v1/me/portfolio` return `reversed` — units whose funding was clawed back and which still have remaining quantity recorded, which `types.go` says "should be transient and is worth surfacing when it is not". `Home.tsx` and `Portfolio.tsx` built their segmented bar from `[spendable, frozen]` over `total = gross`, and no page rendered the figure at all. Under F-152 that bucket held every stranded unit a chargeback left behind, so the one number that would have made the P1 visible on screen was the one number the interface dropped — and `SegmentedBar` drew short and said nothing, because segments that do not reach their total were simply not drawn.

**Fix.** Home and Portfolio render `reversed` as a third segment, and as a field when it is not zero, with the sentence "Removed after a payment was reversed. Not spendable and not withdrawable." `SegmentedBar` draws whatever its segments do not account for as a labelled "Not accounted for" part, in exact integer arithmetic, so the next bucket a caller forgets is visible rather than silently missing. The difference is computed in `lib/credits.ts` (`unaccountedBaseUnits`) rather than in the component, where the source guards keep money arithmetic out.

**Evidence.** TEST_UNIT: `apps/web/src/lib/credits.test.ts` — "a segmented bar's parts either reach its whole or the difference is visible", including the exact F-152 shape (gross 300, parts 0 and 0) and refusing to draw parts that overshoot their whole. STATIC_PROOF: `apps/web/src/components/SegmentedBar.tsx`, `apps/web/src/pages/home/Home.tsx`, `apps/web/src/pages/portfolio/Portfolio.tsx`, `openapi/openapi.yaml` (`reversed` now required on `CreditBalance`).

## F-157 · SandboxPolicy permitted an origin the document it implements says it refuses · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, holding `valuedomain.SandboxPolicy` against `CREDIT_ECONOMY.md` §4.

The section states that `SandboxPolicy` "permits PURCHASED and the five earning origins once the account reaches PAYOUT_KYC, and refuses PROMOTIONAL, REFUND, ADMIN_ADJUSTMENT and PROVIDER_SETTLEMENT outright", and draws the conclusion the whole section rests on: "A granted Credit that could leave the system would be the first rule somebody copied." That is six permitted origins. The policy permitted seven. `COMPETITION_REWARD` — "a prize or reward from a platform competition", not `EarnedByUser()`, nobody paid for it and nobody earned it — was withdrawable, which gives a platform a payout path whose only gate is a competition it runs itself.

`TestSandboxPolicy` could not see it: it asserted three permitted origins and four closed ones out of eleven, so the other four could be anything.

**Fix (D-095).** `COMPETITION_REWARD` is closed, beside the other four grants. `TestSandboxPolicy` asserts over every origin, and `internal/eligibility`'s per-origin explanation reports `ORIGIN_NOT_WITHDRAWABLE` for it. Nothing in this build issues a `COMPETITION_REWARD` lot, so no held value changes.

**Evidence.** TEST_UNIT: `TestAudit_SandboxPolicyPermitsOnlyTheOriginsTheDocumentLists` (permits exactly the documented six), `TestSandboxPolicy`, `TestExplainWithdrawal_TheSandboxPolicyPerOrigin`. STATIC_PROOF: `internal/valuedomain/sandboxpolicy.go`.

## F-158 · The sandbox flag on a purchase was the deployment's mode today, not a recorded fact · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, tracing `CreditPurchase.sandbox` back to its source.

`toAPICreditPurchase` rendered it from `httpapi.Options.CreditPurchaseSandbox`, a boolean `cmd/api` computed once at startup from `cfg.Providers.CreditPurchase.Mode`. Every funding the endpoint returned carried that same value, including ones opened months earlier under a different mode. It answered "what mode is this deployment in now" to a question that asks "was this payment real", and the direction that costs is the cheap one: a deployment promoted from sandbox to live re-labels every sandbox purchase it ever made as real value — in the API, and in the browser, where `BuyCredits.tsx` picks the ECONOMY rather than SIMULATED temperature from it. A sandbox outcome has to be labelled sandbox everywhere it is stored and shown, and a label recomputed from today's configuration is not stored at all.

**Fix (D-096).** Migration 00793 adds `credit_fundings.provider_mode`, written once at `CreateFunding` from the mode that opened the payment and write-once by privilege (00743 left `cp_app` no UPDATE that could reach it). `CreateFundingRequest.Validate` refuses a request without a mode. `Dispatch` cross-checks the recorded mode against the provider snapshot's `livemode` on every event and parks a disagreement for a person — the check that had nothing to compare against before. A funding with no recorded mode predates the column and renders as sandbox: an unrecorded mode cannot be asserted to be real money. `Options.CreditPurchaseSandbox` is gone.

**Evidence.** TEST_UNIT: `TestCreditPurchaseCarriesItsTemperature` (three modes plus the unrecorded case), `TestCreateFundingRequest_Validate`. TEST_INTEGRATION: `test/integration/enums` pairs `credit_fundings_provider_mode_check` with `credit.AllProviderModes()`; `test/integration/migrations` green on 00793. STATIC_PROOF: `migrations/00793_a_funding_records_the_mode_that_opened_its_payment.sql`, `internal/httpapi/handlers_credits.go`.

## F-159 · The money-at-risk ceiling added in unchecked int64 · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, reading `capacity.Guard.AdmitAmount`.

It computed `after := r.AtRiskMinor + amountMinor` in int64 and refused only when `after > MaxAtRiskMinor`. The sum is unchecked, so an amount near `math.MaxInt64` wraps negative, the comparison passes, and the guard whose entire job is to answer "what could this deployment owe after the action" answers with a number below zero.

It was inert only because `internal/credit` calls the guard BEFORE pricing and `PricingPolicy.CreditsFor` then refuses the amount for exceeding `MaxAmountMinor`. The order of those two calls was the only thing between that arithmetic and a ceiling that could be stepped over, and the guard itself refused nothing.

**Fix.** The comparison is headroom — `amountMinor > MaxAtRiskMinor − AtRiskMinor` — which cannot overflow for any non-negative amount, and which refuses everything when a tier is already past its cap. `POST /v1/payments` bounds `amount_minor` against the maximum `GET /v1/credits/pricing` publishes before any guard measures anything against it, so the guard no longer depends on its caller having done so.

**Evidence.** TEST_UNIT: `TestAudit_AdmitAmountRefusesAnAmountThatWouldOverflowTheSum` (`math.MaxInt64` is refused `AT_CAPACITY`; the headroom boundary still decides the ordinary cases), `TestAudit_AGuardAlreadyPastItsCeilingRefusesEveryAmount`. STATIC_PROOF: `internal/capacity/capacity.go`, `internal/httpapi/handlers_credits.go`.
## F-174 · A cooling-off period was measured against a timestamp its caller chose · PRODUCTIZATION · P2 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_ClosureCoolingOffIsMeasuredAgainstACallerSuppliedTimestamp`.

Migration 00758 states the property: the closure cooling-off period is "enforced
by the trigger below, not only by the service, because the whole point of a
cooling-off period is that it survives a bug in code that is in a hurry".
`cp_closure_request_apply_transition` compared `NEW.occurred_at` against
`cooling_off_until` -- and `occurred_at` is a column the INSERTing role chooses.
One statement, `INSERT ... occurred_at = now() + interval '400 days'`, effected a
closure fourteen days early; the wall clock was never consulted at all. The
whole invariant was the caller's own timestamp compared with a column.

The second half of the same defect is that nothing bounded the stamp. It is the
audit record of when an actor acted, on a table whose rows are the evidence for
"who closed this account and why", and it could say anything.

**Fix.** Migration 00798. The wait is compared against `statement_timestamp()`,
a value no caller supplies -- the statement's clock rather than `now()`'s
transaction clock, so a transaction that began before the wait ended cannot
effect a closure it could not have effected when it wrote the row. A BEFORE
INSERT trigger, `cp_transition_stamp_is_honest`, requires `occurred_at` to be
within two minutes of the database clock in either direction; that bound is
clock skew between the application host and the database and nothing else.

The fixtures that effected a closure by advancing a fake clock past the wait
were relying on the defect. They wait it out on the wall clock now, in a fixture
whose cooling-off period is a fraction of a second.

**Evidence.** `migrations/00798_a_cooling_off_period_is_measured_by_the_clock.sql`;
`internal/profile` `TestAudit_ClosureCoolingOffIsMeasuredAgainstACallerSuppliedTimestamp`
(inverted: both the future-stamped forgery and an honestly-stamped one inside
the wait are refused, and the request stays PENDING),
`TestIntegration_Closure_CannotBeEffectedBeforeTheCoolingOffPeriod`,
`TestIntegration_Closure_EffectingClosesTheUserTheAccountsAndTheSessions`.

## F-175 · The operator directory was freely rewritable by the application role · PRODUCTIZATION · P2 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_TheOperatorDirectoryIsRewritableByTheApplicationRole`.

`operator_roles` is the only source of operator authority in this system:
ADR-0022 put Nodal's authority in Neon, ADR-0024 made this table the one thing
the login path reads, and `internal/identity` takes a role from nothing else. It
carried migration 00010's blanket `GRANT SELECT, INSERT, UPDATE` -- the one
authority-bearing table that never got the treatment 00744 gave `accounts`,
00757 gave `users` and 00758 gave `account_closure_requests`.

Nothing in Go has ever updated it, so the grant served nothing and cost three
things. A revocation did not stay revoked, which is the opposite of what
ADR-0024 §3 promises (the promise was made about the bootstrap INSERT while the
column itself stayed writable). A `SUPPORT_READ_ONLY` row could become `ADMIN`
in place while `granted_by`, `granted_at` and `reason` went on describing the
grant somebody actually made. And nothing recorded that any of it happened:
there was no transition table for the directory at all.

**Fix.** Migration 00799, D-100. `operator_role_transitions` is append-only,
carries the actor, the reason and the correlation id, and names the grant it is
about by the directory's own primary key; a SECURITY DEFINER trigger writes
`revoked_at` / `expires_at` from the row, so the record and the change cannot
come apart. `cp_operator_role_provenance_is_immutable` refuses any change to who
was granted what, by whom, when and why, refuses un-revoking a grant and refuses
moving a revoked grant's expiry -- whoever makes it, including the migration
role. `cp_app` loses UPDATE and keeps it on `reason` alone, because PostgreSQL
requires UPDATE on one column for `SELECT ... FOR UPDATE` (00744) and that is a
column the trigger will not let anybody change.

**Evidence.**
`migrations/00799_a_revoked_operator_role_stays_revoked.sql`;
`internal/profile` `TestAudit_TheOperatorDirectoryIsRewritableByTheApplicationRole`
(inverted: the bare UPDATE is refused 42501, a transition row revokes, and
neither the application nor the owner can undo it);
`test/integration/migrations` `TestIntegration_OperatorDirectoryAuthority`;
`test/integration/enums` (`operator_role_transitions_action_check` against
`operatorroles.AllTransitionActions()`); `internal/identity`
`TestIntegration_Bootstrap_DoesNotRestoreARevokedGrant`.

## F-176 · Both account-lifecycle edge sets existed only in Go · PRODUCTIZATION · P2 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_TheClosureStateMachineHasNoEdgeSetInTheDatabase` and
`TestAudit_AClosedUserCanBeReopenedByTheApplicationRole`.

`internal/profile` declares both state machines -- `closureTransitions`, and the
four legal `users.status` edges 00757's own header spells out in words. The
schema enforced only that a transition row DESCRIBES the change it makes
(00726's edge binding); it never asked whether the change was legal.

So the application role could insert `EFFECTED -> CANCELLED` and move a terminal
closure request, and because `decided_at` and `decided_reason` are coalesced the
row then said CANCELLED for the reason the EFFECT was given. And it could insert
`CLOSED -> ACTIVE` and reopen a closed account, which 00757 says has "no edge out
of it, here or in Go" and that "nothing in the product needs it today, so the
schema does not quietly permit it".

**Fix.** Migration 00798 adds a CHECK constraint on each transition table
carrying the edge set as pairs. A CHECK rather than a branch in the apply
function for three reasons, stated in the migration: it refuses at the INSERT
where the mistake is (00760's argument); it is readable by
`pg_get_constraintdef`, which is what lets `test/integration/enums` hold it
against the Go list, and a branch inside a function body can be compared with
nothing; and a second copy inside the function would be unreachable and would
make every failure ambiguous about which rule fired (F-53). `profile.ClosureEdges`
and `profile.UserStatusEdges` are the Go halves, derived from the transition maps
rather than typed out. `Repository.TransitionUserStatus` refuses the same move in
Go, with the domain's own error.

**Evidence.**
`migrations/00798_a_cooling_off_period_is_measured_by_the_clock.sql`;
`internal/profile` `TestAudit_TheClosureStateMachineHasNoEdgeSetInTheDatabase`,
`TestAudit_AClosedUserCanBeReopenedByTheApplicationRole` (both inverted);
`test/integration/enums` `TestIntegration_EveryDeclaredEnumMatchesItsCheck`
(`account_closure_request_transitions_edge_check`,
`user_status_transitions_edge_check`).

## F-177 · A step-up login minted a second session and left the first live · PRODUCTIZATION · P2 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_StepUpLoginLeavesThePreviousSessionLiveAndUsable`.

PART 192 requires session rotation on privilege change, and `auth.Manager.Rotate`
is the mechanism for it -- with no caller anywhere in the repository. A step-up
login IS a privilege change: it raises the session's `AuthTime` and `AMR`, and it
is the thing a person is asked to do before closing their account or registering
a payout destination.

`identity.Complete` called `Sessions.Issue`, so the pre-step-up session stayed
live, kept its own full absolute lifetime and remained a usable credential
carrying the WEAKER authentication. A stolen cookie survived the step-up that
exists to defend against it. The user's own security page counted one browser
that stepped up as two devices.

**Fix.** D-101. The callback reads the session the browser already holds from
`httpmw.SessionFrom` and hands it to `identity.Complete`, which rotates when the
replacement really is the same login moving forward -- the same subject, the same
actor type, and a session the store still accepts. `Rotate` revokes the old
session and keeps its absolute `ExpiresAt`, so rotation cannot extend a login. A
cold step-up has nothing to rotate from and issues. A step-up offered somebody
else's session issues rather than rotating, so this is never a way to end a
stranger's login. A live break-glass elevation survives with its role, because
it is bounded by the clock and ending an emergency because somebody
re-authenticated more strongly would be the wrong way round.

**Evidence.** `internal/identity/login.go` (`issueOrRotate`),
`internal/httpapi/handlers_auth.go`;
`internal/identity` `TestAudit_StepUpLoginLeavesThePreviousSessionLiveAndUsable`
(inverted: the old session is revoked, the new one records `RotatedFrom`, the
absolute expiry is unchanged, the old token no longer authenticates and the
subject holds one live session), `TestAudit_AColdStepUpIssuesAndAStrangersSessionIsNotRotated`;
`internal/httpapi` `TestAudit_TheCallbackHandsTheCurrentSessionToTheLoginService`.

## F-178 · The /me invariant was proved where the identifier was not · PRODUCTIZATION · P2 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_MeRoutesNameAnAccountInTheQueryStringWhereTheInvariantTestCannotSee`
and `TestAudit_TheMeAllowlistCommentIsWrongAboutVerificationSessions`.

`handlers_profile.go` claimed cross-tenant access on `/me` is unrepresentable:
"there is no identifier in the path, and the subject comes from the request
principal". The proof, `TestProfile_NoSelfServiceRouteTakesAnIdentifier`, walked
chi route PATTERNS looking for a `{` -- one of the three places an identifier can
be. Ten `/v1/me` operations name an account in the query string or the body.

One of them mattered. `GetMeVerificationSessionsSessionId` is a GET that CHANGES
STATE -- `Poll` asks the provider and records the answer -- and it resolved its
account with `accountScope`, the read-grade helper that `security.RequireAccount`
lets any holder of `account:read_any` satisfy for any account in the system. A
`SUPPORT_READ_ONLY` operator, the least-privileged staff role, could drive
another person's verification forward: the F-102 shape, on the one route the
`/me` proof could not see.

The second half is the allowlist comment beside it, which read "a session is
loaded under the caller's profile; another person's id is not found". The first
clause was true and the second was not, because the account the session was
loaded under came from the overridable helper.

**Fix.** `accountScopeWrite` on that handler, and `stateChangingGETs` in
`accountscope_test.go` is where a GET that writes has to say so. The invariant is
proved from the generated contract now:
`TestProfile_EveryMeOperationNamingAnAccountIsAccountedFor` walks
`api.StrictServerInterface`, requires every `/me` operation carrying an
`AccountId` in `Params` or `Body` to appear in `meOperationsNamingAnAccount` with
the helper that resolves it and why, refuses an entry for an operation that no
longer takes one, and scans the handler source to check that each entry's claimed
helper is the one the handler calls -- which is exactly what the wrong comment
was. The path walk stays, because a path parameter is a field on the request
struct rather than in `Params` or `Body`.

The five remaining reads keep `accountScope`, and that is a decision rather than
an oversight: `account:read_any` is the operator read override this system grants
on purpose, to a role a person was given in a directory with a reason attached,
the same facts are on the admin plane, and nothing behind those five moves.

**Evidence.** `internal/httpapi/handlers_verification.go`,
`handlers_profile_test.go` (`meOperationsNamingAnAccount`),
`accountscope_test.go` (`stateChangingGETs`);
`internal/httpapi` `TestAudit_MeRoutesNameAnAccountInTheQueryStringWhereTheInvariantTestCannotSee`,
`TestAudit_TheMeAllowlistCommentIsWrongAboutVerificationSessions`,
`TestProfile_EveryMeOperationNamingAnAccountIsAccountedFor`,
`TestAccountScope_TheWriteHelperIsActuallyUsed` (all inverted or extended).

## F-179 · Effecting a closure consulted nothing financial · PRODUCTIZATION · P2 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_EffectingAClosureChecksNothingFinancial` and
`TestAudit_TheClosureDecisionSurfaceCarriesNoFinancialFact`.

EFFECT is the one irreversible operator action on the support surface: it writes
`users.status = CLOSED`, closes every account the person owns and revokes every
session, after which `identity.Complete` refuses the login. Whatever the account
still holds is then unreachable by the person who owns it.

`profile.Decide` checked exactly three things: that a PENDING request exists,
that the operator is not its subject, and that the cooling-off period had passed.
It read no balance, no open payout request and no open position -- and neither did
`AdminUserView`, so the operator could not have consulted them either. Migration
00758 and `internal/profile/closure.go` both say REFUSED exists for "an unsettled
payout, an open dispute, or a balance to deal with first", and the operator was
given none of those three facts.

**Fix.** D-102. `Repository.ClosureBlockers` reads the three in ONE statement --
one, because three round trips could report a balance from before a payout
reserved against it -- across every account the person owns: the gross Credit
balance (the sum of remaining lot quantities, which is what `credit.Balances`
calls Gross and what `credit.VerifyProvenance` pins to the ledger), payout
requests not in a terminal state, and native positions holding a non-zero
quantity. `Decide(EFFECT)` refuses with `INVALID_STATE_TRANSITION` naming the
blocker, and `AdminUserView` carries the same read, so the surface the operator
decides from and the check that refuses the decision cannot disagree. The admin
console states all three above the form and stops offering EFFECT while any of
them stands.

It is not a veto on leaving: a blocker means REFUSE with a reason the person is
shown, and they ask again once it is dealt with.

**Evidence.** `internal/profile/closure.go` (`ClosureBlockers`), `repo.go`
(`closureBlockersSQL`), `account.go`; `openapi/openapi.yaml`
(`AdminUserView.closure_blockers`); `apps/admin/src/views/accounts.ts`;
`internal/profile` `TestAudit_EffectingAClosureChecksNothingFinancial`,
`TestAudit_EffectingAClosureIsRefusedWhileCreditsRemain`,
`TestAudit_EffectingAClosureIsRefusedWhileAPositionIsOpen`,
`TestClosureBlockers_TheTerminalPayoutStatesAreThePayoutPackages`,
`TestClosureBlockers_ClearIsTheAbsenceOfAllThree`; `internal/httpapi`
`TestAudit_TheClosureDecisionSurfaceCarriesNoFinancialFact` (inverted).

## F-180 · STAGING accepted an unbounded bootstrap operator declaration · PRODUCTIZATION · P3 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_StagingAcceptsAnUnboundedBootstrapDeclaration`.

`RuleBootstrapOperators` narrowed `CP_AUTH_BOOTSTRAP_OPERATORS` on
`env == EnvProd` alone, while every sibling auth rule in the same block --
`RuleNoDebugAuth`, `RuleCookieHostOnly`, `RuleCookieSecure` -- is `prodLike`.
STAGING is an internet-reachable deployment with the same cookie topology, and
the grant this variable writes is permanent: by ADR-0024 §3, removing the
declaration stops it being re-offered and revokes nothing. So a STAGING
deployment could stand up an unbounded standing staff directory in an
environment variable, which is the thing the PROD narrowing exists to prevent.

**Fix.** `prodLike`, and ADR-0024 §5 amended to say STAGING and PROD with the
reason. `HUMAN_ACTIONS_QUEUE.md` item 5 already declares exactly one ADMIN, so no
deployment blueprint changes.

**Evidence.** `internal/config/validate.go`; `internal/config`
`TestAudit_StagingAcceptsAnUnboundedBootstrapDeclaration` (inverted, and with the
control it must not break: STAGING still accepts one ADMIN);
`docs/adr/0024-how-a-principal-becomes-an-operator.md` §5.

## F-181 · CUSTOMER was a nameable operator-directory role · PRODUCTIZATION · P3 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_TheOperatorDirectoryMayNameCUSTOMER`.

`operatorroles.Directory()` was derived as "every declared role except
BREAK_GLASS", and migration 00760's CHECK held the same list. Both admitted
`CUSTOMER`, which is not an operator role: it is what `internal/identity` gives a
principal the directory says nothing about.

`identity.Complete` decides the actor type from WHETHER the directory returned
anything, not from what it returned. So a `CUSTOMER` row produced a session whose
`ActorType` is OPERATOR carrying only customer permissions -- and that person's
own terms acceptance was then written with `actor_type = 'OPERATOR'`, which
migration 00759 documents as meaning "an acceptance recorded on somebody's behalf
by an operator". The record of their consent said somebody else gave it, and
their own audit events said the same.

**Fix.** Migration 00800 drops it from the CHECK; `Directory()` excludes it
beside BREAK_GLASS and the package doc says the two exclusions are different
facts; `ParseDeclarations` refuses it by name with the reason rather than
reporting an unknown role, which would read as a typo. No row can exist to
migrate: nothing but the bootstrap declaration writes this table.

**Evidence.** `migrations/00800_the_operator_directory_does_not_name_a_customer.sql`,
`internal/operatorroles/operatorroles.go`; `internal/config`
`TestAudit_TheOperatorDirectoryMayNameCUSTOMER` (inverted);
`internal/operatorroles` `TestDirectory_IsEveryRoleExceptBreakGlassAndCustomer`,
`TestParseDeclarations_RefusesCustomer`; `test/integration/enums`
(`operator_roles_role_check` against `operatorroles.Directory()`);
ADR-0024 §4.

## F-182 · The login-state refusal described an attack when what happened was two tabs · PRODUCTIZATION · P3 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), reproduced by
`TestAudit_ASecondLoginInvalidatesTheFirstTabsFlow`.

`httpmw.SetLoginState` writes one cookie name at `Path=/`, so a second
`GET /v1/auth/login` overwrites the first flow's digest. Two sign-in tabs, or a
sign-in started and then a step-up begun beside it, leave the older tab's
callback answering `401 UNAUTHENTICATED: this sign-in did not start in this
browser; start again from the beginning`.

**Decision D-103: keep one slot and change the copy.** One slot is F-87's
control and it is what makes a planted callback unusable; a per-flow cookie would
be a larger change to the thing that is working. The message was the defect: it
named the attack as the only explanation when a second tab is the common case by
a long way, and a person who reads it has been told, wrongly, that something
attacked them.

**Fix.** `this sign-in did not start in this browser, or a newer sign-in
replaced it; start again`.

**Evidence.** `internal/httpapi/handlers_auth.go`; `internal/httpapi`
`TestAudit_ASecondLoginInvalidatesTheFirstTabsFlow` (the control's assertion is
unchanged -- the callback is still refused -- and the expectation moved to the
copy).

## F-183 · The privacy notice said security events are removed on a schedule · PRODUCTIZATION · P3 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54).

The notice's retention paragraph read "Transient records -- sessions, login
attempts, security events -- are removed on a schedule."
`CP_RETENTION_SECURITY_EVENT_DAYS` is 0 by default, which `cmd/api`'s retention
pass reads as pruning being off, and nothing else purges `security_events`. Two
of the three were true.

**Fix.** The paragraph says which is which, and says that no retention period has
been chosen for security events rather than implying one exists. The document
version is bumped to `2026-09-10.2`, which changes every document's content hash
and re-asks every user to accept. That is the mechanism working rather than a
cost of using it: a notice that describes the system differently is a different
notice, and acceptance is recorded against the bytes shown.

**Evidence.** `internal/terms/documents/privacy_policy.md`,
`internal/terms/terms.go` (`Version`); `internal/terms`
`TestRegistry_EveryDeclaredDocumentLoads`,
`TestRegistry_ContentHashesAreDistinctAndLineEndingIndependent`.

## F-184 · A test asset symbol collided about one run in fifty · PRODUCTIZATION · P3 · FIXED

**Found by** the accounts-auth adversarial audit (goal §54), incidental to it.

`internal/httpapi/domaina_admin_integration_test.go` built a native asset symbol
from four hex characters of a UUIDv7 tail. Four hex characters is 65,536 values,
and the package creates enough assets per run for the birthday bound to put a
`SYMBOL_TAKEN` refusal at roughly one run in fifty. A flake at that rate is a
test suite people learn to re-run.

**Fix.** `uniqueSuffix` keeps the random tail and appends a per-run counter, so
the answer is unique within a run rather than merely improbable.

**Evidence.** `internal/httpapi/domaina_admin_integration_test.go`
(`uniqueSuffix`); `internal/httpapi` `TestIntegration_EveryStoppingControlIsOneOperator`,
`TestIntegration_ResumingAMarketTakesTwoPeople`.
## F-136 · The blueprint handed the internet-facing API the schema-owner DSN, under a name the test that forbids it did not know · PRODUCTIZATION · P1 · FIXED

**Found by** the config-deploy audit (goal §54), reading `render.yaml` against
`test/infra/render_test.go` rather than reading either alone.

`CP_DATABASE_MIGRATE_URL` is deliberately absent from `nodal-api`, F-93 says
why, a twenty-line comment in `render.yaml` says why, and
`TestTheWebServiceIsNotGivenTheSchemaOwner` asserts the absence. And two lines
above that comment the same file declared `NODAL_DB_MIGRATE_URL` as a prompted
secret on the same service, so Render put the `cp_migrate` DSN — password
included — into the API container's environment.

`cp_migrate` owns every table, and an owner can `ALTER TABLE ... DISABLE
TRIGGER`. Since 00743–00753 the state machines are enforced by triggers: the
transition bindings, `forbid_mutation` on fifty-three append-only tables, and
the eleven that write state columns the application cannot. So a compromise of
the web process would have undone all of them in one statement.

Nothing read it. A repository-wide grep found the name in `render.yaml` and in
two test stand-ins (`test/infra/render_test.go`,
`test/deployed/surface_test.go`) and nowhere else: `cmd/migrate` takes
`CP_DATABASE_MIGRATE_URL` from its own environment when an operator runs it.
The credential was in the internet-facing process for no purpose at all.

The test failed because a name test is only as good as the names it knows.

**Fix.** The entry and both stand-ins are gone.
`TestTheWebServiceIsNotGivenTheSchemaOwner` asks about the ROLE instead of a
name: no variable on that service may carry `MIGRATE` in its key, or
`cp_migrate` / `MIGRATE_URL` in its value. `HUMAN_ACTIONS_QUEUE.md` item 5
tells the operator to delete the dashboard value by hand, because a blueprint
sync does not remove one, and `docs/operations/LAUNCH_TIER.md` §10 — which did
not exist — states how migrations are actually run on this tier: an operator,
from their own machine, against Neon, with the DSN in that shell and nowhere
else.

**Evidence.** STATIC_PROOF: `grep -rn NODAL_DB_MIGRATE_URL` — nothing outside
the audit file's own prose. TEST_UNIT:
`TestTheWebServiceIsNotGivenTheSchemaOwner`,
`TestAuditConfigDeploy_TheAPIServiceIsNotHandedTheSchemaOwnerDSN`.

## F-137 · STAGING booted with no alert destination and no PII keyring, and three documents said it could not · PRODUCTIZATION · P1 · FIXED

**Found by** the config-deploy audit, loading `render.yaml`'s own variables
through `config.Load` with the two dashboard secrets absent — the state an
operator who had not yet done items 1 and 2 of the human actions queue would
be in.

`render.yaml` said: *"config.Validate refuses to start in STAGING or PROD when
it is empty, so a deployment that forgets it does not boot silently unalerted —
it does not boot."* `HUMAN_ACTIONS_QUEUE.md` items 1 and 2 repeated it.
`MASTER_BUILD_STATE.md` said the deploy *"refuses to start without them, by
design."*

It booted. `RuleAlertDestination` and `RulePIIKeyring` checked that the
`SecretRef` was non-empty, and the blueprint sets `CP_ALERT_WEBHOOK_URL` to the
literal string `env://NODAL_ALERT_WEBHOOK_URL`, which is never empty. The value
behind the reference was read only at composition time, where
`cmd/api/alerts.go` and `cmd/api/pii.go` logged at ERROR and returned nil —
deliberately, so an unresolvable secret would not cause an outage.

Each decision is defensible and together they meant a healthy, serving STAGING
in which a ledger-integrity violation reached nobody (the F-118 state) and no
verified e-mail was ever stored (the F-47 state), with a 200 on every probe.

**Fix.** `Config.ResolvableSecrets` resolves both references during `Load`,
where the lookup is still in hand; an unresolvable or empty one is a violation
of the rule that was supposed to have caught it. A scheme the loader's chain
cannot resolve at all — `aws-sm://`, whose resolver lives in the running
process — is not a violation, because that is the AWS architecture's normal
shape. `newAlertDispatcher` and `newPIIStore` return an error in STAGING and
PROD instead of nil: the second refusal, for the resolver the process holds and
the loader does not, and for a keyring that resolves and does not parse. LOCAL,
TEST and DEV keep the honest degradation and the log line naming the
consequence. The three documents now describe what happens.

**Evidence.** TEST_UNIT:
`TestValidate_TheAlertDestinationAndTheKeyringAreRESOLVED` (absent, whitespace,
both environments, and `aws-sm://` still accepted);
`TestADeploymentWithNowhereToAlertRefusesToStart`;
`TestADeploymentWithoutAUsableKeyringRefusesToStart`;
`TestRender_TheBlueprintActuallyStartsTheAPI` — which now resolves the
references and asserts the other direction, that removing either value stops
the blueprint loading;
`TestAuditConfigDeploy_STAGINGRefusesToBootWithoutTheAlertDestinationAndKeyring`.

## F-138 · The blueprint's secret scan read one service of two, and nothing pinned the static site · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, twice over in the same file.

`TestRender_NoSecretIsWrittenIntoTheBlueprint` opens with *"render.yaml is
committed. Anything with a literal value in it is public to everyone who can
read the repository"* — a claim about the FILE — and then scanned
`loadBlueprint(t).Services[0]`. The static site's `envVars` were never looked
at, so a live Stripe secret key written there was committed and green.
Reproduced by injecting `sk_live_…` into that block and running the test: PASS.
A build-time variable on a static site is embedded in the bundle and served to
every visitor, so this is the worst of the two places to put one.

And `renderBlueprint` stopped at `envVars`: `headers`, `routes`, `domains`,
`buildCommand` and `staticPublishPath` were in no test at all. The CSP, HSTS,
`X-Frame-Options`, `Referrer-Policy`, `Permissions-Policy`,
`X-Content-Type-Options`, the SPA rewrite and the custom domain could all be
deleted with `go test ./test/infra/` still green — confirmed by deleting them.
`test/deployed/surface_test.go` pins headers, but only the API's, only behind
the `deployed` build tag, and only against a live target.

**Fix.** The scan is `credentialMarkersIn`, a shared function over every
service, and the early `continue` on a known-secret key is gone — a name being
on the secret list is a reason to require an `env://` reference, not a reason
to stop looking for key material under it. `renderBlueprint` carries the whole
service. Five new tests pin the site: its build command and publish path, the
single SPA rewrite, both services' hostnames against
`CP_HTTP_PUBLIC_BASE_URL`/`VITE_API_ORIGIN`/`CP_HTTP_CORS_ORIGINS`, the six
headers by VALUE rather than by presence, and the policy directive by
directive.

**Evidence.** TEST_UNIT:
`TestRender_NoSecretIsWrittenIntoTheBlueprint`,
`TestRender_TheStaticSiteIsBuiltAndPublishedFromThisRepository`,
`TestRender_TheSPARewriteExists`,
`TestRender_EveryServiceDeclaresItsOwnHostname`,
`TestRender_TheStaticSitesSecurityHeadersAreWhatTheAppNeeds`,
`TestAuditConfigDeploy_TheSecretScanCoversEveryServiceInTheBlueprint` (which
runs the real scan over an injected document rather than a copy of the scan),
`TestAuditConfigDeploy_TheStaticSitesSecurityHeadersArePinnedSomewhere`.

## F-139 · `style-src 'self'` blocked the inline style attributes the app ships · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, reading the deployed policy against the
bundle it governs.

CSP Level 3 governs a `style=` ATTRIBUTE with `style-src-attr`, which falls
back to `style-src` when it is absent. The policy declared `style-src 'self'`
and no `style-src-attr`, so every inline style attribute in the bundle was
refused by the browser.

The app ships four. `Skeleton.tsx` sets a loading placeholder's width;
`CandleChart.tsx` two; and `SegmentedBar.tsx` sets `flexGrow` from the exact
base-unit string of each segment — and `SegmentedBar` is the
available/reserved/pending balance bar its own file header calls *"the central
information-design problem on Home"*. With the attribute blocked every segment
keeps `flex-grow: 0` and the bar renders empty, while the element's
`aria-label` still reads "Available now, about 62%; …". A figure stated to a
screen reader and not shown to an eye is the honesty rule inverted.

**Fix (D-116).** `style-src 'self' 'unsafe-inline'`, and the components are
left alone. The precise form — `style-src 'self'; style-src-attr
'unsafe-inline'` — is not supported by every browser the app targets, and a
browser that ignores `style-src-attr` falls back to `style-src` and blocks the
attribute again. The relaxation is bounded and the bound is asserted rather
than described: `script-src` stays without `'unsafe-inline'`, this bundle
renders no user-controlled HTML, and `object-src 'none'`, `base-uri 'self'` and
`frame-ancestors 'none'` are unchanged, so what an injected style could reach
is the appearance of a page an attacker cannot get script into.

**Evidence.** TEST_UNIT:
`TestRender_TheStaticSitesSecurityHeadersAreWhatTheAppNeeds` (the directive and
its bound);
`TestAuditConfigDeploy_TheCSPPermitsTheInlineStylesTheAppShips`, which walks
`apps/web/src` for `style={{` and fails if the policy refuses what it finds.

## F-140 · The policy's own directives, and the Stripe key's account, were asserted nowhere · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, as the half of F-138 that is not about
the scan's reach.

Two specific gaps behind the general one. `connect-src` names the only origins
the app may talk to — an extra entry there is an exfiltration destination and a
missing one is a dead page — and nothing held it to a list. And the app is
given `VITE_STRIPE_PUBLISHABLE_KEY` while the API is given
`CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF`; the adapter makes a read-only call
at startup and refuses to sell Credits if its SECRET key answers for a
different account, and nothing compared the key the BROWSER is given against
the same value. A publishable key embeds its account id, so the comparison is
free and was not being made.

**Fix.** `connect-src` is held to exactly `'self'` + the API origin +
`https://api.stripe.com`, `script-src` to exactly `'self'` +
`https://js.stripe.com`, and `frame-ancestors`, `object-src`, `base-uri` and
`default-src` each to their single source. The publishable key must embed the
account id the API names, and its mode must match the provider's — a
`pk_live_` key against a `sandbox` provider fails here rather than at
confirmation.

**Evidence.** TEST_UNIT:
`TestRender_TheStaticSitesSecurityHeadersAreWhatTheAppNeeds`,
`TestRender_TheStripeKeyAndTheStripeAccountAreTheSameAccount`.

## F-141 · `nodal-api` declared no custom domain while every other value assumed one · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, asking what a deployment created from
this file alone would actually answer on.

The static site declared `domains: [app-nodal.actorvia.xyz]`. `nodal-api`
declared none — while `CP_HTTP_PUBLIC_BASE_URL`, `CP_AUTH_REDIRECT_URL`,
`CP_AUTH_POST_LOGIN_URL`, `CP_HTTP_CORS_ORIGINS`, the app's `VITE_API_ORIGIN`
and the static site's CSP `connect-src` and `form-action` all name
`api-nodal.actorvia.xyz`. A blueprint sync would have given it an
`*.onrender.com` hostname: the OIDC `redirect_uri` would not match, the cookie
would be set on a host the app does not call, and every request would be a CORS
failure.

The live service has the domain attached by hand, which is exactly the drift
the blueprint exists to remove, and `HUMAN_ACTIONS_QUEUE.md` item 4 covers the
app's CNAME while explicitly saying *"Do not touch api-nodal"* — so re-creating
the deployment from the file plus the queue reproduced none of it.

**Fix.** `domains: [api-nodal.actorvia.xyz]`, and a test that derives the host
from `CP_HTTP_PUBLIC_BASE_URL` rather than repeating it, checks the two
services do not claim the same name, and checks the app is built to call the
API's host and that the API allows the app's origin.

**Evidence.** TEST_UNIT: `TestRender_EveryServiceDeclaresItsOwnHostname`,
`TestAuditConfigDeploy_TheAPIServiceDeclaresItsOwnHostname`.

## F-142 · `/v1/version` reported `build_version: dev` for every image ever built · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, following `CMD` backwards from
`render.yaml` into `build/Dockerfile`.

`build/Dockerfile` stamps `config.BuildVersion` from the `VERSION` build arg
and defaulted it to `dev`. Render passes a service's environment variables to
the docker build as build args — which is the sole reason `CMD` is in
`render.yaml` at all — and no `VERSION` was declared. So every image ever built
from this blueprint reported `build_version: dev`, and because `BuildVersion`
is part of `Config.Hash`, the configuration hash could not distinguish two
different builds of the same configuration either.

The endpoint whose whole purpose is to prove which build and which
configuration are running could prove neither, and
`HUMAN_ACTIONS_QUEUE.md` item 3 promised that the push would unblock exactly
that comparison.

**Fix (D-117).** Render sets `RENDER_GIT_COMMIT` itself, at build time and at
run time. `ARG RENDER_GIT_COMMIT=` and `ARG VERSION=${RENDER_GIT_COMMIT:-dev}`
make the link-time version the commit Render is building and `dev` everywhere
else, with nothing new in the configuration table and a local `docker build`
unchanged; `scripts/images` still passes `--build-arg VERSION=<sha>`, which
wins over the default. Both stages, because an ARG declared inside a stage is
scoped to it and an OCI version label saying `dev` over a binary saying the
commit is the same defect wearing a smaller hat.

**Evidence.** TEST_UNIT: `TestAuditConfigDeploy_TheBuildStampsAVersion` — the
constant default is gone, both stages derive `VERSION` from
`RENDER_GIT_COMMIT`, and `CMD` is still passed, because the default relies on
the same mechanism and its loss would be a failed build rather than a silent
`dev`.

## F-143 · The live event stream carried no session on the deployed topology · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, comparing the app's two origins against
every request the bundle makes.

`EventSource`'s credentials mode is `"same-origin"` unless `withCredentials` is
set. The deployed app is `app-nodal.actorvia.xyz` and the API is
`api-nodal.actorvia.xyz`: the same SITE, a different ORIGIN. `StreamStatus.tsx`
constructed `new EventSource(`${API_BASE}/events/stream`)` with no options, so
the session cookie was not attached and `GetEventsStream` — which requires
`PermAccountRead` — refused it. The bell, the activity feed and every
stream-driven invalidation were dead on the deployed tier, with the badge
saying "reconnecting" forever.

Everything else in the app was correct, which is why this was the one request
that broke: the generated client sets `credentials: "include"` and `probeReady`
deliberately omits them. The browser suite could not catch it either —
`vite.config.ts` proxies `/v1`, so those tests run same-origin, where the
default already sends the cookie.

**Fix.** `new EventSource(url, { withCredentials: true })`, and a source rule
in `apps/web/src/lib/source-scan.test.ts`, which is where this app keeps the
rules that have to hold for code nobody has written yet. It is stated
unconditionally rather than only when `VITE_API_ORIGIN` differs from the page,
because the option is correct either way: a same-origin request sends the
cookie with or without it.

**Evidence.** TEST_UNIT (node --test): *"every EventSource is constructed with
withCredentials"*, which also asserts the app opens one at all so the rule
cannot pass by vacuum. `TestAuditConfigDeploy_TheEventStreamCarriesTheSessionCrossOrigin`
holds the same thing from the Go side, against the blueprint's two hostnames.

## F-144 · `CP_SEED_ENABLED` was read by nothing, and the sandbox tier seeded regardless · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, grepping for readers of a variable the
blueprint sets and finding none.

`internal/config` declared `CP_SEED_ENABLED` required of every binary,
documented it as *"Allow seeding clearly-labeled fake users/assets/balances.
Must be false in STAGING/PROD"*, and `RuleNoSeed` enforced that. `render.yaml`
set it to `"false"`. Then `cmd/api/marketsurfaces.go` seeded the sandbox demo
catalogue on every boot, keyed on `cfg.SandboxTier()` alone — its own docstring
said *"when the deployment is a sandbox tier AND ASKS FOR IT"*, and there was
no asking. `c.Seed.Enabled` was read by `internal/config`'s own validator and
by nothing else in `cmd/` or `internal/`.

So the deployed STAGING declared that it does not seed and seeded: eight demo
markets, a demo Credit balance, and an activity feed built out of them. Nothing
real was at risk — the rows are SANDBOX-labelled and migration 00774 forbids a
PROD one — but a stated control was not in force, and the one variable an
operator would reach for to turn seeding off did nothing.

**Fix (D-115).** The two cannot be the same variable: `RuleNoSeed` forbids
`CP_SEED_ENABLED` in STAGING and PROD, so it could never have been the deployed
tier's control. `CP_API_DEMO_DATA` (bool, default false) is the demo
catalogue's switch — `demoDataAtBoot` runs on `cfg.SandboxTier() &&
cfg.API.DemoData`, config refuses `true` on any deployment that is not a
sandbox tier, and `render.yaml` sets it `true` on STAGING and says why.
`CP_SEED_ENABLED` keeps what it always claimed: `config.SeedScriptsAllowed` is
its reader and `scripts/seed` and `scripts/seedeconomy` call it. `scripts/demodata`
is deliberately not gated on it, because it loads the SANDBOX catalogue a
sandbox tier is allowed to hold.

**Evidence.** TEST_UNIT: `TestSeedScriptsAllowed` (absent means yes inside
LOCAL/DEV/TEST so `make seed` and CI keep working; `false` means no; a deployed
environment is no whatever it says; an unparseable value is an error, never a
default); `TestVars_DocumentedInDocGo` and the requirements-table count;
`TestAuditConfigDeploy_SeedingHonoursTheVariableThatForbidsIt`, which walks for
readers of both switches.

## F-145 · A misspelled capability loaded clean and was dropped in silence · PRODUCTIZATION · P3 · FIXED

**Found by** the config-deploy audit, reading `config.Config`'s own comment
against `Validate`.

The comment on `SandboxGates` says *"each must be a declared capability and
must also be in EnabledCapabilities"*. `Validate` checked the second half only:
it compared the sandbox list against the enabled list and never asked
`internal/gates` whether either name exists. `cmd/api` then did two different
things with the same mistake — `parseCapabilities` skipped an invalid name
without a word, and `sandboxGatesAtBoot` returned an error and refused to
start.

The first is the one that matters. `CP_API_ENABLED_CAPABILITIES` is condition 1
of the policy authority, so a typo silently makes a capability inactive
whatever its gate row says, with a healthy service and a 200 on every health
check. `CREDIT_PURCHASEE` is one letter and the whole Credit purchase path.
That is the defect class F-124 is in the register for.

**Fix.** Three refusals at descending distance from the damage:
`RuleCapabilityNameDeclared` in `config.Validate`, on both lists, where an
operator sees it before a deploy; `parseCapabilities` returns an error, for a
Config that did not come through `Load`; and `test/infra` checks every name the
blueprint states.

`internal/config` cannot import `internal/gates` — `internal/gates`' own
integration test imports `internal/agent`, which imports `internal/config`, so
the edge compiles and then `go vet -tags integration ./...` reports a cycle in
a test binary. The names are restated in `internal/config/capability.go` and
held equal to `gates.AllCapabilities()` by a test in `test/infra`, which may
import both. A duplicated list nobody greps is the defect F-130 records; this
one fails a test rather than rotting.

**Evidence.** TEST_UNIT:
`TestConfigTable_TheCapabilityNamesAreTheOnesGatesDeclares`,
`TestRender_EveryCapabilityNamedInTheBlueprintExists`, `TestParseCapabilities`,
`TestAuditConfigDeploy_AMisspelledCapabilityIsRefusedByTheConfiguration`.

## F-146 · `return_to` of the form `/\host` was an open redirect off the callback that sets the session cookie · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, reading the `return_to` guard against the
WHATWG URL standard.

`internal/identity/login.go` validated with one string comparison: refuse
unless it starts with `/`, and refuse `//`. `"/\evil.example"` walks through
it. `postLoginDestination` then returned the caller's own string verbatim when
`CP_AUTH_POST_LOGIN_URL` was empty, so `GetAuthCallback` answered `Location:
/\evil.example` in the same response that sets the session cookie. Every
browser implementing the standard resolves a special-scheme relative reference
beginning `"/\"` through the relative-slash state into the AUTHORITY state —
`\` is a slash for http(s) — so the browser goes to `https://evil.example/`.
That is the standard backslash bypass of a `//` check.

It was not reachable on the shipped launch tier, which sets
`CP_AUTH_POST_LOGIN_URL` and therefore pins the host. It was reachable on any
deployment that left the variable unset, and nothing prevented that: it was
`opt(...)` and no rule required it in STAGING or PROD.

**Fix.** Three changes, so that no single one is load-bearing.
`identity.IsLocalPath` refuses a second character of `/` or `\`, any control
character — the tab, newline and carriage return that URL parsers STRIP before
parsing, and the NUL that truncates a C string — and anything `url.Parse` reads
as carrying a scheme, host or opaque part; `Begin` uses it, so the row is never
written. `postLoginDestination` applies the same test at the point of USE and
falls back to `/`, including when a base is configured, because a redirect
built from a stored value must not depend on the writer of that value having
been careful. And `RuleOIDCConfigured` requires `CP_AUTH_POST_LOGIN_URL` in
STAGING and PROD.

The comment that said *"Neither input is the user's, so this is not an open
redirect: the base is configuration and the path is what the login service
recorded"* was half true and therefore wrong, and is replaced with what is
actually the case.

**Evidence.** TEST_UNIT:
`TestAuditConfigDeploy_PostLoginDestinationCannotLeaveTheOrigin`;
`TestPostLoginDestination_HonoursARealPathAndRefusesEverythingElse` — fourteen
refused forms and six honoured paths, because a guard that also refuses the
deep link a customer was reading has broken the feature `return_to` exists for;
`TestValidate_PostLoginURL`.

## F-147 · `render.yaml` pointed operators at a document that does not exist · PRODUCTIZATION · P3 · FIXED

**Found by** the config-deploy audit, following the file's own first paragraph.

Line 4 read *"see docs/operations/RENDER_DEPLOY.md for what an operator does
with it"*. There is no such file and there never has been.

**Fix.** It points at `docs/operations/LAUNCH_TIER.md`, which is the document
that exists, and at `docs/build/HUMAN_ACTIONS_QUEUE.md` for the actions a
person still has to take.

**Evidence.** STATIC_PROOF: `ls docs/operations/` — ten files —
`BACKUP_RESTORE`, `DEPLOYMENT`, `DEPLOYMENT_GAP_ANALYSIS`,
`DISASTER_RECOVERY`, `DNS_RECORDS`, `IDENTITY_PROVIDER`, `LAUNCH_TIER`,
`PATH_B_STATUS`, `PROVIDER_ACTIVATION_CHECKPOINT`, `RECONCILIATION` — and no
`RENDER_DEPLOY.md`. The reference is prose in a YAML comment, which is why
no test caught it: `test/docs` walks markdown.

## F-148 · The static site published its source maps · PRODUCTIZATION · P3 · FIXED

**Found by** the config-deploy audit, reading `apps/web/vite.config.ts` against
what the CDN serves.

`sourcemap: true` emits a `//# sourceMappingURL=` comment beside the bundle, so
a browser fetches the map and the CDN serves the entire readable frontend —
comments included — to anyone who opens dev tools. 2.6 MB of it, beside a
630 kB bundle.

**Fix.** `sourcemap: "hidden"`. The maps are still WRITTEN, so a production
stack trace can be symbolicated by whoever holds `dist/`; no comment is
emitted, so nothing fetches them. Confirmed on a real build: the `.map` file
exists and `grep -c sourceMappingURL` on the bundle is 0.

**Evidence.** STATIC_PROOF: `apps/web/vite.config.ts`; the build output above.

## F-149 · `CP_AUTH_POST_LOGIN_URL` defaulted to one deployment's hostname · PRODUCTIZATION · P3 · FIXED

**Found by** the config-deploy audit, alongside F-146.

The variable's LOCAL/TEST default was `https://app-nodal.actorvia.xyz/`. A
default is what a developer gets when they say nothing, and this one sends
their browser to the internet after a local sign-in. It also read as the
deployment's value being covered when it is not: defaults are applied only
where `Environment.AllowsDefaults()`, which is LOCAL and TEST, so the variable
this file names was simultaneously defaulted to a production hostname and
unrequired in production.

It was not only untidy. The CI `web-e2e` job starts the API under test with
`CP_ENV=LOCAL` and sets no `CP_AUTH_POST_LOGIN_URL`, and LOCAL is an
environment where defaults apply — so `apps/web/e2e/auth.setup.ts`, which
drives the real `GET /v1/auth/login` and follows the callback, was being sent
to `https://app-nodal.actorvia.xyz/` by a suite whose whole premise is that it
runs same-origin behind the Vite proxy. The variable named in the finding is
the one that sends it there.

**Fix.** The default is `""`, which is what a same-origin run actually wants —
a relative redirect that resolves to whatever origin the browser is on.
`render.yaml` and `.env.example` keep the hostname, where it is a deployment's
statement rather than a fallback, and F-146's rule requires it in STAGING and
PROD. The rule is inside the `CP_AUTH_MODE=oidc` branch, so the dev-auth E2E
recipes are not asked for one.

**Evidence.** TEST_UNIT: `TestValidate_PostLoginURL`, which asserts the default
is empty as well as the rule, and that LOCAL, TEST and DEV still accept an
empty value. STATIC_PROOF: `.github/workflows/ci.yml` (the `web-e2e` job's
`CP_ENV: LOCAL` and no post-login variable); `Environment.AllowsDefaults()`.

## F-150 · `Permissions-Policy: payment=()` took the wallets out of Stripe's Payment Element · PRODUCTIZATION · P2 · FIXED

**Found by** the config-deploy audit, reading the header against the one page
this tier exists to exercise.

`payment=()` denies the Payment Request API to every frame, including the
`js.stripe.com` iframe the Payment Element runs in. Apple Pay, Google Pay and
Link are offered through that API, so the wallet rows disappear from Buy
Credits — the deny-list was written as "deny everything we do not use" and this
one is used, by a third party, inside an iframe.

**Fix.** `payment=(self "https://js.stripe.com")`. `self` stays because the
allowlist is what the iframe inherits its permission through; everything else
in the header is unchanged and still denied.

**Evidence.** TEST_UNIT:
`TestRender_TheStaticSitesSecurityHeadersAreWhatTheAppNeeds`, which asserts the
five denials and this one delegation.
## F-160 · A gate sandbox-activated out of REVOKED reported success, logged it, and could never be active · PRODUCTIZATION · P2 · FIXED

**Found by** the independent governance / sandbox-tier audit of ADR-0023 (goal
§54), reproduced on `productization` by
`TestAUDIT_ASandboxGateEnteredFromRevokedCanNeverBeActive`,
`TestAUDIT_ASandboxTierCanRecoverAGateItRevoked` and
`TestIntegration_AUDIT_SandboxFromRevokedKeepsTheRevoke`.

ADR-0023 and migration 00755 both name REVOKED as a legal source of SANDBOX,
`cp_gate_can_transition` and `gates.CanTransition` agree, and `cp_gate_sandbox`
accepted the move. Its UPDATE wrote `state`, `effective_at`, `expires_at` and
`version` — so `revoked_at` survived, and `gates.evaluateSandbox` refused a row
with a revoke recorded before it looked at anything else. `Admin.Sandbox`
returned success, the console showed SANDBOX, `sandboxGatesAtBoot` logged
"capability sandbox-activated at boot", and the capability stayed refused with
`gate is revoked` for ever.

There was no way back either. The only operation that clears `revoked_at` is
`propose` (00701), and PENDING_APPROVAL leads only to APPROVED — which needs a
second distinct principal — or back to REVOKED. So a sandbox tier that revoked a
capability could not sandbox-activate it again without running part of the
dual-control ceremony the sandbox tier exists so a rehearsal need not fabricate.

**Fix.** Migration 00791 clears `revoked_at` and `revoke_reason` with the rest of
the row when a gate enters SANDBOX, and backfills the rows the old function
wrote. `evaluateSandbox` no longer consults the column: a revoke is a STATE, and
revoking a gate moves it to REVOKED, where the state check refuses it — the
column on a SANDBOX row is residue of an earlier life. Commit 0f4bfa7.

**Evidence.** TEST_INTEGRATION: `TestIntegration_SandboxGate` now starts from
REVOKED and asserts `RevokedAt == nil` and an active sandbox verdict;
`TestIntegration_AUDIT_SandboxFromRevokedKeepsTheRevoke` passes through
`BootstrapSandbox`, the path `cmd/api` runs at boot. TEST_UNIT:
`TestEvaluateWith_SandboxRowIsActiveOnlyOnASandboxTier` holds both halves — a
SANDBOX row with a stale revoke is active, a REVOKED row is not, whatever the
tier. STATIC_PROOF:
`migrations/00791_a_sandbox_row_carries_no_approval_and_no_revoke.sql`.

## F-161 · A SANDBOX gate reached from EXPIRED or REVOKED carried the whole approval version that reached ACTIVE · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced by
`TestIntegration_AUDIT_SandboxFromExpiredCarriesTheApprovalItClaimsNotToHave`.

Migration 00755's header, ADR-0023 §1, `internal/gates`' package doc and the
operator console all state that a SANDBOX gate carries no approval chain and no
evidence; the console renders "none, and none is expected". That was true only
of a gate sandboxed from a freshly bootstrapped DISABLED row, which has nothing
to carry. `cp_gate_sandbox` deliberately did not touch those columns, so a gate
sandboxed from EXPIRED or REVOKED — both documented sources — kept the entire
approval version that reached ACTIVE: three chain entries, the proposer, the
approval version and all four evidence references. `GET /v1/admin/gates`
returned every one of them beside `state: SANDBOX, active: true`, and the
sandbox transition and its audit event attested the evidence digest of that
approval, which nobody produced for the sandbox activation.

**Fix (D-090).** The same migration writes a blank row: approval version 0,
`approvers` and `evidence_hashes` `'[]'`, proposer and the four `*_ref` columns
NULL, in the same statement as the state change. "Never touches the approval
chain" becomes "clears the approval chain", which is the property every reader
was promised, and the cleared version survives in `capability_gate_transitions`
with the digest each principal attested. `internal/gates/sandbox.go` and
`sandbox_bootstrap.go` pass the digest of a gate with no evidence as the sandbox
transition's hash, so the transition, the audit event and the resulting row all
say the same thing. The console's one stale sentence follows. Commit 0f4bfa7.

**Evidence.** TEST_INTEGRATION:
`TestIntegration_SandboxFromAFinishedCeremonyCarriesNoneOfIt` drives a real
three-principal ceremony to ACTIVE, expires it, sandboxes it and asserts the row
and its transition carry nothing;
`TestIntegration_AUDIT_SandboxFromExpiredCarriesTheApprovalItClaimsNotToHave` is
the auditor's reproduction. STATIC_PROOF: `apps/admin/src/views/gates.ts`.

## F-162 · Rehearsing the real ceremony on a sandbox-listed capability stopped the deployment booting · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced by
`TestIntegration_AUDIT_ARealCeremonyOnASandboxCapabilityBlocksTheNextBoot`.

`BootstrapSandbox` refused to move a gate in PENDING_APPROVAL, APPROVED, ACTIVE
or SUSPENDED — correctly, because a configuration line must not change the state
of a real approval. But the refusal was an error, `cmd/api`'s wire step returned
it, and the process exited. So an operator who rehearsed step one of the
dual-control ceremony — on the tier that exists for rehearsing it — for any
capability named in `CP_API_SANDBOX_GATES` stopped the deployment restarting
until somebody edited the blueprint. The only in-machine escape, a revoke, landed
on F-160's permanently inert gate.

**Fix (D-091).** The gate is still never moved; the outcome is now a skip.
`gates.SandboxAtBoot` returns the skipped capabilities alongside the moved ones
and `cmd/api` logs a WARN naming the capability, its state and what to do about
it, then carries on with the rest of the list. Boot still refuses when
`CP_API_SANDBOX_GATES` is set on a deployment that is not a sandbox tier, and the
manual path still refuses the transition as illegal. Commit 7666e28.

**Evidence.** TEST_INTEGRATION:
`TestIntegration_BootstrapSandboxIsIdempotentAndNeverMovesAnApproval` asserts the
skip is reported, writes no audit event, leaves the proposal untouched, still
activates the rest of the list, and that the operator-driven move is still
refused with INVALID_STATE_TRANSITION.

## F-163 · No kill switch reached the conversion-request path · PRODUCTIZATION · P1 · FIXED

**Found by** the same audit, reproduced by
`TestAUDIT_NoKillSwitchCanReachTheConversionRequestPath`.

POLICY_AUTHORITY §2 declares the blocking matrix as a property of every guarded
operation and puts the `WITHDRAW` class under `WITHDRAWALS_DISABLE`,
`ACCOUNT_FREEZE` and `GLOBAL_NEW_RISK_KILL`. `internal/payout` imported
`internal/killswitch` nowhere. Nothing on `POST /v1/payouts` →
`httpapi.payoutsAdapter.Create` → `compileRoute` → `payout.Service.Create`
consulted a switch, and neither did the reservation or the submission to the
provider — so an operator could activate all three controls and a conversion
request would still be created, still move the user's Credits into
PAYOUT_RESERVED, and still be handed to a provider. The account's own status was
not read either, so a FROZEN account could convert.

This is the withdrawal surface that matters: the conservative and development
legal policies both deny `PAYOUT`, and `legalrouter.SandboxPolicy` is the one
policy that permits it, so every exercise of the withdrawal boundary this system
can perform goes through this path. `internal/withdrawal`, the older
crypto-address path that refuses everything today, has always made both checks —
which is the negative control in the auditor's test.

**Fix (D-092).** `payout.Service` takes a `KillSwitchChecker` and an account
reader, both required; `Create`, `CompleteVerification` and `Submit` call them
inside the transaction that authorizes them, switches first and account status
second. `Create` refuses before the quote is consumed, so a switch cannot burn a
quote on its way to refusing; `Submit` refuses inside the claim, the last point
before a provider is called under a committed idempotency key. At the boundary,
`compileRoute` pre-checks the compiled action's class against an in-process
snapshot of at most one second — explicitly a pre-check, never the authority.
Cancellation declares `CANCEL` and consults nothing, because that class is never
blocked and stopping a cancellation would hold a user's reserved value for the
length of an incident. Commit d074f55.

**Evidence.** TEST_INTEGRATION:
`TestIntegration_EveryWithdrawSwitchStopsAConversionRequest` activates each of
the three switches in turn, asserts KILL_SWITCH_ACTIVE naming it, asserts nothing
moved, releases it and asserts the path reopens;
`TestIntegration_AFrozenAccountCannotConvert` and
`TestIntegration_AKillSwitchStopsASubmissionMidFlight` cover the account status
and the submission. TEST_UNIT:
`TestAUDIT_NoKillSwitchCanReachTheConversionRequestPath` (the auditor's, now
green). STATIC_PROOF: `internal/payout/service.go`, `cmd/api/wire.go`.

## F-164 · ADR-0023 §3 said every sandbox permission's approval reference reads NOT-AN-APPROVAL · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, reproduced by
`TestAUDIT_EverySandboxPermissionsApprovalReferenceSaysItIsNotAnApproval`.

One of the six permissions in `legalrouter.SandboxPolicy` — the SIMULATION rule —
carries `PRODUCT-SIM-001`, not `NOT-AN-APPROVAL-SANDBOX-TIER-ONLY`. The code is
right: simulated capital moves nothing, the conservative and development policies
carry the same rule under the same reference, and it is the one rule in that
policy which is not a sandbox-tier concession. Labelling it one would imply the
other deployments permit simulation on nothing. The ADR sentence was the one a
reader checks the policy against, and it was false as written.

**Fix.** The sentence states the exception and why it exists; the auditor's test
asserts the corrected rule positively in both halves, and fails if the sandbox
policy ever stops permitting simulation, so the exception cannot become vacuous.
No code changed.

**Evidence.** TEST_UNIT:
`TestAUDIT_EverySandboxPermissionsApprovalReferenceSaysItIsNotAnApproval`.
STATIC_PROOF: `docs/adr/0023-the-sandbox-tier.md`;
`internal/legalrouter/sandbox.go`.

## F-165 · ADR-0023 cited a test directory that has never existed · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, by opening the path.

ADR-0023's Consequences section cited `test/integration/gates` for the four
properties a reader most needs proven: that a SANDBOX gate is active only for a
sandbox-tier checker, carries no approval, cannot be promoted from SANDBOX, and
cannot exist in PROD. That directory does not exist and never has —
`test/integration/` holds the migration, enum and a few cross-cutting suites, and
`internal/gates`' database-backed tests live beside the code behind
`//go:build integration`, which is what F-01 is about. The citation check in
`test/docs` covers Go test-function names, not paths, so nothing caught it.

**Fix.** The ADR cites `internal/gates/sandbox_integration_test.go`,
`sandbox_test.go` and `parity_integration_test.go`, which are the files that
prove those properties. `test/docs` now also resolves path-shaped citations in
the documents a reviewer relies on, so the next one fails a test instead of being
read.

**Evidence.** TEST_UNIT: `TestDocs_EveryPathTheyNameExists`. STATIC_PROOF:
`docs/adr/0023-the-sandbox-tier.md` Consequences and Evidence sections.

## F-224 · A payout below the provider's published minimum was created, reserved, submitted and settled, and the fee was never taken, because `quote_id` was optional · PRODUCTIZATION · P1 · FIXED

**Found by** the withdrawal-verification audit (goal §54), reproduced as
`TestAuditWV_APayoutBelowTheProviderMinimumIsReservedAndSettledWithoutAQuote`.
50 Credits — $0.50 gross — against a provider publishing a $1.00 minimum and a
25c + 25bp fee. `POST /v1/payouts/quote` refuses it in as many words
(`minimum_ok=false`, `net_amount_minor=25`); the same amount requested without
naming the quote reached VERIFIED with 50 000 000 base units reserved and then
SETTLED. Corroborated on a live sandbox tier, where the quote route answered
`422 UNSUPPORTED` and `POST /v1/payouts` answered `201`.

`quote_id` was optional in the schema, optional in `payout.CreateRequest`, and
the WHOLE minimum-and-fee branch of `payout.Service.Create` sat inside
`if r.QuoteID != nil`. `payout.Engine.Evaluate` had no minimum and no fee input
at all, so a request that named no quote met neither. The handler's stated reason
for the option — "an operator resolving a stuck payout has no quote to name" —
was false: the route is `accountScopeWrite`, and an operator resolves through
`ResolveManualReview`, which creates nothing.

**Fix.** D-119. `quote_id` is `required` in the schema and the handler refuses
`VALIDATION_FAILED` naming it; the false comment is gone. The minimum moves into
the domain: `payout.CreateRequest` carries `ProviderTerms` — the provider's
published fee model and minimum — the way it carries `DisclosureAccepted`, with
no permissive zero value, so a caller that forgets them stops a payout rather
than letting one through unpriced. `Create` refuses when the quote's net is under
the minimum the provider publishes TODAY, which is the case a value copied onto
the quote row cannot catch. Migration 00808 records the gross, the fee and the
net the customer was shown, because a payout that settles is explained by one row
rather than by a fee schedule that has since been repriced. What is reserved is
the GROSS: the fee comes out of what leaves rather than being added to it, so the
units the customer gives up are the units they asked to convert.

**Evidence.** STATIC_PROOF: `internal/payout/payout.go`,
`internal/payout/service.go`, `internal/httpapi/handlers_native.go`,
`openapi/openapi.yaml`,
`migrations/00808_a_conversion_request_records_the_price_the_customer_was_shown.sql`.
TEST_INT: `TestAuditWV_APayoutBelowTheProviderMinimumIsReservedAndSettledWithoutAQuote`
(inverted), `TestIntegration_APayoutIsRefusedUntilTheDisclosureIsAccepted`.
TEST_UNIT: `TestCreateRequest_Validate`.

## F-225 · The provider was instructed to pay no amount, to no destination, in no currency — and the request still reached SETTLED · PRODUCTIZATION · P1 · FIXED

**Found by** the same audit, reproduced as
`TestAuditWV_TheProviderIsToldTheAmountAndTheDestination`: a recording provider
captured `Amount.Minor()==0`, an empty destination reference, an empty
destination kind and an empty currency for a payout of 500 000 000 Credits, which
then reached SETTLED with the platform's `PAYOUT_SETTLED` balance reading
550000000.

`payout.Service.Submit` built a `SubmitRequest` carrying the idempotency key, the
reference and `money.USD{}`, and left everything else at its zero value. It took
no quote, no destination and no amount, and read neither
`payout_requests.quote_id` nor `destination_id`. The sandbox provider settled
anyway, because nothing asked it to check what it had been told.

**Fix.** `Submit` assembles the instruction inside the transaction that claims the
request, from the destination and the recorded quote: the amount is the quote's
NET in the quote's currency — what the customer was told would reach them — plus
the destination's provider reference and kind. It is built BEFORE the claim and
refused there, so a request that cannot be described to a provider stays in
VERIFIED with its value reserved rather than being claimed under a committed key
nobody can act on. `SubmitRequest.Validate()` refuses a zero amount, an empty
currency, an empty destination reference and an undeclared kind, on the type every
adapter is handed, so no adapter can receive one whichever caller assembled it;
`payouttest.Sandbox` validates what it is given and records it.

**Evidence.** STATIC_PROOF: `internal/payout/service.go` (`submitRequestFor`),
`internal/payout/provider.go` (`SubmitRequest.Validate`),
`internal/payout/payouttest/sandbox.go`. TEST_INT:
`TestAuditWV_TheProviderIsToldTheAmountAndTheDestination` (inverted).

## F-226 · An open sanctions review, an account restriction and an unsupported jurisdiction stopped `GET /v1/me/eligibility` and stopped nothing on the conversion path · PRODUCTIZATION · P1 · FIXED

**Found by** the same audit, reproduced as
`TestAuditWV_AnOpenSanctionsReviewStopsAConversionRequest`. A provider answer of
the shape ADR-0025 names — document, age, jurisdiction and sanctions PASS,
political exposure FAIL — is ingested; the profile reaches
`identity_state=VERIFIED`, `sanctions_state=REVIEW`; the resolver reports
PAYOUT_KYC; `eligibility.ExplainWithdrawal` reports `ACCOUNT_RESTRICTED`,
`eligible=false`, `withdrawable=0`. `payout.Service.Create` then reserved the
whole balance and `Submit` settled it.

`payout.EligibilityInput` had no field for the sanctions state, the account
restrictions or the jurisdiction verdict; `guardWithdraw` read only the kill
switches and the account's status; `payoutsAdapter.Create` read no compliance
column at all. Two surfaces answering one question from two readers, and only one
of them reading.

**Fix.** D-120. `payout.EligibilityInput` gains `SanctionsState`,
`AccountRestrictions` and `JurisdictionSupported`, none with a permissive zero
value, and `Engine.Evaluate` adds them to the absolute blocks beside
`ACCOUNT_FROZEN` with the words `eligibility.ExplainWithdrawal` already uses:
`ACCOUNT_RESTRICTED` and `JURISDICTION_RESTRICTED`. One reader answers them —
`httpapi.WithdrawalDeps.complianceFacts` — and the eligibility page, the quote and
the reserving transaction all call it, the last two inside their own transaction
so the answer belongs to the snapshot the decision is made in. The sanctions
screen is read the way the eligibility page reads it (HIT and REVIEW restrict,
CLEAR and UNKNOWN do not); an empty value is nobody having answered, and nobody
having answered is not a clearance.

**The correction this finding also makes.** F-168's entry said
`internal/eligibility/evaluate.go` reads `sanctions_state` "as one of the
allowlists that decides whether a payout may proceed". It does not:
`eligibility.Evaluate`, `eligibility.Policy` and the whole `Input`/`Decision`
engine have no production caller on any reachable path —
`settlement.PlanInput.Eligibility` is filled only by `settlementtest/world.go`.
The surface that reads the screen is `eligibility.ExplainWithdrawal`, and D-120 is
what puts the same fact on the conversion path. The unreachable engine is LEFT
where it is rather than deleted: it is the policy shape §19 describes and what a
second consumer would use. D-120 records that it has no caller so the next person
does not assume one.

**Evidence.** STATIC_PROOF: `internal/payout/eligibility.go`,
`internal/httpapi/wiring_verification.go`, `internal/httpapi/wiring_native.go`.
TEST_INT: `TestAuditWV_AnOpenSanctionsReviewStopsAConversionRequest` (inverted,
with a sub-case per fact).

## F-227 · The compliance transition table recorded edges and constrained none · PRODUCTIZATION · P1 · FIXED

**Found by** the same audit, reproduced as
`TestAuditWV_TheProfileTransitionTableConstrainsWhichEdgesAreLegal` and by hand
as `cp_app` on the live database. Three INSERTs, no code path: one moved a
compliance profile from UNVERIFIED to VERIFIED and the SECURITY DEFINER trigger
wrote the state, the `verified_at`, the expiry and a CLEAR sanctions screen from
it; one moved a verification session from CREATED to APPROVED for a session the
provider was never called for; four `verification_checks` rows with `outcome`
PASS then made `verification.Resolver.Level` report PAYOUT_KYC.

`VERIFICATION_AND_WITHDRAWAL.md` §4 says nothing reaches VERIFIED except from
PENDING, RESTRICTED or SUSPENDED, and that this "is a property of the transition
table rather than of the code that reads it". It was a property of
`internal/verification.CanTransition` and of nothing else: the edge bindings of
00731 and 00741 ask whether a transition row names the state the entity is really
in, never whether the edge that row describes exists.

**Fix.** D-121, migration 00806. `compliance_profile_state_edges` and
`verification_session_status_edges` are populated from
`verification.StateEdges()` and `verification.SessionEdges()`, held identical to
them by `test/integration/enums` the same way `AllStates()` is held to the CHECK,
and both apply functions raise AD001 for an edge that is not in them. Nothing but
`cp_migrate` may write the tables. `verification_checks` gains a BEFORE INSERT
trigger: a check must name its session's own provider, and the session must be in
a status only a provider ANSWER produces — PROCESSING, REQUIRES_INPUT,
MANUAL_REVIEW, APPROVED, DECLINED — so a forged session has nothing to attach to.
`Ingest` records the evidence after the status it belongs to rather than before
it, which is now load-bearing rather than incidental.

**The residual, stated.** A session that HAS been answered can still be given
further check rows by `cp_app`, and on a sandbox tier a rehearsal reaches
APPROVED legitimately. Closing that is the `capability_gates` treatment —
revoking INSERT on `verification_checks` from `cp_app` and routing
`Repository.RecordCheck` through a SECURITY DEFINER function — which is a change
to one call site and is recorded in D-121 as the next step rather than done here,
because a privilege change to evidence rows wants its own migration and its own
exploit test. A same-state transition row can also still carry a sanctions
screening decision, because the screen has no edge table of its own in either
language yet.

**Evidence.** STATIC_PROOF:
`migrations/00806_a_transition_table_that_records_edges_constrains_none.sql`,
`internal/verification/state.go`, `internal/verification/session.go`,
`internal/verification/service.go`. TEST_INT:
`TestAuditWV_TheProfileTransitionTableConstrainsWhichEdgesAreLegal` (all three
assertions inverted, with a positive control),
`TestIntegration_EveryLegalEdgeTableMatchesItsGoTable`,
`TestIntegration_NobodyButTheMigrationRoleWritesAnEdgeTable`.

## F-228 · A payout destination the provider had said it cannot pay was accepted and marked VERIFIED whenever the client omitted `country` · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, on a sandbox tier as `customer-a` with a fresh
step-up: a BANK destination with `"country":"FR"` answered
`503 PROVIDER_UNAVAILABLE RECIPIENT_COUNTRY_UNSUPPORTED`; the same body WITHOUT
`country` answered `201` with status VERIFIED and usable true.

`wiring_verification.go` asked `CanPayRecipient` only when the client supplied a
country AND the provider published some, and `country` was optional in the
schema. `payout.RecipientProfile.Validate` has always refused an empty country
with `RECIPIENT_PROFILE_INCOMPLETE`; the adapter simply never asked it. Nothing
ever set `RecipientProfile.Region`, so `RefusalRegionExcluded` and
`RefusalRegionUnknown` could not fire at all, and `providerSupports` at payout
checked only the kind and the currency.

**Fix.** D-122. `country` is required in the schema, in the handler and in the
domain; `CanPayRecipient` is asked unconditionally and about the whole profile;
`region` is accepted, stored (00810) and required wherever the provider publishes
excluded subdivisions for that country, refused `VALIDATION_FAILED` rather than
`PROVIDER_UNAVAILABLE` because it is something the caller can supply;
`providerSupports` at payout re-asks `CanPayRecipient` from the stored values
rather than re-deriving a weaker check at the moment value would leave. The web's
destination form makes the country a required field and asks for the region, with
the provider's refusal rendered where the person is.

**Evidence.** STATIC_PROOF: `internal/httpapi/wiring_verification.go`,
`internal/httpapi/wiring_native.go`,
`internal/httpapi/handlers_payout_destinations.go`,
`internal/payout/repository.go`, `openapi/openapi.yaml`,
`migrations/00810_a_conversion_request_says_whether_it_was_a_rehearsal.sql`,
`apps/web/src/pages/withdraw/Destinations.tsx`. TEST_UNIT:
`TestPostMePayoutDestinations_RequiresTheCountryItPaysInto`. TEST_E2E:
`apps/web/e2e/scenarios/f-verified-sandbox.spec.ts` step 2b.

## F-229 · `payout_requests.state` and its money columns were freely writable by `cp_app` · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAuditWV_TheConversionRequestStateMachineIsEnforcedByTheDatabase`. One
transaction as `cp_app`: an honest transition row REJECTED to SETTLED — an edge
`payout.CanTransition` does not have — plus an UPDATE setting the state, the
reserved and settled quantities, the settled instant, the provider and a forged
provider reference. It committed, and was rendered to the holder as a SETTLED
payout of the whole amount with no ledger posting, no allocation, and its
eligibility reasons still naming the shortfall that rejected it.

Every neighbouring state column in this area got the F-42 treatment in this
goal's wave — 00761, 00762, 00763 — and this one, the row that says whether
somebody's money left, kept a column grant that included `state` and all four
money columns (00713, narrowed but not removed by 00733).

**Fix.** D-123, migration 00807. `payout_request_state_edges` is populated from
`payout.StateEdges()` and enum-paired like the others;
`cp_payout_apply_state_transition()` writes the state from the transition row and
refuses an edge that is not in the table. The money moves ONTO the transition row
— the two quantities and their instants — with `provider_reference` and
`provider_status`, because every place this package wrote one of them it was also
moving the state, so the two become one event and a quantity can no longer be
rewritten beside a lawful move. UPDATE on `payout_requests` is revoked from
`cp_app`, with a column grant back for the provider slot it claims before it
calls, the decision it records, and `failure_reason`. The DEFERRED edge binding
from 00731 stays as belt and braces: it still catches a write by a role that HAS
the privilege with no transition row at all.

**Evidence.** STATIC_PROOF:
`migrations/00807_a_conversion_requests_state_and_its_money_are_not_the_applications_to_write.sql`,
`internal/payout/payout.go` (`StateEdges`), `internal/payout/service.go`
(`stateChange`, `transitionWith`). TEST_INT:
`TestAuditWV_TheConversionRequestStateMachineIsEnforcedByTheDatabase` (with five
privilege probes and a positive control),
`TestIntegration_MoneyColumnsAreOutOfTheApplicationsReach`,
`TestIntegration_EveryLegalEdgeTableMatchesItsGoTable`.

## F-230 · No earned Credit could ever reach a payout-eligible funding finality, and the explanation said waiting would fix it · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAuditWV_AnEarnedCreditCanReachAPayoutEligibleFinality`, and independently by
the browser audit as **F-e2e-1** (`apps/web/e2e/audit-surface.spec.ts`, "an
earning of a permitted origin can reach the payout path"): a verified customer
with a tokenised sandbox destination, holding `DATA_SALE_EARNING` and
`MARKET_TRADING_PROCEEDS`, told `FUNDING_NOT_SETTLED` with nothing able to settle
it.

`internal/nativemarket` and `internal/commerce` minted every earning at
`FinalityReversible` unconditionally, both with a comment saying an earning
cannot be more final than the money behind it and neither looking at the money
behind it. The only writer that promotes a lot out of REVERSIBLE is
`credit.Service.SettleFunding`, which keys on `credit_fundings.lot_id` — a row
`RecordLot` never creates. `FundingFinality.PayoutEligible()` admits only SETTLED
and UNFUNDED, so five of the six origins `valuedomain.SandboxPolicy` marks
withdrawable could never be withdrawn on any deployment, and
`VERIFICATION_AND_WITHDRAWAL.md` §9's "reachable end to end on a sandbox tier"
was false for every value a sandbox tier can produce.

**Fix.** D-124, migration 00809. A derived lot — trading proceeds, a creator
earning, marketplace proceeds and the fees on them — records the lots consumed to
fund it in `credit_lot_parents`, written at mint in the same transaction, and is
minted at the LEAST final finality among them: UNFUNDED or SETTLED parents give a
payout-eligible lot at birth, a REVERSIBLE parent gives a REVERSIBLE lot.
`credit.Service.SettleDerived` promotes it once EVERY parent is payout-eligible,
run from the settlement ticker in `cmd/api` immediately after `settleOnce`,
batch-bounded, each lot taken under the advisory lock `SetFinality` already uses
and tried rather than waited for. A parent that is DISPUTED or REVERSED freezes
what was derived from it.

The pool is fungible, so the pool keeps the same kind of record:
`native_market_credit_sources` holds the lots paid into a market's reserve and
the sell side draws them down in arrival order, so a sale's proceeds name real
parents. A default either way would be wrong — REVERSIBLE for ever is the defect
renamed, and payout-eligible by default is buy with a card, sell back, withdraw,
charge back — and a draw-down the record cannot cover mints REVERSIBLE, because
"we do not know what funded this" is not "it was settled".

**What a chargeback on a spent purchase does, precisely.**
`credit.Service.Reverse` unwinds the FUNDING'S OWN lot and posts a deficit
against the payer. Taking value back from a third party who earned it is a
posting kind this ledger does not have, and the earner may have spent it or had
it paid out. What IS expressible is applied: `SettleDerived` moves a derived lot
whose parent is DISPUTED or REVERSED to DISPUTED, which is neither spendable nor
payout-eligible, and never promotes it. What is not expressible is the residual:
a derived lot promoted on a SETTLED parent that is disputed months later cannot
be clawed back from the earner, and value already spent or already paid out
cannot be recovered at all. D-124 records that rather than inventing a posting
for it.

**Evidence.** STATIC_PROOF:
`migrations/00809_proceeds_are_as_final_as_what_paid_for_them.sql`,
`internal/credit/derived.go`, `internal/nativemarket/poolprovenance.go`,
`internal/nativemarket/service.go`, `internal/commerce/service.go`,
`cmd/api/creditsettle.go`, `internal/eligibility/withdrawal.go`. TEST_INT:
`TestAuditWV_AnEarnedCreditCanReachAPayoutEligibleFinality` (inverted),
`TestAuditWV_AProceedsLotSettlesWhenItsFundingDoesAndNotBefore`,
`TestIntegration_ASandboxTraderEarnsProceedsThatCanReachAPayout`,
`TestIntegration_ProceedsOfAReversiblePurchaseAreReversible`,
`TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave` (narrowed to the claim
that is now load-bearing: the GRANT never leaves by ORIGIN, and every other
seeded lot is proved derived from it).

## F-231 · The single-use hosted verification link was written to `idempotency_keys.response_body` · PRODUCTIZATION · P2 · FIXED

**Found by** the same audit, reproduced as
`TestAuditWV_TheHostedVerificationURLIsWrittenDownNowhere` and live: starting a
verification session under an Idempotency-Key returned a hosted URL, and reading
`idempotency_keys.response_body` for that key returned the body with the link in
it, expiring twenty-four hours later.

ADR-0025 §3 says the hosted URL "is handed to the browser that asked for it and
written down nowhere", and `VERIFICATION_AND_WITHDRAWAL.md` §5 lists it under
"Never stored". The route is Mutating, so `runCommand` marshalled the whole
response into a row `cp_readonly` and `cp_ops` may SELECT and `cp_app` may not
DELETE.

**Fix.** D-125. `CommandResult` gains a `StoredBody`: the caller gets the whole
answer, the record gets the answer minus the fields the route declares
never-stored. `neverStored` is per operation and per field, because the same
field name on a quote is a price's expiry that MUST be recorded. A replay answers
with the session, without the link, and with a `resume` sentence telling the
client to start a new session — saying nothing would leave a browser waiting for
a field that is never coming. `hosted_url` stays optional in the schema, `resume`
is added, and the Verify page renders it.

**Evidence.** STATIC_PROOF: `internal/httpapi/neverstored.go`,
`internal/httpapi/command.go`, `internal/httpapi/ports.go`,
`internal/httpapi/wiring.go`, `openapi/openapi.yaml`,
`apps/web/src/pages/verify/Verify.tsx`. TEST_INT:
`TestAuditWV_TheHostedVerificationURLIsWrittenDownNowhere` (inverted, plus the
replay). TEST_UNIT: `TestNoNeverStoredFieldReachesTheIdempotencyRecord`, which
drives every command route in the area and reads what landed.

## F-232 · The conversion request recorded no sandbox fact · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit: the payout list returned items with no `sandbox`
field, the by-id read returned `"sandbox":true`, and the create response returned
201 without it. The by-id read answered from
`s.opts.Ports.Payouts.SandboxProvider()` — what the provider is NOW — and
`payout_requests` had no column.

**Fix.** Migration 00810 adds `sandbox` and `environment`, written at Create from
the provider's availability and `cfg.SandboxTier()` as `AddDestination` already
does, with 00762's CHECK that a rehearsal cannot exist in PROD, and `toAPIPayout`
reads the column so the list, the create response and the by-id read agree
(D-096 and F-158's treatment). The columns are nullable and NULL renders as a
rehearsal: a row written before the migration has no recorded fact, and a NOT
NULL default of false would assert about every one of them that it was real,
which is the one direction this label must never be wrong in.
`CreateRequest.Validate` refuses a request with no environment, so nothing in
this binary writes another NULL.

**Evidence.** STATIC_PROOF:
`migrations/00810_a_conversion_request_says_whether_it_was_a_rehearsal.sql`,
`internal/payout/payout.go`, `internal/payout/service.go`,
`internal/httpapi/handlers_native.go`, `internal/httpapi/wiring_native.go`.
TEST_UNIT: `TestCreateRequest_Validate`. TEST_E2E:
`apps/web/e2e/scenarios/f-verified-sandbox.spec.ts` step 7.

## F-233 · `POST /v1/payouts` answered 403 for another account's destination where every sibling answers 404 · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit: `customer-a` naming `customer-b`'s destination got
403 FORBIDDEN with "that payout destination belongs to another account", and a
non-existent id got 404. A distinguishable refusal is a membership oracle:
anybody could learn which destination ids exist by asking.

**Fix.** NOT_FOUND, "no such payout destination", in both places —
`payoutsAdapter.Create` and `payout.Service.Quote` — which is F-41's rule and
what `DisableDestination`, `conversionAdapter.Quote` and the by-id read already
answer.

**Evidence.** STATIC_PROOF: `internal/httpapi/wiring_native.go`,
`internal/payout/quote.go`.

## F-234 · `PROVIDER_BOUNDARY.md` §3 described a contract the shipped interfaces do not have, and §2's journey named a route that does not exist · PRODUCTIZATION · P3 · FIXED

**Found by** the same audit, by reading the document against the code: nine
methods and an error taxonomy that are not there (a `Capabilities` that takes a
jurisdiction, `QuotePayout`, `GetPayout`, `ListPayouts`, `ParsePayoutWebhook`,
`RedactVerification`, retryable and terminal error classes, and a provider name
and request id on every response), `POST /v1/me/terms/accept` where the route is
`POST /v1/me/terms-acceptances`, and an ACCOUNT_RESTRICTED clause naming a
restriction nothing writes.

**Fix.** §3 is split into "what a Role C contract must be able to express" — the
union of what Persona, Veriff, Sumsub, Stripe, Tilia and Thunes each require,
kept, because it is what an adapter is designed against — and "what the shipped
interfaces are", which lists `payout.Provider`'s three methods and
`verification.Provider`'s, with the gap recorded as what a real adapter will add.
The route name is corrected. `compliance_profiles.restrictions` is stated as read
by two surfaces and written by nothing, with what would write it: a
dual-controlled operator action on the compliance profile, which is an admin kind
with an approval, an executor and a transition row rather than one line, so it is
named rather than wired.

**Evidence.** STATIC_PROOF: `docs/product/PROVIDER_BOUNDARY.md` §3,
`docs/product/VERIFICATION_AND_WITHDRAWAL.md` §2, §3. TEST_UNIT:
`TestDocs_EveryPathTheyNameExists`, `TestDocs_CountsMatchTheCode`.

## F-249 · The closure decision read its financial blockers as an aggregate, so a conversion request committing beside it left a reservation on a CLOSED account · PRODUCTIZATION · P2 · FIXED

**Found by** the withdrawal-verification audit's notes, against D-102.
`profile.Repository.ClosureBlockers` reads the Credit balance, the open payout
requests and the open native positions in one aggregate statement, and an
aggregate read sees a snapshot. Under READ COMMITTED a `payout.Service.Create`
committing between that read and the closure's own commit is invisible to both:
the closure sees no open request, the payout sees an ACTIVE account, and the
reservation lands on an account that is CLOSED a moment later — value held out of
the balance of somebody who can no longer sign in to cancel it, recoverable only
by an operator, and refused by `Submit` in the meantime.

**Fix.** PostgreSQL will not take a row lock through an aggregate, so it is a
second statement rather than a clause: the EFFECT path locks the person's
`accounts` rows FOR UPDATE before it reads the blockers, and
`payout.Service.guardWithdraw` takes FOR SHARE on the same row before it reads
the account's status. One of the two now waits and then sees what the other did —
either the request commits first and blocks the closure with a reason the
operator can give the person, or the closure commits first and the request meets
the CLOSED status in its own guard. `cp_app` may take the lock because 00744 left
a column grant on `accounts`, which is what a row lock needs. FOR SHARE rather
than FOR UPDATE on the payout side, so two concurrent conversion requests on one
account do not queue behind each other there; they are already serialised on the
lots they consume.

**Evidence.** STATIC_PROOF: `internal/profile/repo.go` (`LockHoldings`),
`internal/profile/account.go`, `internal/payout/service.go`. TEST_INT:
`TestIntegration_ACreateWaitsForAnAccountLockTheClosureWouldHold`.

## F-250 · A browser scenario asserted that a reserved payout stops at VERIFIED, which the sweep had made false · PRODUCTIZATION · P3 · FIXED

**Found by** the browser audit (F-e2e-4).
`apps/web/e2e/scenarios/f-verified-sandbox.spec.ts` asserted that a reserved
request stays in VERIFIED and said in as many words that "nothing in this build
hands a reserved request to a provider, so it never reaches PROVIDER_PENDING".
`cmd/api/payoutsweep.go` (D-085) submits reserved requests on a fifteen-second
ticker and the sandbox provider settles five seconds after it accepts; the
assertion passed only because it read the state immediately, before the first
tick. The same spec explained FUNDING_NOT_SETTLED with "every Credit on a seeded
deployment is UNFUNDED… an earning inherits that", which was not the mechanism —
earnings were minted REVERSIBLE unconditionally, which is F-230.

**Fix.** The scenario follows the request through the sweep with a bounded poll
of the by-id read and asserts the states the deployment actually reaches, that
the payout is labelled a rehearsal on the row rather than by asking what the
provider is today, and that the page shows the state the request is in. The
comment says what D-124 makes true, and the eligibility branch stops asserting
FUNDING_NOT_SETTLED specifically: it asserts that a permitted origin which cannot
leave says why.

**Evidence.** STATIC_PROOF: `apps/web/e2e/scenarios/f-verified-sandbox.spec.ts`
steps 3 and 7. TEST_E2E: the withdraw and verification specs against a
sandbox-tier API on ports 18230 and 18231.

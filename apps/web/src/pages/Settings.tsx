/**
 * SETTINGS AND SECURITY (PART 111): sessions, security, funding, wallets,
 * disclosures, risk preferences.
 *
 * Two panels here are deliberately blunt. The withdrawal form is a real form
 * that sends a real request and shows whatever the backend answers — in this
 * deployment that is a refusal, and a refusal with a reason is far more useful
 * than a greyed-out control with no explanation. And risk preferences are shown
 * read-only, because the v1 API has no endpoint that writes them and a form
 * that appeared to save a preference it could not save would be a lie the user
 * would only discover when it failed to hold.
 */
import { useMemo, useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useAssets,
  useBuyingPower,
  useDeposits,
  useHoldings,
  useRequestWithdrawal,
  useRevokeSession,
  useSessions,
  useSignOut,
  type BuyingPower,
  type Deposit,
  type HoldingsResponse,
  type SessionSummary,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState, Explanation } from "../components/DataState.tsx";
import { Button } from "../components/Button.tsx";
import { LOGIN_PATH } from "../api/client.ts";
import {
  AsOf,
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  Page,
  Panel,
  Pill,
  Table,
} from "../components/Layout.tsx";
import {
  CONFIDENCE_DISCLAIMER,
  PENDING_SETTLEMENT_NOTE,
  RISK_FOOTER,
  SIMULATED_RESULTS_NOTICE,
  USDC_DISCLOSURE,
  USD_VALUATION_NOTE,
} from "../lib/honesty.ts";
import { parseQuantityInput } from "../lib/money.ts";
import { formatInstant } from "../lib/time.ts";
import { useActiveAccountId, useSession } from "../session.tsx";

export function Settings(): ReactNode {
  const session = useSession();
  const accountId = useActiveAccountId();
  const sessions = useSessions(session.signedIn);
  const revoke = useRevokeSession();
  const signOut = useSignOut();
  const deposits = useDeposits(accountId);
  const holdings = useHoldings(accountId);
  const buyingPower = useBuyingPower(accountId, "WITHDRAWAL");

  return (
    <Page
      title="Settings and security"
      lead="Who you are to the backend, which sessions can act as you, and exactly what this account can and cannot do."
    >
      <Panel title="Signed-in principal" description="As the backend reports it, not as the browser remembers it.">
        {session.principal === undefined ? (
          <EmptyState title="No principal" body="The backend returned no principal for this session." />
        ) : (
          <FieldGrid columns={3}>
            <Field label="Subject">
              <Identifier value={session.principal.subject_id} />
            </Field>
            <Field label="Actor type">{session.principal.actor_type}</Field>
            <Field label="Roles">{session.principal.roles.join(", ")}</Field>
            <Field label="Authenticated at">{formatInstant(session.principal.auth_time)}</Field>
            <Field
              label="Authentication methods"
              note="What the identity provider asserted about how you proved who you are."
            >
              {session.principal.amr.length === 0 ? (
                <span className="absent">none reported</span>
              ) : (
                session.principal.amr.join(", ")
              )}
            </Field>
            <Field
              label="Strong authentication valid until"
              note="Sensitive actions require a recent strong authentication."
            >
              {session.principal.step_up_valid_until === undefined ? (
                <span className="absent">no strong authentication on this session</span>
              ) : (
                formatInstant(session.principal.step_up_valid_until)
              )}
            </Field>
            <Field label="Accounts this session may act on">
              {session.principal.account_ids.length === 0 ? (
                <span className="absent">none</span>
              ) : (
                session.principal.account_ids.map((id) => (
                  <div key={id}>
                    <Identifier value={id} />
                  </div>
                ))
              )}
            </Field>
          </FieldGrid>
        )}
        <div className="form-actions">
          <a className="btn btn-secondary" href={`${LOGIN_PATH}?step_up=true`}>
            Re-authenticate strongly
          </a>
          <Button
            variant="danger"
            busy={signOut.isPending}
            busyLabel="Signing out…"
            onClick={() => {
              signOut.mutate(undefined, {
                onSuccess: () => {
                  window.location.assign("/");
                },
              });
            }}
          >
            Sign out
          </Button>
        </div>
        <p className="field-note">
          Re-authenticating asks the identity provider for a stronger assertion and returns you here.
          It is the same flow the backend demands before a sensitive action.
        </p>
        {signOut.isError && <Explanation error={signOut.error} />}
      </Panel>

      <Panel title="Sessions" description="Every session that can currently act as you.">
        <AsyncPanel
          query={sessions}
          loadingLabel="Loading sessions…"
          empty={{
            isEmpty: (list: SessionSummary[]) => list.length === 0,
            title: "No sessions",
            body: "The backend returned no sessions, which is unusual given you are reading this. Try reloading.",
          }}
        >
          {(list: SessionSummary[]) => (
            <Table
              caption="Sessions"
              headers={["Started", "Last seen", "Expires", "Address", "Device", "This one", "Revoke"]}
            >
              {list.map((item) => (
                <tr key={item.id}>
                  <td>
                    {formatInstant(item.created_at)}
                    <div className="mono-small">{item.id}</div>
                  </td>
                  <td>{formatInstant(item.last_seen_at)}</td>
                  <td>{formatInstant(item.expires_at)}</td>
                  <td className="mono-small">{item.ip ?? "not recorded"}</td>
                  <td className="cell-prose">
                    {item.device_label ?? item.user_agent ?? "not recorded"}
                  </td>
                  <td>{item.current === true ? <Pill tone="info">current</Pill> : ""}</td>
                  <td>
                    {item.revoked_at !== undefined ? (
                      <Button disabledReason={`Already revoked ${formatInstant(item.revoked_at)}.`}>
                        Revoke
                      </Button>
                    ) : (
                      <Button
                        variant="danger"
                        busy={revoke.isPending && revoke.variables === item.id}
                        busyLabel="Revoking…"
                        onClick={() => {
                          revoke.mutate(item.id);
                        }}
                      >
                        Revoke
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </Table>
          )}
        </AsyncPanel>
        {revoke.isError && <Explanation error={revoke.error} />}
        <p className="note">
          Revoking a session ends it on the server. Revoking the one you are using signs you out of
          this tab as soon as the next request is made.
        </p>
      </Panel>

      <Panel title="Wallets and custody" description="Where the assets on this account are actually held.">
        <AsyncPanel
          query={holdings}
          loadingLabel="Loading custody information…"
          empty={{
            isEmpty: (data: HoldingsResponse) => data.holdings.length === 0,
            title: "No holdings, so no custody to report",
            body: "The backend returned no holdings for this account.",
          }}
        >
          {(data: HoldingsResponse) => (
            <>
              <Table caption="Custody" headers={["Asset", "Chain", "Mint", "Held at"]}>
                {data.holdings.map((holding) => (
                  <tr key={holding.asset}>
                    <th scope="row">{holding.symbol}</th>
                    <td>{holding.chain ?? "not reported"}</td>
                    <td>
                      <code className="mono-small">{holding.mint_address ?? "not reported"}</code>
                    </td>
                    <td>
                      {holding.location === undefined || holding.location === "" ? (
                        <span className="absent">the backend reported no custody address</span>
                      ) : (
                        <Identifier value={holding.location} />
                      )}
                    </td>
                  </tr>
                ))}
              </Table>
              <AsOf at={data.as_of} />
              <p className="note">
                The v1 API has no wallet resource: there is no endpoint that lists wallets, their key
                custody arrangement, or their signing policy. What is shown above is the custody
                location the holdings endpoint reports, and nothing more is claimed.
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel title="Funding" description="A summary of funding on this account.">
        <AsyncPanel
          query={deposits}
          loadingLabel="Loading funding records…"
          empty={{
            isEmpty: (list: Deposit[]) => list.length === 0,
            title: "No funding records",
            body: "The backend has recorded no deposits for this account.",
          }}
        >
          {(list: Deposit[]) => (
            <Table caption="Funding summary" headers={["Started", "Provider", "Status", "Spendable"]}>
              {list.map((deposit) => (
                <tr key={deposit.id}>
                  <td>{formatInstant(deposit.created_at)}</td>
                  <td>{deposit.provider}</td>
                  <td>
                    <Pill tone={deposit.status === "AVAILABLE" ? "good" : "info"}>{deposit.status}</Pill>
                  </td>
                  <td>{deposit.buying_power_eligible ? "yes" : "not yet"}</td>
                </tr>
              ))}
            </Table>
          )}
        </AsyncPanel>
        <Disclosure title="Settlement">
          <p>{PENDING_SETTLEMENT_NOTE}</p>
        </Disclosure>
      </Panel>

      {accountId !== undefined && <Withdrawals accountId={accountId} />}

      <Panel title="Risk preferences" description="What the backend is enforcing on this account right now.">
        <AsyncPanel query={buyingPower} loadingLabel="Loading the applicable policy…">
          {(data: BuyingPower) => (
            <>
              <FieldGrid columns={2}>
                <Field label="Policy version" note="The exact policy the figures were computed under.">
                  <Identifier value={data.policy_version} />
                </Field>
                <Field label="Evaluated for">{data.purpose}</Field>
              </FieldGrid>
              <h3>Restrictions</h3>
              {data.restrictions.length === 0 ? (
                <p className="note">
                  The backend applied no restrictions to this account at this instant.
                </p>
              ) : (
                <Table caption="Restrictions" headers={["Code", "Scope", "Blocking", "Detail"]}>
                  {data.restrictions.map((restriction) => (
                    <tr key={`${restriction.code}-${restriction.scope}`}>
                      <th scope="row" className="mono-small">
                        {restriction.code}
                      </th>
                      <td>{restriction.scope}</td>
                      <td>
                        {restriction.blocking ? <Pill tone="bad">blocking</Pill> : <Pill tone="warn">advisory</Pill>}
                      </td>
                      <td className="cell-prose">{restriction.detail}</td>
                    </tr>
                  ))}
                </Table>
              )}
              <h3>Haircuts</h3>
              {data.haircuts.length === 0 ? (
                <p className="note">No haircut is being applied to any asset on this account.</p>
              ) : (
                <Table caption="Haircuts" headers={["Asset", "Factor", "Reason"]}>
                  {data.haircuts.map((haircut) => (
                    <tr key={`${haircut.asset}-${haircut.reason}`}>
                      <th scope="row" className="mono-small">
                        {haircut.asset}
                      </th>
                      <td>{String(haircut.factor_bps)} bps</td>
                      <td className="cell-prose">{haircut.reason}</td>
                    </tr>
                  ))}
                </Table>
              )}
              <AsOf at={data.as_of} />
            </>
          )}
        </AsyncPanel>
        <Button disabledReason="The v1 API exposes no endpoint that writes a risk preference, so there is nothing for this to save.">
          Change risk preferences
        </Button>
        <p className="note">
          Customer-set risk preferences narrow the platform's own limits; they never widen them. When
          an endpoint exists, this panel will write to it — until then it shows what is in force and
          does not offer a control that could not take effect.
        </p>
      </Panel>

      <Panel title="Disclosures" description="The standing statements this product is required to make.">
        <Disclosure title="What you hold">
          <p>{USDC_DISCLOSURE}</p>
          <p>{USD_VALUATION_NOTE}</p>
        </Disclosure>
        <Disclosure title="Settlement">
          <p>{PENDING_SETTLEMENT_NOTE}</p>
        </Disclosure>
        <Disclosure title="Simulated results">
          <p>{SIMULATED_RESULTS_NOTICE}</p>
        </Disclosure>
        <Disclosure title="Model scores">
          <p>{CONFIDENCE_DISCLAIMER}</p>
        </Disclosure>
        <Disclosure title="Risk">
          <p>{RISK_FOOTER}</p>
        </Disclosure>
      </Panel>
    </Page>
  );
}

/**
 * A real withdrawal form. In this deployment the backend refuses — step-up
 * first, and the WITHDRAWALS capability is disabled in v1 — and the refusal is
 * shown as the backend wrote it.
 */
function Withdrawals(props: { readonly accountId: string }): ReactNode {
  const holdings = useHoldings(props.accountId);
  const assets = useAssets();
  const withdraw = useRequestWithdrawal();
  const [assetId, setAssetId] = useState("");
  const [amount, setAmount] = useState("");
  const [destination, setDestination] = useState("");
  const [idempotencyKey, setIdempotencyKey] = useState(() => newIdempotencyKey());

  const options = useMemo(
    () => (holdings.data?.holdings ?? []).map((holding) => ({
      id: holding.asset,
      symbol: holding.symbol,
      decimals: holding.decimals,
    })),
    [holdings.data],
  );
  const chosen = assetId !== "" ? assetId : (options[0]?.id ?? "");
  const decimals = options.find((option) => option.id === chosen)?.decimals;
  const parsed = parseQuantityInput(amount, decimals ?? -1);
  const ready = chosen !== "" && parsed.ok && destination.trim() !== "";

  return (
    <Panel title="Withdrawals" description="Moving assets off the platform.">
      {assets.isError && <Explanation error={assets.error} />}
      {options.length === 0 ? (
        <EmptyState
          title="Nothing to withdraw"
          body="The backend reported no holdings on this account, so there is nothing a withdrawal could move."
        />
      ) : (
        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            if (!ready) return;
            withdraw.mutate({
              accountId: props.accountId,
              assetId: chosen,
              quantity: parsed.value,
              destinationAddress: destination.trim(),
              idempotencyKey,
            });
          }}
        >
          <div className="form-row">
            <label htmlFor="withdraw-asset">Asset</label>
            <select
              id="withdraw-asset"
              className="input"
              value={chosen}
              onChange={(event) => {
                setAssetId(event.target.value);
              }}
            >
              {options.map((option) => (
                <option key={option.id} value={option.id}>
                  {option.symbol}
                </option>
              ))}
            </select>
          </div>
          <div className="form-row">
            <label htmlFor="withdraw-amount">Amount</label>
            <input
              id="withdraw-amount"
              className="input"
              inputMode="decimal"
              autoComplete="off"
              value={amount}
              aria-describedby="withdraw-amount-help"
              {...(parsed.ok || amount === "" ? {} : { "aria-invalid": true })}
              onChange={(event) => {
                setAmount(event.target.value);
              }}
            />
            <p id="withdraw-amount-help" className="field-note">
              {amount === ""
                ? "Converted to exact base units by shifting the decimal point, never by multiplying."
                : parsed.ok
                  ? `Will be sent as ${parsed.value} base units`
                  : parsed.error}
            </p>
          </div>
          <div className="form-row">
            <label htmlFor="withdraw-destination">Destination address</label>
            <input
              id="withdraw-destination"
              className="input"
              autoComplete="off"
              spellCheck={false}
              value={destination}
              onChange={(event) => {
                setDestination(event.target.value);
              }}
            />
            <p className="field-note">
              The backend validates this address. A transfer to a wrong address on a public chain
              cannot be reversed by anyone.
            </p>
          </div>
          <div className="form-actions">
            {ready ? (
              <Button submit variant="danger" busy={withdraw.isPending} busyLabel="Requesting…">
                Request withdrawal
              </Button>
            ) : (
              <Button
                variant="danger"
                disabledReason={
                  chosen === ""
                    ? "Choose an asset."
                    : !parsed.ok
                      ? amount === ""
                        ? "Enter an amount."
                        : parsed.error
                      : "Enter a destination address."
                }
              >
                Request withdrawal
              </Button>
            )}
            <Button
              variant="quiet"
              onClick={() => {
                setIdempotencyKey(newIdempotencyKey());
                setAmount("");
                setDestination("");
                withdraw.reset();
              }}
            >
              Clear
            </Button>
          </div>
          <p className="mono-small">idempotency key {idempotencyKey}</p>
        </form>
      )}

      {withdraw.isError && (
        <Explanation error={withdraw.error}>
          <p>
            Nothing was moved and no request is pending. This is the backend's own answer, shown
            unedited, rather than a control that was hidden from you without explanation.
          </p>
        </Explanation>
      )}
      {withdraw.isSuccess && (
        <div className="callout">
          <h3>Withdrawal requested</h3>
          <FieldGrid columns={2}>
            <Field label="Withdrawal">
              <Identifier value={withdraw.data.id} />
            </Field>
            <Field label="Status">
              <Pill tone="info">{withdraw.data.status}</Pill>
            </Field>
            <Field label="Destination">
              <Identifier value={withdraw.data.destination_address} />
            </Field>
            <Field label="Requested at">{formatInstant(withdraw.data.created_at)}</Field>
          </FieldGrid>
        </div>
      )}
    </Panel>
  );
}

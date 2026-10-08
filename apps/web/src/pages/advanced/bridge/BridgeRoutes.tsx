/**
 * Bridge routes and the account's transfer form.
 *
 * A transfer acts only on an account the session owns. The quantity is typed
 * as a decimal of the route's source asset and sent as base units by moving
 * digits; the backend decides everything else — the route's status, the
 * holder's balance on the source chain, Risk Kernel V2, the reservation — and
 * its answer is the leg, shown with its state and its progress. A leg whose
 * lock submission has an unknown outcome is shown as unknown, never as a
 * success or a failure.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useBridgeRoutes,
  useBridgeTransfer,
  type BridgeLegResult,
  type BridgeRoute,
  type BridgeRouteList,
  type TransferRequest,
} from "../../../api/advanced/bridge.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, DecimalValue, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { BRIDGE_LEG_MODES, ROUTE_STATUS_MEANING, routeStatusTone, type BridgeLegMode } from "../../../lib/bridge.ts";
import { parseQuantityInput } from "../../../lib/money.ts";
import { REQUEST_MODE_MEANING } from "../../../lib/stable.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { ScrollTable } from "../shared.tsx";
import { CommandFlow, ReplayNote, type Prepared } from "../stable/parts.tsx";
import { BridgeNav, LegFields, LegProgress } from "./parts.tsx";
import { legHref } from "./paths.ts";

export function BridgeRoutesPage(): ReactNode {
  const routes = useBridgeRoutes();
  return (
    <Page title="Bridge routes" lead="The routes between the simulated chains, and moving your own units along one.">
      <SimulatedNotice />
      <BridgeNav />
      <Panel
        title="Routes"
        description="Each joins a simulated asset on one simulated chain to its bridged twin on another, through one bridge facility. The twin can only be minted by an attested release."
      >
        <AsyncPanel
          query={routes}
          loadingLabel="Loading the bridge routes…"
          empty={{
            isEmpty: (data: BridgeRouteList) => data.items.length === 0,
            title: "No bridge route exists",
            body: "The backend lists no route in this deployment. An operator opens one; nothing is shown in its place.",
          }}
        >
          {(data: BridgeRouteList) => (
            <>
              <RoutesTable routes={data.items} />
              <TransferPanel routes={data.items} />
            </>
          )}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function RoutesTable(props: { readonly routes: readonly BridgeRoute[] }): ReactNode {
  return (
    <ScrollTable caption="Bridge routes" headers={["Route", "Status", "From", "To", "Unit value", "Release attempts", "Opened"]}>
      {props.routes.map((route) => (
        <tr key={route.id} data-route={route.id}>
          <th scope="row">
            {route.name}
            <span className="cell-note mono-small">{route.id}</span>
          </th>
          <td>
            <Pill tone={routeStatusTone(route.status)} title={ROUTE_STATUS_MEANING[route.status]}>
              {route.status}
            </Pill>
          </td>
          <td>
            {route.source_symbol} <code className="mono-small">{route.source_chain}</code>
          </td>
          <td>
            {route.destination_symbol} <code className="mono-small">{route.destination_chain}</code>
          </td>
          <td>
            <DecimalValue value={route.unit_price.decimal} /> {route.unit_price.quote}
            <span className="cell-note">per whole unit, for the risk notional and buying power</span>
          </td>
          <td>{String(route.max_release_attempts)}</td>
          <td>{formatInstant(route.created_at)}</td>
        </tr>
      ))}
    </ScrollTable>
  );
}

function TransferPanel(props: { readonly routes: readonly BridgeRoute[] }): ReactNode {
  const account = useAdvancedAccount();
  const own = account.ownAccountId;
  const command = useBridgeTransfer();
  const [routeId, setRouteId] = useState<string>(props.routes[0]?.id ?? "");
  const [raw, setRaw] = useState("");
  const [mode, setMode] = useState<BridgeLegMode>("SANDBOX");
  const routeSelect = useId();
  const quantityId = useId();
  const helpId = useId();
  const modeId = useId();
  const route = props.routes.find((candidate) => candidate.id === routeId) ?? props.routes[0];
  const parsed = route === undefined ? undefined : parseQuantityInput(raw, route.decimals);

  let prepared: Prepared<TransferRequest>;
  if (own === undefined) {
    prepared = { ok: false, reason: "This session owns no account; units are bridged only from an account the session owns." };
  } else if (route === undefined) {
    prepared = { ok: false, reason: "Choose a route." };
  } else if (route.status !== "ACTIVE") {
    prepared = { ok: false, reason: `The service takes new legs only on an ACTIVE route; this one is ${route.status}.` };
  } else if (parsed === undefined || !parsed.ok) {
    prepared = { ok: false, reason: raw === "" || parsed === undefined ? "Enter how many units to bridge." : parsed.error };
  } else {
    prepared = { ok: true, request: { accountId: own, routeId: route.id, quantity: parsed.value, mode } };
  }

  return (
    <section aria-label="Bridge units">
      <h3>Bridge units</h3>
      {own !== undefined && account.inspecting && (
        <p className="note">
          This form acts on your own account <code className="mono-small">{own}</code>, never on the account being
          inspected.
        </p>
      )}
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <div className="form-row">
          <label htmlFor={routeSelect}>Route</label>
          <select
            id={routeSelect}
            className="input"
            value={route?.id ?? ""}
            onChange={(event) => {
              setRouteId(event.target.value);
            }}
          >
            {props.routes.map((candidate) => (
              <option key={candidate.id} value={candidate.id}>
                {candidate.name}
              </option>
            ))}
          </select>
          {route !== undefined && (
            <p className="field-note">
              {route.source_symbol} on {route.source_chain} to {route.destination_symbol} on {route.destination_chain};{" "}
              <Pill tone={routeStatusTone(route.status)}>{route.status}</Pill>
            </p>
          )}
        </div>
        <div className="form-row">
          <label htmlFor={quantityId}>Units to bridge</label>
          <input
            id={quantityId}
            className="input"
            inputMode="decimal"
            autoComplete="off"
            value={raw}
            aria-describedby={helpId}
            {...(parsed === undefined || parsed.ok || raw === "" ? {} : { "aria-invalid": true })}
            onChange={(event) => {
              setRaw(event.target.value);
            }}
          />
          <p id={helpId} className="field-note">
            {route === undefined ? (
              "Choose a route first."
            ) : raw === "" || parsed === undefined ? (
              `Whole units of ${route.source_symbol}, with up to ${String(route.decimals)} decimal places, sent exactly as base units. They must be in your custodied wallet on ${route.source_chain}.`
            ) : parsed.ok ? (
              <>
                Will be sent as <BaseUnits value={parsed.value} />.
              </>
            ) : (
              parsed.error
            )}
          </p>
        </div>
        <div className="form-row">
          <label htmlFor={modeId}>Mode</label>
          <select
            id={modeId}
            className="input"
            value={mode}
            onChange={(event) => {
              const chosen = BRIDGE_LEG_MODES.find((candidate) => candidate === event.target.value);
              if (chosen !== undefined) setMode(chosen);
            }}
          >
            {BRIDGE_LEG_MODES.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
              </option>
            ))}
          </select>
          <p className="field-note">{REQUEST_MODE_MEANING[mode]}</p>
        </div>
      </form>
      <CommandFlow
        label="bridge"
        prepared={prepared}
        command={command}
        describe={(request: TransferRequest) => {
          const chosen = props.routes.find((candidate) => candidate.id === request.routeId);
          return (
            <p>
              Bridge{" "}
              {chosen === undefined ? (
                <BaseUnits value={request.quantity} />
              ) : (
                <>
                  <Qty value={request.quantity} decimals={chosen.decimals} symbol={chosen.source_symbol} /> (
                  <BaseUnits value={request.quantity} />) from {chosen.source_chain} to {chosen.destination_chain}
                </>
              )}{" "}
              for account <code className="mono-small">{request.accountId}</code>, in {request.mode} mode. Simulated value
              only.
            </p>
          );
        }}
        renderResult={(result: BridgeLegResult) => (
          <div role="status">
            <ReplayNote replay={result.replay} />
            {!result.settled && (
              <p className="note">The leg is recorded and not final yet: its state below says where it stands.</p>
            )}
            <LegProgress status={result.leg.status} />
            <LegFields leg={result.leg} />
            <p className="meta-row">
              <Link to={legHref(result.leg.id)}>This leg on its own page, with its transactions on both chains</Link>
              <Identifier value={result.leg.id} />
            </p>
          </div>
        )}
        refusedNote="The backend did not carry this out: nothing was reserved, signed or submitted. A refused request is kept on record as a rejected economic intent."
      />
    </section>
  );
}

/**
 * BRIDGE — operator: opening a route, pausing and resuming one, reconciling
 * a route's books with both chains, and settling a leg from both chains by
 * transaction id and reconciling it.
 *
 * These are sandbox:operate commands; opening, pausing, resuming and settling
 * a leg need a recent strong sign-in. A principal without the permission is
 * refused by the backend, and the page shows that refusal with its code — it
 * never hides a panel to pretend the question was not asked.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useBridgeRoutes,
  useCreateBridgeRoute,
  usePauseBridgeRoute,
  useReconcileBridgeLeg,
  useReconcileBridgeRoute,
  useResumeBridgeRoute,
  type BridgeLegResult,
  type BridgeRoute,
  type BridgeRouteList,
  type BridgeRouteReconciliation,
  type LegActionRequest,
  type RouteActionRequest,
  type RouteDefinitionRequest,
  type RouteReasonRequest,
} from "../../../api/advanced/bridge.ts";
import { useAssets } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { isCanonicalUuid } from "../../../lib/advanced.ts";
import { BRIDGE_CHAIN_IDS, chainIdOf, parseSymbol, routeStatusTone, type BridgeChainId } from "../../../lib/bridge.ts";
import { parsePositiveDecimal, parseReason, parseRegistryName, parseWholeNumber } from "../../../lib/stable.ts";
import { ScrollTable } from "../shared.tsx";
import { CommandFlow, simulatedAssets, type Prepared } from "../stable/parts.tsx";
import { BridgeNav, LegFields, LegProgress } from "./parts.tsx";
import { legHref } from "./paths.ts";

export function BridgeOperatorPage(): ReactNode {
  return (
    <Page
      title="Bridge operations"
      lead="Opening and pausing routes, reconciling a route's books with both chains, and settling a leg from both chains. The backend decides who may run them."
    >
      <SimulatedNotice />
      <BridgeNav />
      <p className="note">
        Every command here is an operator's (sandbox:operate). Opening, pausing and resuming a route and settling a leg also
        need a recent strong sign-in. A session without the permission is refused by the backend, and the refusal is shown
        with its code. Producing blocks and injecting faults are on the chains page.
      </p>
      <RouteActionsPanel />
      <ReconcileLegPanel />
      <CreateRoutePanel />
    </Page>
  );
}

function RouteActionsPanel(): ReactNode {
  const routes = useBridgeRoutes();
  return (
    <Panel title="Act on a route" description="Choose a route, then the action.">
      <AsyncPanel
        query={routes}
        loadingLabel="Loading the bridge routes…"
        empty={{
          isEmpty: (data: BridgeRouteList) => data.items.length === 0,
          title: "No bridge route exists",
          body: "Open one below first; there is nothing to act on yet.",
        }}
      >
        {(data: BridgeRouteList) => <RouteActions routes={data.items} />}
      </AsyncPanel>
    </Panel>
  );
}

function RouteActions(props: { readonly routes: readonly BridgeRoute[] }): ReactNode {
  const [routeId, setRouteId] = useState<string>(props.routes[0]?.id ?? "");
  const [raw, setRaw] = useState("");
  const selectId = useId();
  const reasonId = useId();
  const pause = usePauseBridgeRoute();
  const resume = useResumeBridgeRoute();
  const reconcile = useReconcileBridgeRoute();
  const route = props.routes.find((candidate) => candidate.id === routeId) ?? props.routes[0];
  if (route === undefined) return null;
  const reason = parseReason(raw);
  const withReason: Prepared<RouteReasonRequest> = reason.ok
    ? { ok: true, request: { routeId: route.id, reason: reason.value } }
    : { ok: false, reason: reason.error };
  const statusResult = (after: BridgeRoute): ReactNode => (
    <p role="status">
      {after.name} is now <Pill tone={routeStatusTone(after.status)}>{after.status}</Pill>
    </p>
  );
  return (
    <>
      <div className="form-row">
        <label htmlFor={selectId}>Route</label>
        <select
          id={selectId}
          className="input"
          value={route.id}
          onChange={(event) => {
            setRouteId(event.target.value);
          }}
        >
          {props.routes.map((candidate) => (
            <option key={candidate.id} value={candidate.id}>
              {candidate.name} ({candidate.status})
            </option>
          ))}
        </select>
        <p className="field-note">
          <Pill tone={routeStatusTone(route.status)}>{route.status}</Pill> {route.source_symbol} on {route.source_chain} to{" "}
          {route.destination_symbol} on {route.destination_chain}
        </p>
      </div>
      <section aria-label="Pause or resume the route">
        <h3>Pause or resume</h3>
        <div className="form-row">
          <label htmlFor={reasonId}>Reason</label>
          <input
            id={reasonId}
            className="input"
            autoComplete="off"
            value={raw}
            onChange={(event) => {
              setRaw(event.target.value);
            }}
          />
          <p className="field-note">Recorded with the change. Legs already in flight still complete on a paused route.</p>
        </div>
        <CommandFlow
          label={`pause ${route.name}`}
          prepared={withReason}
          command={pause}
          variant="danger"
          describe={(request: RouteReasonRequest) => (
            <p>
              Pause the route <code className="mono-small">{request.routeId}</code>, because “{request.reason}”.
            </p>
          )}
          renderResult={statusResult}
          refusedNote="The backend did not change the route's status."
        />
        <CommandFlow
          label={`resume ${route.name}`}
          prepared={withReason}
          command={resume}
          describe={(request: RouteReasonRequest) => (
            <p>
              Resume the route <code className="mono-small">{request.routeId}</code>, because “{request.reason}”.
            </p>
          )}
          renderResult={statusResult}
          refusedNote="The backend did not change the route's status."
        />
      </section>
      <section aria-label="Reconcile the route">
        <h3>Reconcile the route</h3>
        <p className="field-note">
          Compares the route's books with the source chain's escrow and the destination's supply. A mismatch opens a record
          that blocks new risk and pauses the route. Moves no value.
        </p>
        <CommandFlow
          label={`reconcile ${route.name}`}
          prepared={{ ok: true, request: { routeId: route.id } }}
          command={reconcile}
          describe={(request: RouteActionRequest) => (
            <p>
              Reconcile the route <code className="mono-small">{request.routeId}</code> with both chains.
            </p>
          )}
          renderResult={(rec: BridgeRouteReconciliation) => <RouteReconciliationView rec={rec} />}
          refusedNote="The backend did not reconcile; nothing was recorded."
        />
      </section>
    </>
  );
}

function RouteReconciliationView(props: { readonly rec: BridgeRouteReconciliation }): ReactNode {
  const { rec } = props;
  return (
    <div role="status" data-field="route-reconciliation">
      <p>
        {rec.matched ? <Pill tone="good">books and both chains agree</Pill> : <Pill tone="bad">mismatch</Pill>}
        {rec.breaker_tripped && <span className="cell-note">this run paused the route</span>}
      </p>
      <FieldGrid columns={3}>
        <Field label="Locked on the books" note="The facility's BRIDGE_LOCKED of the source asset.">
          <BaseUnits value={rec.locked} />
        </Field>
        <Field label="Outstanding on the books" note="The facility's BRIDGE_OUTSTANDING of the destination asset.">
          <BaseUnits value={rec.outstanding} />
        </Field>
        <Field label="In flight" note="Every customer's BRIDGE_IN_FLIGHT of the source asset.">
          <BaseUnits value={rec.in_flight} />
        </Field>
        <Field label="The books conserve" note="Locked equals outstanding plus in flight.">
          {rec.books_conserve ? <Pill tone="good">yes</Pill> : <Pill tone="bad">no</Pill>}
        </Field>
        <Field label="Source escrow on the chain" note="Against locked, plus refunded, plus locks that landed and are not posted.">
          <BaseUnits value={rec.escrow} />
          <span className="cell-note">
            expected <BaseUnits value={rec.expected_escrow} />
          </span>
        </Field>
        <Field label="Destination supply on the chain" note="Against outstanding, plus releases that landed and are not posted.">
          <BaseUnits value={rec.destination_supply} />
          <span className="cell-note">
            expected <BaseUnits value={rec.expected_supply} />
          </span>
        </Field>
        <Field label="Escrow agrees">{rec.escrow_matches ? <Pill tone="good">yes</Pill> : <Pill tone="bad">no</Pill>}</Field>
        <Field label="Supply agrees">{rec.supply_matches ? <Pill tone="good">yes</Pill> : <Pill tone="bad">no</Pill>}</Field>
        <Field label="Refunded">
          <BaseUnits value={rec.refunded} />
        </Field>
        <Field label="Reconciliation record">
          {rec.record_id === undefined ? <span className="absent">none recorded</span> : <Identifier value={rec.record_id} />}
        </Field>
      </FieldGrid>
    </div>
  );
}

function ReconcileLegPanel(): ReactNode {
  const command = useReconcileBridgeLeg();
  const [raw, setRaw] = useState("");
  const legId = useId();
  const text = raw.trim();
  const prepared: Prepared<LegActionRequest> = isCanonicalUuid(text)
    ? { ok: true, request: { legId: text } }
    : { ok: false, reason: text === "" ? "Enter the leg's id." : "A leg id is a UUID in its canonical form." };
  return (
    <Panel
      title="Settle and reconcile a leg"
      description="Drives a leg as far as the chains allow now — never resubmitting a lock, never racing a release that can still land; a SANDBOX leg is driven by producing blocks — and, once its destination is final, checks the lock, the release and both postings against the chains."
    >
      <div className="form-row">
        <label htmlFor={legId}>Leg id</label>
        <input
          id={legId}
          className="input"
          autoComplete="off"
          value={raw}
          onChange={(event) => {
            setRaw(event.target.value);
          }}
        />
      </div>
      <CommandFlow
        label="settle and reconcile the leg"
        prepared={prepared}
        command={command}
        describe={(request: LegActionRequest) => (
          <p>
            Settle the leg <code className="mono-small">{request.legId}</code> from both chains by transaction id and
            reconcile it.
          </p>
        )}
        renderResult={(result: BridgeLegResult) => (
          <div role="status">
            {!result.settled && <p className="note">The leg is not final yet: its state below says where it stands.</p>}
            <LegProgress status={result.leg.status} />
            <LegFields leg={result.leg} />
            <p className="meta-row">
              <Link to={legHref(result.leg.id)}>The leg on its own page</Link>
            </p>
          </div>
        )}
        refusedNote="The backend did not settle the leg."
      />
    </Panel>
  );
}

function CreateRoutePanel(): ReactNode {
  const registry = useAssets();
  const simulated = simulatedAssets(registry.data);
  const command = useCreateBridgeRoute();
  const [name, setName] = useState("");
  const [source, setSource] = useState<string>("");
  const [destination, setDestination] = useState<BridgeChainId>("sbx-ethereum");
  const [symbol, setSymbol] = useState("");
  const [priceRaw, setPriceRaw] = useState("1");
  const [attemptsRaw, setAttemptsRaw] = useState("3");
  const nameId = useId();
  const sourceId = useId();
  const destinationId = useId();
  const symbolId = useId();
  const priceId = useId();
  const attemptsId = useId();

  const chosenSource = source === "" ? simulated.find((asset) => asset.chain.startsWith("sbx-"))?.id ?? "" : source;
  const parsedName = parseRegistryName(name, "The route name");
  const parsedSymbol = parseSymbol(symbol);
  const price = parsePositiveDecimal(priceRaw, "The unit value");
  const attempts = parseWholeNumber(attemptsRaw, 1, 20, "Release attempts");

  let prepared: Prepared<RouteDefinitionRequest>;
  if (!parsedName.ok) prepared = { ok: false, reason: parsedName.error };
  else if (chosenSource === "") prepared = { ok: false, reason: "Choose the source asset: a simulated asset on a simulated chain." };
  else if (!parsedSymbol.ok) prepared = { ok: false, reason: parsedSymbol.error };
  else if (!price.ok) prepared = { ok: false, reason: price.error };
  else if (!attempts.ok) prepared = { ok: false, reason: attempts.error };
  else {
    prepared = {
      ok: true,
      request: {
        name: parsedName.value,
        sourceAssetId: chosenSource,
        destinationChain: destination,
        destinationSymbol: parsedSymbol.value,
        unitPrice: price.value,
        maxReleaseAttempts: attempts.value,
      },
    };
  }

  return (
    <Panel
      title="Open a route"
      description="From a simulated asset on its simulated chain to a bridged twin on another chain, whose mint no key controls: only an attested release mints it."
    >
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <div className="form-row">
          <label htmlFor={nameId}>Route name</label>
          <input id={nameId} className="input" autoComplete="off" value={name} onChange={(event) => { setName(event.target.value); }} />
          <p className="field-note">1 to 63 lowercase letters, digits and hyphens. Generic: never a public brand.</p>
        </div>
        <div className="form-row">
          <label htmlFor={sourceId}>Source asset</label>
          <select id={sourceId} className="input" value={chosenSource} onChange={(event) => { setSource(event.target.value); }}>
            {simulated.map((asset) => (
              <option key={asset.id} value={asset.id}>
                {asset.symbol} on {asset.chain}
              </option>
            ))}
          </select>
          <p className="field-note">It must live on a simulated chain whose devnet mint exists; the backend checks.</p>
        </div>
        <div className="form-row">
          <label htmlFor={destinationId}>Destination chain</label>
          <select
            id={destinationId}
            className="input"
            value={destination}
            onChange={(event) => {
              const chosen = chainIdOf(event.target.value);
              if (chosen !== undefined) setDestination(chosen);
            }}
          >
            {BRIDGE_CHAIN_IDS.map((chain) => (
              <option key={chain} value={chain}>
                {chain}
              </option>
            ))}
          </select>
        </div>
        <div className="form-row">
          <label htmlFor={symbolId}>Bridged twin's symbol</label>
          <input id={symbolId} className="input" autoComplete="off" value={symbol} onChange={(event) => { setSymbol(event.target.value); }} />
        </div>
        <div className="form-row">
          <label htmlFor={priceId}>Value of one whole unit, in USD</label>
          <input
            id={priceId}
            className="input"
            inputMode="decimal"
            autoComplete="off"
            value={priceRaw}
            onChange={(event) => {
              setPriceRaw(event.target.value);
            }}
          />
          <p className="field-note">Used for the risk notional and for buying-power marks; a simulated paper dollar is one. Sent exactly as typed.</p>
        </div>
        <div className="form-row">
          <label htmlFor={attemptsId}>Release attempts before manual review</label>
          <input
            id={attemptsId}
            className="input"
            inputMode="numeric"
            autoComplete="off"
            value={attemptsRaw}
            onChange={(event) => {
              setAttemptsRaw(event.target.value);
            }}
          />
        </div>
      </form>
      <CommandFlow
        label="open the route"
        prepared={prepared}
        command={command}
        describe={(request: RouteDefinitionRequest) => (
          <p>
            Open <code className="mono-small">{request.name}</code> from{" "}
            {simulated.find((asset) => asset.id === request.sourceAssetId)?.symbol ?? request.sourceAssetId} to{" "}
            {request.destinationSymbol} on <code className="mono-small">{request.destinationChain}</code>, one whole unit
            valued at {request.unitPrice} USD, at most {String(request.maxReleaseAttempts)} release attempts per round.
          </p>
        )}
        renderResult={(route: BridgeRoute) => (
          <div role="status">
            <ScrollTable caption="The route" headers={["Route", "Status", "Facility", "Destination mint"]}>
              <tr>
                <th scope="row">{route.name}</th>
                <td>
                  <Pill tone={routeStatusTone(route.status)}>{route.status}</Pill>
                </td>
                <td>
                  <Identifier value={route.facility_id} />
                </td>
                <td>
                  <Identifier value={route.destination_mint} />
                </td>
              </tr>
            </ScrollTable>
          </div>
        )}
        refusedNote="The backend opened no route."
      />
    </Panel>
  );
}

/**
 * INSTRUMENTS: the canonical instrument model (advanced goal §4).
 *
 * One instrument, one kind and one versioned terms document describe every
 * tradable thing: spot pairs, stable-value tokens, equities and ETFs, dated
 * and perpetual futures, options, prediction-market outcomes and synthetics.
 * Terms never change in place. Every change is a new immutable version with a
 * content hash, a reason and the kind of actor that made it, and the detail
 * page lists every one of them.
 *
 * Prices in terms are integers at the instrument's price scale, and quantities
 * integers at its quantity decimals. The page shows each as the decimal it
 * denotes — by moving the point, never by dividing — with the integer the
 * backend sent beside it.
 */
import { useId, type ReactNode } from "react";
import { Link, Route, Routes, useParams, useSearchParams } from "react-router-dom";

import {
  INSTRUMENT_LIST_LIMIT,
  useAdvancedInstrument,
  useAdvancedInstruments,
  type AdvancedInstrument,
  type AdvancedInstrumentDetail,
  type InstrumentTerms,
} from "../../../api/advanced/core.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  INSTRUMENT_KINDS,
  formatMillis,
  parseInstrumentKind,
  settlementStyleLabel,
} from "../../../lib/advanced.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref } from "../area.ts";
import { AdvancedNotFound, Codes, ScrollTable } from "../shared.tsx";

/** The section: the list, and one instrument by id. */
export function InstrumentsSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<InstrumentList />} />
      <Route path=":instrumentId" element={<InstrumentDetail />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}

function InstrumentList(): ReactNode {
  const [params, setParams] = useSearchParams();
  const filter = parseInstrumentKind(params.get("kind"));
  const instruments = useAdvancedInstruments(filter.kind);
  const selectId = useId();

  return (
    <Page
      title="Instruments"
      lead="The canonical instrument model: one row per instrument, with the version of its terms in force."
    >
      <SimulatedNotice />
      <Panel
        title="Canonical instruments"
        description="As the backend lists them. Choosing a kind asks the backend for that kind; nothing is filtered in the browser."
        actions={
          <div className="filter">
            <label htmlFor={selectId}>Instrument kind</label>
            <select
              id={selectId}
              className="input"
              value={filter.kind ?? ""}
              onChange={(event) => {
                const value = event.target.value;
                setParams(value === "" ? {} : { kind: value });
              }}
            >
              <option value="">All kinds</option>
              {INSTRUMENT_KINDS.map((kind) => (
                <option key={kind} value={kind}>
                  {kind}
                </option>
              ))}
            </select>
          </div>
        }
      >
        {filter.ignored !== undefined && (
          <p className="note" role="status">
            This address asked for the kind “{filter.ignored}”, which the contract does not declare, so no filter was
            sent and every kind is listed.
          </p>
        )}
        <AsyncPanel
          query={instruments}
          loadingLabel="Loading instruments…"
          empty={{
            isEmpty: (list: AdvancedInstrument[]) => list.length === 0,
            title: filter.kind === undefined ? "No instruments" : `No ${filter.kind} instruments`,
            body: "The backend returned no instruments for this request. That is its answer; nothing is listed in their place.",
          }}
        >
          {(list: AdvancedInstrument[]) => (
            <>
              <ScrollTable
                caption="Instruments in this list"
                headers={["Instrument", "Kind", "Symbol", "Venue", "Status", "Risk class", "Terms in force"]}
              >
                {list.map((instrument) => (
                  <tr key={instrument.id}>
                    <th scope="row">
                      <Link to={advancedHref("instruments", instrument.id)}>{instrument.canonical_name}</Link>
                    </th>
                    <td>
                      <Pill tone="neutral">{instrument.kind}</Pill>
                    </td>
                    <td>
                      <code className="mono-small">{instrument.terms.symbol}</code>
                    </td>
                    <td>
                      {instrument.terms.venue_code}
                      {instrument.terms.exchange_mic !== undefined && (
                        <span className="cell-note">MIC {instrument.terms.exchange_mic}</span>
                      )}
                    </td>
                    <td>{instrument.status}</td>
                    <td>{instrument.risk_class}</td>
                    <td>
                      version {String(instrument.terms_version)}
                      <span className="cell-note">
                        <code className="mono-small">{instrument.terms_hash}</code>
                      </span>
                    </td>
                  </tr>
                ))}
              </ScrollTable>
              <p className="note">
                {list.length === INSTRUMENT_LIST_LIMIT
                  ? `The backend returned ${String(list.length)} instruments, the most the API sends in one answer. More may exist; this list has no cursor to reach them.`
                  : `${String(list.length)} ${list.length === 1 ? "instrument" : "instruments"} returned by the backend.`}
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function InstrumentDetail(): ReactNode {
  const { instrumentId } = useParams();
  const instrument = useAdvancedInstrument(instrumentId);

  return (
    <Page
      title={instrument.isSuccess ? instrument.data.canonical_name : "Instrument"}
      lead="One instrument: the terms in force, and every version its terms have had."
      actions={<LinkButton to={advancedHref("instruments")}>All instruments</LinkButton>}
    >
      <SimulatedNotice />
      {instrument.isSuccess ? (
        <InstrumentPanels data={instrument.data} />
      ) : (
        <Panel title="Instrument">
          <AsyncPanel query={instrument} loadingLabel="Loading the instrument…">
            {() => null}
          </AsyncPanel>
        </Panel>
      )}
    </Page>
  );
}

function InstrumentPanels(props: { readonly data: AdvancedInstrumentDetail }): ReactNode {
  const { data } = props;
  return (
    <>
      <Panel title="Instrument" description="Its identity, and which version of its terms is in force.">
        <FieldGrid columns={3}>
          <Field label="Instrument">
            <Identifier value={data.id} />
          </Field>
          <Field label="Kind">
            <Pill tone="neutral">{data.kind}</Pill>
          </Field>
          <Field label="Status">{data.status}</Field>
          <Field label="Risk class">{data.risk_class}</Field>
          <Field label="Terms in force">version {String(data.terms_version)}</Field>
          <Field label="Terms hash" note="SHA-256 of the canonical terms; any change to them is a new version with a new hash.">
            <Identifier value={data.terms_hash} />
          </Field>
        </FieldGrid>
      </Panel>

      <Panel title="Terms in force" description={`Version ${String(data.terms_version)}, exactly as the backend holds it.`}>
        <TermsFields terms={data.terms} />
      </Panel>

      <Panel
        title="Terms history"
        description="Every version of the terms, oldest first. Versions are immutable: a change is a new row, never an edit."
      >
        {data.versions.length === 0 ? (
          <EmptyState title="No versions returned" body="The backend returned no terms history for this instrument." />
        ) : (
          <ScrollTable caption="Terms versions" headers={["Version", "Terms hash", "Reason", "Actor", "Recorded"]}>
            {data.versions.map((version) => (
              <tr key={version.version}>
                <th scope="row">
                  version {String(version.version)}
                  {version.version === data.terms_version && (
                    <>
                      {" "}
                      <Pill tone="good">in force</Pill>
                    </>
                  )}
                </th>
                <td>
                  <code className="mono-small">{version.terms_hash}</code>
                </td>
                <td className="cell-prose">{version.reason}</td>
                <td>
                  {version.actor_type}
                  <span className="cell-note mono-small">{version.actor_id}</span>
                </td>
                <td>
                  {formatInstant(version.created_at)}
                  <span className="cell-note">{exactInstant(version.created_at)}</span>
                </td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </Panel>
    </>
  );
}

/** An integer the backend sent at a scale, as the decimal it denotes and as itself. */
function Scaled(props: { readonly value: string; readonly scale: number; readonly what: string }): ReactNode {
  return (
    <>
      <Qty value={props.value} decimals={props.scale} />
      <span className="cell-note">
        integer {props.value} at {props.what} {String(props.scale)}
      </span>
    </>
  );
}

function None(props: { readonly what: string }): ReactNode {
  return <span className="absent">{props.what}</span>;
}

function TermsFields(props: { readonly terms: InstrumentTerms }): ReactNode {
  const { terms } = props;
  return (
    <>
      <h3>Identity</h3>
      <FieldGrid columns={3}>
        <Field label="Kind">{terms.kind}</Field>
        <Field label="Symbol">
          <code className="mono-small">{terms.symbol}</code>
        </Field>
        <Field label="Venue">{terms.venue_code}</Field>
        <Field label="Exchange MIC">{terms.exchange_mic ?? <None what="none" />}</Field>
        <Field label="Trading calendar">{terms.trading_calendar}</Field>
      </FieldGrid>

      <h3>Assets</h3>
      <FieldGrid columns={2}>
        <Field label="Settlement asset">
          <Identifier value={terms.settlement_asset_id} />
        </Field>
        <Field label="Base asset">
          {terms.base_asset_id === undefined ? <None what="none" /> : <Identifier value={terms.base_asset_id} />}
        </Field>
        <Field label="Quote asset">
          {terms.quote_asset_id === undefined ? <None what="none" /> : <Identifier value={terms.quote_asset_id} />}
        </Field>
        <Field label="Underlying instrument">
          {terms.underlying_instrument_id === undefined ? (
            <None what="none" />
          ) : (
            <Link to={advancedHref("instruments", terms.underlying_instrument_id)}>
              <code className="mono-small">{terms.underlying_instrument_id}</code>
            </Link>
          )}
        </Field>
      </FieldGrid>

      <h3>Prices and quantities</h3>
      <FieldGrid columns={3}>
        <Field label="Price scale" note="Prices are integers at this many decimal places.">
          <span className="num">{String(terms.price_scale)}</span>
        </Field>
        <Field label="Tick size" note="The smallest price step.">
          <Scaled value={terms.tick_size} scale={terms.price_scale} what="price scale" />
        </Field>
        <Field label="Quantity decimals" note="Quantities are integers at this many decimal places.">
          <span className="num">{String(terms.quantity_decimals)}</span>
        </Field>
        <Field label="Lot size" note="Every quantity is a whole number of lots.">
          <Scaled value={terms.lot_size} scale={terms.quantity_decimals} what="quantity decimals" />
        </Field>
        <Field label="Minimum quantity">
          <Scaled value={terms.min_quantity} scale={terms.quantity_decimals} what="quantity decimals" />
        </Field>
        <Field label="Contract multiplier" note="Base units of the underlying per contract; 1 for spot.">
          <BaseUnits value={terms.contract_multiplier} />
        </Field>
      </FieldGrid>

      <h3>Contract</h3>
      <FieldGrid columns={3}>
        <Field label="Expiry">
          {terms.expiry === undefined ? (
            <None what="does not expire" />
          ) : (
            <>
              {formatInstant(terms.expiry)}
              <span className="cell-note">{exactInstant(terms.expiry)}</span>
            </>
          )}
        </Field>
        <Field label="Strike">
          {terms.strike_mantissa === undefined ? (
            <None what="no strike" />
          ) : (
            <Scaled value={terms.strike_mantissa} scale={terms.price_scale} what="price scale" />
          )}
        </Field>
        <Field label="Option right">{terms.option_right ?? <None what="not an option" />}</Field>
        <Field label="Exercise style">{terms.exercise_style ?? <None what="not an option" />}</Field>
        <Field label="Funding interval">
          {terms.funding_interval_ms === undefined ? (
            <None what="no funding" />
          ) : (
            <>
              {formatMillis(terms.funding_interval_ms)}
              <span className="cell-note">{String(terms.funding_interval_ms)} ms</span>
            </>
          )}
        </Field>
        <Field label="Outcomes">
          <Codes codes={terms.outcomes ?? []} none="no outcome set" />
        </Field>
      </FieldGrid>

      <h3>Settlement and risk</h3>
      <FieldGrid columns={3}>
        <Field label="Settlement">{settlementStyleLabel(terms.settlement_style)}</Field>
        <Field label="Margin model">{terms.margin_model}</Field>
        <Field label="Corporate actions">{terms.corporate_action_behavior}</Field>
        <Field label="Transferability">{terms.transferability}</Field>
        <Field label="Jurisdiction tags">
          <Codes codes={terms.jurisdiction_tags} none="none" />
        </Field>
      </FieldGrid>

      <h3>Oracle</h3>
      <FieldGrid columns={2}>
        <Field label="Oracle sources">
          <Codes codes={terms.oracle_sources} none="none" />
        </Field>
        <Field label="Oldest usable mark" note="A mark older than this is stale for this instrument.">
          {formatMillis(terms.oracle_max_age_ms)}
          <span className="cell-note">{String(terms.oracle_max_age_ms)} ms</span>
        </Field>
      </FieldGrid>
    </>
  );
}

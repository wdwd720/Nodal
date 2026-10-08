/**
 * DEFINING A CONTRACT (operator): a dated future, a perpetual or an option,
 * through the derivatives service (POST /advanced/derivatives/contracts, which
 * asks for a recent step-up).
 *
 * The form shows exactly what will be sent. The backend fixes the rest of the
 * terms -- the contract trades 24x7 on the sandbox venue, settles by payment
 * in its settlement asset, never by delivery, and is margined by its kind's
 * model -- and refuses a contract it could not settle exactly, or whose symbol
 * another contract has; that refusal, with its code, is what the form shows
 * when the contract is not defined. A defined contract trades only once an
 * operator records its first mark.
 */
import { useId, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { useDefineDerivativeContract } from "../../../api/advanced/derivatives.ts";
import { useInstruments, type Instrument } from "../../../api/queries.ts";
import { EmptyState } from "../../../components/DataState.tsx";
import { Disclosure, Field, FieldGrid } from "../../../components/Layout.tsx";
import {
  CONTRACT_RISK_CLASSES,
  DERIVATIVE_KINDS,
  EXERCISE_STYLES,
  KIND_LABEL,
  OPTION_RIGHTS,
  buildContractDefinition,
  type ContractRiskClass,
  type DerivativeKind,
  type ExerciseStyle,
  type OptionRight,
} from "../../../lib/derivatives.ts";
import { ActionKey, ChoiceField, CommandFailure, ConfirmButton, TextField, derivativesHref, useActionKey } from "./parts.tsx";

/** Instruments a contract can be on: those that name an underlying asset and are not themselves derivatives. */
function underlyingCandidates(list: readonly Instrument[]): Instrument[] {
  return list.filter(
    (instrument) => instrument.base_asset !== undefined && !(DERIVATIVE_KINDS as readonly string[]).includes(instrument.type),
  );
}

export function DefineContractForm(): ReactNode {
  const instruments = useInstruments();
  const candidates = useMemo(() => underlyingCandidates(instruments.data ?? []), [instruments.data]);
  const ids = {
    kind: useId(),
    symbol: useId(),
    name: useId(),
    underlying: useId(),
    settlement: useId(),
    multiplier: useId(),
    scale: useId(),
    tick: useId(),
    lot: useId(),
    minimum: useId(),
    expiry: useId(),
    funding: useId(),
    strike: useId(),
    right: useId(),
    style: useId(),
    oracle: useId(),
    sources: useId(),
    risk: useId(),
  };
  const [kind, setKind] = useState<DerivativeKind>("PERPETUAL");
  const [symbol, setSymbol] = useState("");
  const [name, setName] = useState("");
  const [underlying, setUnderlying] = useState("");
  const [settlement, setSettlement] = useState("");
  const [multiplier, setMultiplier] = useState("");
  const [scale, setScale] = useState("2");
  const [tick, setTick] = useState("");
  const [lot, setLot] = useState("1");
  const [minimum, setMinimum] = useState("1");
  const [expiry, setExpiry] = useState("");
  const [fundingHours, setFundingHours] = useState("8");
  const [strike, setStrike] = useState("");
  const [right, setRight] = useState<OptionRight>("CALL");
  const [style, setStyle] = useState<ExerciseStyle>("EUROPEAN");
  const [oracleSeconds, setOracleSeconds] = useState("60");
  const [sources, setSources] = useState("sandbox-operator");
  const [risk, setRisk] = useState<ContractRiskClass>("SPECULATIVE");
  const action = useActionKey();
  const define = useDefineDerivativeContract();

  const chosen = underlying !== "" ? underlying : (candidates[0]?.id ?? "");
  const built = buildContractDefinition({
    kind,
    symbol,
    name,
    underlyingId: chosen,
    settlementId: settlement,
    multiplier,
    priceScale: scale,
    tick,
    lot,
    minimum,
    expiry,
    fundingHours,
    strike,
    right,
    style,
    oracleSeconds,
    sources,
    risk,
  });
  const blocked = built.ok ? undefined : built.error;

  /** Any change is a different definition: it needs its own confirmation and key. */
  function changed<T>(set: (value: T) => void): (value: T) => void {
    return (value: T) => {
      set(value);
      action.reset();
      define.reset();
    };
  }

  if (instruments.isSuccess && candidates.length === 0) {
    return (
      <EmptyState
        title="No instrument to define a contract on"
        body="The backend lists no instrument that names an underlying asset, so there is nothing a contract could be on."
      />
    );
  }

  return (
    <>
      <div className="form">
        <ChoiceField
          id={ids.kind}
          label="Kind"
          value={kind}
          choices={DERIVATIVE_KINDS}
          describe={(value) => KIND_LABEL[value]}
          onChange={changed(setKind)}
        />
        <TextField
          id={ids.symbol}
          label="Symbol"
          value={symbol}
          hint="Letters and digits, optionally with . _ / -; no other contract may have it."
          onChange={changed(setSymbol)}
        />
        <TextField id={ids.name} label="Name (optional)" value={name} hint="The contract is named by its symbol without one." onChange={changed(setName)} />
        <div className="form-row">
          <label htmlFor={ids.underlying}>Underlying instrument</label>
          <select
            id={ids.underlying}
            className="input"
            value={chosen}
            onChange={(event) => {
              changed(setUnderlying)(event.target.value);
            }}
          >
            {candidates.map((instrument) => (
              <option key={instrument.id} value={instrument.id}>
                {instrument.canonical_name} ({instrument.type})
              </option>
            ))}
          </select>
          <span className="field-note">The contract is on this instrument's base asset; its price is what the contract tracks.</span>
        </div>
        <TextField
          id={ids.settlement}
          label="Settlement asset id (optional)"
          value={settlement}
          hint="Empty settles the contract in the sandbox dollar."
          onChange={changed(setSettlement)}
        />
        <TextField
          id={ids.multiplier}
          label="Contract size (base units of the underlying)"
          value={multiplier}
          hint="How much of the underlying one contract is, in its base units."
          onChange={changed(setMultiplier)}
        />
        <TextField id={ids.scale} label="Price scale" value={scale} hint="Decimal places of a price." onChange={changed(setScale)} />
        <TextField
          id={ids.tick}
          label="Tick"
          value={tick}
          hint="The smallest price step, as a price no more precise than the price scale."
          onChange={changed(setTick)}
        />
        <TextField id={ids.lot} label="Lot size (contracts)" value={lot} onChange={changed(setLot)} />
        <TextField id={ids.minimum} label="Minimum quantity (contracts)" value={minimum} onChange={changed(setMinimum)} />
        {kind === "PERPETUAL" ? (
          <TextField
            id={ids.funding}
            label="Funding interval (hours)"
            value={fundingHours}
            hint="How often funding is charged between longs and shorts."
            onChange={changed(setFundingHours)}
          />
        ) : (
          <TextField
            id={ids.expiry}
            label="Expiry (UTC)"
            value={expiry}
            placeholder="2026-12-31T16:00:00Z"
            hint="An instant in UTC after now. The contract settles by the settlement rule at expiry."
            onChange={changed(setExpiry)}
          />
        )}
        {kind === "OPTION" && (
          <>
            <TextField
              id={ids.strike}
              label="Strike"
              value={strike}
              hint="A price no more precise than the price scale, on the tick grid."
              onChange={changed(setStrike)}
            />
            <ChoiceField id={ids.right} label="Right" value={right} choices={OPTION_RIGHTS} onChange={changed(setRight)} />
            <ChoiceField id={ids.style} label="Exercise style" value={style} choices={EXERCISE_STYLES} onChange={changed(setStyle)} />
          </>
        )}
        <TextField
          id={ids.oracle}
          label="Mark maximum age (seconds)"
          value={oracleSeconds}
          hint="A mark older than this prices nothing: no open, no close, no run."
          onChange={changed(setOracleSeconds)}
        />
        <TextField id={ids.sources} label="Oracle sources" value={sources} hint="Comma-separated." onChange={changed(setSources)} />
        <ChoiceField id={ids.risk} label="Risk class" value={risk} choices={CONTRACT_RISK_CLASSES} onChange={changed(setRisk)} />
      </div>
      <FieldGrid columns={2}>
        <Field label="Will be sent" note="Prices as integer mantissas at the price scale, sizes as whole base units.">
          {built.ok ? (
            <code className="mono-small" data-field="definition">
              {JSON.stringify(built.body)}
            </code>
          ) : (
            <span className="absent">nothing yet: {built.error}</span>
          )}
        </Field>
        <ActionKey actionKey={action.key} />
      </FieldGrid>
      <Disclosure title="What defining a contract does">
        <p>
          It creates the contract with the terms above, as simulated value only. The backend fixes the rest: the contract trades
          around the clock on the sandbox venue, settles by payment in its settlement asset rather than by delivery, and is
          margined by its kind's model. It refuses a contract it could not settle exactly, or whose symbol another contract has,
          and writes nothing. Nothing trades in a new contract until an operator records its first mark. This needs a recent
          step-up.
        </p>
      </Disclosure>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={define.isPending}
          busyLabel="Defining…"
          onConfirm={() => {
            if (!built.ok) return;
            define.mutate({ definition: built.body, idempotencyKey: action.confirm() });
          }}
        >
          Confirm: define the contract
        </ConfirmButton>
      </div>
      {define.isError && <CommandFailure error={define.error} />}
      {define.isSuccess && (
        <p className="callout" role="status">
          Defined <Link to={derivativesHref("contracts", define.data.id)}>{define.data.terms.symbol}</Link> (
          {KIND_LABEL[define.data.kind]}, {define.data.status}), settling in {define.data.settlement_asset.symbol}. It trades
          once an operator records its first mark.
        </p>
      )}
    </>
  );
}

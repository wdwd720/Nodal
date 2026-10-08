/**
 * TREASURY — house-versus-customer crossing rules and what customers are told.
 *
 * The order book refuses a trade between a house book and a customer by
 * default. Changing that is the operator's, with a recent strong sign-in, and
 * it is always disclosed: the rule version is recorded with a customer-facing
 * disclosure that must say the activity is simulated. The disclosures of an
 * instrument's book are readable by customers as well as operators.
 */
import { useState, type ReactNode } from "react";

import {
  useCrossingDisclosures,
  useSetCrossingRule,
  type TreasuryCrossingDisclosure,
  type TreasuryCrossingDisclosureList,
  type TreasuryCrossingRuleRequest,
} from "../../../api/advanced/treasury.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { parseCrossingDisclosure, parseText, parseUuid } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { ScrollTable } from "../shared.tsx";
import { Choice, CommandFlow, CommandForm, TextArea, TextInput, prepare } from "./command.tsx";
import { TreasuryNav } from "./parts.tsx";

export function CrossingPage(): ReactNode {
  return (
    <Page
      title="House crossing rules"
      lead="Whether house books may trade with customers on an instrument's book, and the disclosure customers are given for every version of that rule."
    >
      <SimulatedNotice />
      <TreasuryNav />
      <CrossingRulePanel />
      <CrossingDisclosuresPanel />
    </Page>
  );
}

const ALLOW_CHOICES = ["refuse", "allow"] as const;
type AllowChoice = (typeof ALLOW_CHOICES)[number];

function CrossingRulePanel(): ReactNode {
  const setRule = useSetCrossingRule();
  const [instrument, setInstrument] = useState("");
  const [allow, setAllow] = useState<AllowChoice>("refuse");
  const [disclosure, setDisclosure] = useState("");
  const [reason, setReason] = useState("");

  const i = parseUuid(instrument, "The instrument id");
  const d = parseCrossingDisclosure(disclosure);
  const r = parseText(reason, "The reason", 500);
  const prepared = prepare<TreasuryCrossingRuleRequest>(
    [i.ok ? undefined : i.error, d.ok ? undefined : d.error, r.ok ? undefined : r.error],
    () => ({
      instrument_id: i.ok ? i.value : "",
      allow: allow === "allow",
      disclosure: d.ok ? d.value : "",
      reason: r.ok ? r.value : "",
    }),
  );

  return (
    <Panel
      title="Change a crossing rule"
      description="Records the next version of an instrument book's crossing rule together with its customer disclosure, archived as evidence. It changes a rule customers trade under, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Change a crossing rule">
        <TextInput id="treasury-crossing-instrument" label="Instrument id" value={instrument} onChange={setInstrument} />
        <Choice
          id="treasury-crossing-allow"
          label="House books trading against customers"
          value={allow}
          options={ALLOW_CHOICES}
          onChange={setAllow}
          describe={(value) => (value === "allow" ? "allowed, under this disclosure" : "refused (the default)")}
        />
        <TextArea
          id="treasury-crossing-disclosure"
          label="Disclosure customers are given"
          value={disclosure}
          onChange={setDisclosure}
          rows={4}
          note="20 to 2000 characters, and it must say the activity is simulated."
        />
        <TextInput id="treasury-crossing-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<TreasuryCrossingRuleRequest, TreasuryCrossingDisclosure>
          label="Record the crossing rule"
          variant="danger"
          prepared={prepared}
          command={setRule}
          refusedNote="The crossing rule is unchanged."
          describe={(request) => (
            <FieldGrid columns={2}>
              <Field label="Instrument">
                <Identifier value={request.instrument_id} />
              </Field>
              <Field label="House books trading against customers">{request.allow ? "allowed" : "refused"}</Field>
              <Field label="Disclosure">{request.disclosure}</Field>
              <Field label="Reason">{request.reason}</Field>
            </FieldGrid>
          )}
          renderResult={(result) => <DisclosureRows items={[result]} />}
        />
      </CommandForm>
    </Panel>
  );
}

function DisclosureRows(props: { readonly items: readonly TreasuryCrossingDisclosure[] }): ReactNode {
  return (
    <ScrollTable caption="Crossing disclosures" headers={["Rule version", "Setting", "What customers are told", "Reason", "Recorded by", "Evidence"]}>
      {props.items.map((item) => (
        <tr key={item.id} data-rule-version={String(item.rule_version)}>
          <th scope="row">{String(item.rule_version)}</th>
          <td>{item.allow ? <Pill tone="warn">house may trade against customers</Pill> : <Pill tone="good">refused</Pill>}</td>
          <td className="cell-prose">{item.disclosure}</td>
          <td className="cell-prose">{item.reason}</td>
          <td>
            {item.actor_type} <code className="mono-small">{item.actor_id}</code>
            <span className="cell-note">{formatInstant(item.disclosed_at)}</span>
          </td>
          <td>
            <code className="mono-small">{item.evidence_ref}</code>
            <span className="cell-note mono-small">{item.evidence_sha256}</span>
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

/** Every crossing disclosure of one instrument's book, for whoever asks: customers and operators alike. */
export function CrossingDisclosuresPanel(props: { readonly initialInstrument?: string }): ReactNode {
  const [draft, setDraft] = useState(props.initialInstrument ?? "");
  const [asked, setAsked] = useState<string | undefined>(props.initialInstrument);
  const disclosures = useCrossingDisclosures(asked);
  const parsed = parseUuid(draft, "The instrument id");
  return (
    <Panel
      title="Crossing disclosures"
      description="What customers are told about house books trading against them on one instrument's book, every rule version, oldest first."
    >
      <CommandForm label="Read an instrument's crossing disclosures">
        <TextInput id="treasury-disclosures-instrument" label="Instrument id" value={draft} onChange={setDraft} />
        <div className="form-actions">
          {parsed.ok ? (
            <Button
              onClick={() => {
                setAsked(parsed.value);
              }}
            >
              Show the disclosures
            </Button>
          ) : (
            <Button disabledReason={parsed.error}>Show the disclosures</Button>
          )}
        </div>
      </CommandForm>
      {asked !== undefined && (
        <AsyncPanel
          query={disclosures}
          loadingLabel="Loading the crossing disclosures…"
          empty={{
            isEmpty: (data: TreasuryCrossingDisclosureList) => data.items.length === 0,
            title: "No crossing rule was ever changed on this book",
            body: "House books trading against customers is refused there by default, and the backend has no disclosure to show.",
          }}
        >
          {(data: TreasuryCrossingDisclosureList) => <DisclosureRows items={data.items} />}
        </AsyncPanel>
      )}
    </Panel>
  );
}

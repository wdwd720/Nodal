/**
 * INSTALLS AND PAPER-FOLLOW, for the account the area is reading.
 *
 * An install adds an approved strategy to an account in PAPER mode, the only
 * mode an install can have, with a paper book that starts with the paper
 * capital the backend states. Following it runs the strategy in the sandbox,
 * now, on an input the backend builds itself -- the install's paper book and
 * the reference prices it reads from the price store -- so the page sends
 * nothing about the market or the portfolio. Every proposal the manifest
 * permits becomes an economic intent that faces routing and Risk Kernel V2,
 * and only an ALLOW reaches paper execution. The history below lists every
 * follow run with each intent it produced and the risk decision about it.
 *
 * The paper balance, equity, turnover and performance are PAPER results
 * computed by the backend from the install's own fills at reference prices;
 * they are not a record of what this account earned.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useFollowInstall,
  useRecordPerformance,
  useRemoveInstall,
  useStrategyFollows,
  useStrategyInstalls,
  type FollowInput,
  type InstallActionInput,
  type StrategyFollowRun,
  type StrategyInstall,
} from "../../../api/advanced/marketplace.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { Button, LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel, Explanation } from "../../../components/DataState.tsx";
import { Disclosure, Field, FieldGrid, Identifier, Page, Panel, Pill, Table } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, verdictTone, type CursorTrail } from "../../../lib/advanced.ts";
import { SIMULATED_RESULTS_NOTICE } from "../../../lib/honesty.ts";
import { FOLLOW_OUTCOMES, OUTCOME_MEANING, outcomeTone } from "../../../lib/marketplace.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { Codes, CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import {
  BasisPoints,
  OutcomePill,
  SimulatedAmount,
  TerminationPill,
  VersionState,
  strategyHref,
  usePrepared,
} from "./parts.tsx";

export function InstallsPage(): ReactNode {
  const { accountId, ownAccountId } = useAdvancedAccount();
  const [selected, setSelected] = useState<StrategyInstall | undefined>(undefined);
  return (
    <Page
      title="Installs and paper-follow"
      lead="The strategies this account installed, in PAPER mode only, and every paper-follow run with the intents it produced and the Risk Kernel V2 decision about each."
      actions={<LinkButton to={strategyHref.home()}>Back to strategies</LinkButton>}
    >
      <SimulatedNotice />
      <Disclosure title="Paper results">
        <p>{SIMULATED_RESULTS_NOTICE}</p>
        <p>
          The paper balance, equity, turnover and performance come from each install&apos;s paper book: the paper capital it
          started with, moved by its own fills, valued at reference prices the backend reads itself. Nobody supplies a
          snapshot, and the figures are before the paper venue&apos;s disclosed charges.
        </p>
      </Disclosure>
      {accountId === undefined ? (
        <Panel title="Your installs">
          <NoAccount />
        </Panel>
      ) : (
        <>
          <InstallsPanel
            key={`installs-${accountId}`}
            accountId={accountId}
            canAct={accountId === ownAccountId}
            onFollow={setSelected}
          />
          {selected !== undefined && accountId === ownAccountId && (
            <FollowPanel
              key={selected.id}
              install={selected}
              accountId={accountId}
              onClose={() => {
                setSelected(undefined);
              }}
            />
          )}
          <FollowHistory key={`follows-${accountId}-${selected?.id ?? ""}`} accountId={accountId} install={selected} />
          <OutcomeGlossary />
        </>
      )}
    </Page>
  );
}

function InstallsPanel(props: {
  readonly accountId: string;
  readonly canAct: boolean;
  readonly onFollow: (install: StrategyInstall) => void;
}): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const installs = useStrategyInstalls(props.accountId, trail.current);
  return (
    <Panel
      title="Your installs"
      description="Each install with the version it installed and its latest recorded paper performance. Actions act for this account only; an operator inspecting it can read, not act."
    >
      <AsyncPanel
        query={installs}
        loadingLabel="Loading installs…"
        empty={{
          isEmpty: (page: CursorPage<StrategyInstall>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No installs",
          body: "This account has installed no strategy. Approved strategies can be installed from their listing.",
        }}
      >
        {(page: CursorPage<StrategyInstall>) => (
          <>
            <ScrollTable
              caption="Installs on this page"
              headers={["Strategy", "Mode", "Install", "Version", "Latest paper performance", "Installed", "Actions"]}
            >
              {page.items.map((install) => (
                <tr key={install.id}>
                  <th scope="row">
                    <Link to={strategyHref.listing(install.listing_id)}>{install.listing_title}</Link>
                    <span className="cell-note">
                      <code className="mono-small">{install.strategy_ref}</code>
                    </span>
                  </th>
                  <td>
                    <Pill tone="info">{install.mode}</Pill>
                  </td>
                  <td>
                    <Pill tone={install.status === "ACTIVE" ? "good" : "neutral"}>{install.status}</Pill>
                    <span className="cell-note">
                      <code className="mono-small">{install.id}</code>
                    </span>
                  </td>
                  <td>
                    version {String(install.version_number)}
                    <span className="cell-note">
                      <VersionState status={install.version_status} moderation={install.version_moderation} />
                    </span>
                    <span className="cell-note">
                      paper capital <SimulatedAmount value={install.paper_capital} />
                    </span>
                  </td>
                  <td>
                    {install.latest_performance === undefined ? (
                      <span className="absent">none recorded</span>
                    ) : (
                      <>
                        return <BasisPoints value={install.latest_performance.return_bps} />
                        <span className="cell-note">
                          largest drawdown <BasisPoints value={install.latest_performance.max_drawdown_bps} />
                        </span>
                        <span className="cell-note">
                          over {String(install.latest_performance.follow_runs_count)} follow runs
                        </span>
                      </>
                    )}
                  </td>
                  <td>{formatInstant(install.created_at)}</td>
                  <td>
                    {props.canAct ? (
                      <InstallActions install={install} accountId={props.accountId} onFollow={props.onFollow} />
                    ) : (
                      <span className="absent">read only</span>
                    )}
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="installs"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function InstallActions(props: {
  readonly install: StrategyInstall;
  readonly accountId: string;
  readonly onFollow: (install: StrategyInstall) => void;
}): ReactNode {
  const { install } = props;
  const record = useRecordPerformance();
  const remove = useRemoveInstall();
  const recordAction = usePrepared<InstallActionInput>();
  const removeAction = usePrepared<InstallActionInput & { readonly reason: string }>();
  if (install.status !== "ACTIVE") {
    return <span className="absent">removed</span>;
  }
  const base = { accountId: props.accountId, installId: install.id };
  return (
    <div className="form-actions">
      <Button
        onClick={() => {
          props.onFollow(install);
        }}
      >
        Paper-follow
      </Button>
      <Button
        busy={record.isPending}
        busyLabel="Recording…"
        onClick={() => {
          record.mutate(recordAction.prepared ?? recordAction.prepare((idempotencyKey) => ({ ...base, idempotencyKey })));
        }}
      >
        Record performance
      </Button>
      <Button
        variant="danger"
        busy={remove.isPending}
        busyLabel="Removing…"
        onClick={() => {
          remove.mutate(
            removeAction.prepared ??
              removeAction.prepare((idempotencyKey) => ({ ...base, reason: "removed by the account owner", idempotencyKey })),
          );
        }}
      >
        Remove
      </Button>
      {record.isSuccess && (
        <p className="note" role="status">
          Recorded: over {String(record.data.follow_runs_count)} follow runs, return{" "}
          <BasisPoints value={record.data.return_bps} />. A paper result.
        </p>
      )}
      {record.isError && <Explanation error={record.error} />}
      {remove.isError && <Explanation error={remove.error} />}
    </div>
  );
}

function FollowPanel(props: {
  readonly install: StrategyInstall;
  readonly accountId: string;
  readonly onClose: () => void;
}): ReactNode {
  const follow = useFollowInstall();
  const action = usePrepared<FollowInput>();
  return (
    <Panel
      title={`Paper-follow ${props.install.listing_title}`}
      description={`Runs ${props.install.strategy_ref} once, now, on the input the backend builds itself.`}
      actions={
        <Button variant="quiet" onClick={props.onClose}>
          Close
        </Button>
      }
    >
      <form
        className="form"
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          follow.mutate(
            action.prepared ??
              action.prepare((idempotencyKey) => ({ accountId: props.accountId, installId: props.install.id, idempotencyKey })),
          );
        }}
      >
        <p className="note">
          The strategy sees the install&apos;s paper book -- its paper capital moved by its own fills -- and, for every
          instrument it names, the freshest reference price the backend holds (no older than five minutes) with its
          recent history. Nothing you type reaches it. Every proposal is valued at the reference price, never at a price
          the strategy chose; an allowed one fills on the paper venue at the mark, with its spread and fees disclosed on
          the fill, and only if this account&apos;s wallet covers it. The manifest&apos;s interval bounds how often an
          install can be followed, and each run counts against the account&apos;s sandbox budget.
        </p>
        <div className="form-actions">
          <Button variant="primary" submit busy={follow.isPending} busyLabel="Running the strategy…">
            {action.prepared === undefined ? "Follow now" : "Retry the same follow"}
          </Button>
        </div>
        {follow.isError && <Explanation error={follow.error} />}
      </form>
      {follow.isSuccess && <FollowRunView run={follow.data} />}
    </Panel>
  );
}

function FollowHistory(props: { readonly accountId: string; readonly install: StrategyInstall | undefined }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const follows = useStrategyFollows(props.accountId, props.install?.id, trail.current);
  return (
    <Panel
      title="Paper-follow history"
      description={
        props.install === undefined
          ? "Every follow run of this account, newest first."
          : `The follow runs of ${props.install.strategy_ref}, newest first.`
      }
    >
      <AsyncPanel
        query={follows}
        loadingLabel="Loading follow runs…"
        empty={{
          isEmpty: (page: CursorPage<StrategyFollowRun>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No follow runs",
          body: "No paper-follow run is recorded for this selection.",
        }}
      >
        {(page: CursorPage<StrategyFollowRun>) => (
          <>
            {page.items.map((run) => (
              <FollowRunView key={run.id} run={run} />
            ))}
            <CursorPager
              noun="follow runs"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/** One follow run and what became of every intent it produced. */
function FollowRunView(props: { readonly run: StrategyFollowRun }): ReactNode {
  const { run } = props;
  return (
    <section className="callout" aria-label={`Follow run ${run.id}`}>
      <FieldGrid columns={4}>
        <Field label="Run time" note={exactInstant(run.as_of)}>
          {formatInstant(run.as_of)}
          {run.replayed === true && <span className="cell-note">already followed: nothing ran again</span>}
        </Field>
        <Field label="Run">
          <TerminationPill termination={run.termination} />
          <span className="cell-note">{run.trigger}</span>
        </Field>
        <Field label="Proposals">
          {String(run.proposals_count)} proposed, {String(run.accepted_count)} within the manifest,{" "}
          {String(run.violations_count)} violations
        </Field>
        <Field label="Intents">
          {String(run.intents_submitted)} submitted: {String(run.intents_dispatched)} dispatched,{" "}
          {String(run.intents_risk_rejected)} refused by risk, {String(run.intents_failed)} failed
        </Field>
        <Field label="Uninvested paper balance" note="The install's paper book when the run started.">
          {run.paper_cash === undefined ? (
            <span className="absent">not recorded: this run predates paper books</span>
          ) : (
            <SimulatedAmount value={run.paper_cash} />
          )}
        </Field>
        <Field label="Paper book equity" note="The uninvested balance plus every position at its reference price, before charges.">
          <SimulatedAmount value={run.equity} />
        </Field>
        <Field label="Turnover dispatched" note="A paper figure.">
          <SimulatedAmount value={run.turnover} />
        </Field>
        <Field label="Sandbox run">
          <Identifier value={run.sandbox_run_id} />
        </Field>
        <Field label="Evidence">
          <Identifier value={run.evidence_ref} />
        </Field>
      </FieldGrid>
      {run.marks.length === 0 ? (
        <p className="note">No reference price was recorded for this run.</p>
      ) : (
        <ScrollTable caption={`Reference prices of follow run ${run.id}`} headers={["Instrument", "Reference price", "Observed"]}>
          {run.marks.map((mark) => (
            <tr key={mark.instrument_id}>
              <th scope="row">
                <code className="mono-small">{mark.instrument_id}</code>
              </th>
              <td>
                <SimulatedAmount value={mark.mark} />
                <span className="cell-note">
                  observation <code className="mono-small">{mark.price_id}</code>
                </span>
              </td>
              <td>{formatInstant(mark.observed_at)}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
      {run.intents.length === 0 ? (
        <p className="note">This run produced no intent.</p>
      ) : (
        <ScrollTable
          caption={`Intents of follow run ${run.id}`}
          headers={["Proposal", "Kind", "Notional", "Outcome", "Risk Kernel V2", "Execution", "Intent"]}
        >
          {run.intents.map((intent) => (
            <tr key={intent.intent_id}>
              <th scope="row">{String(intent.proposal_seq)}</th>
              <td>{intent.kind}</td>
              <td>
                <SimulatedAmount value={intent.notional} />
                {intent.reference_price !== undefined && (
                  <span className="cell-note">
                    at the reference price <SimulatedAmount value={intent.reference_price} />
                  </span>
                )}
              </td>
              <td>
                <OutcomePill outcome={intent.outcome} />
              </td>
              <td>
                {intent.risk_decision === undefined ? (
                  <span className="absent">no decision: the route was refused first</span>
                ) : (
                  <>
                    <Pill tone={verdictTone(intent.risk_decision.verdict)}>{intent.risk_decision.verdict}</Pill>
                    <Codes codes={intent.risk_decision.reason_codes} none="no reason codes" />
                    <span className="cell-note">
                      decision <code className="mono-small">{intent.risk_decision.decision_hash}</code>
                    </span>
                  </>
                )}
              </td>
              <td>
                {intent.effect_status === undefined ? (
                  <span className="absent">not dispatched</span>
                ) : (
                  <>
                    <Pill tone="neutral">{intent.effect_status}</Pill>
                    {intent.effect_failure_code !== undefined && <Codes codes={[intent.effect_failure_code]} />}
                  </>
                )}
              </td>
              <td>
                <Link to={strategyHref.intent(intent.intent_id)}>
                  <code className="mono-small">{intent.intent_id}</code>
                </Link>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </section>
  );
}

function OutcomeGlossary(): ReactNode {
  return (
    <Panel title="What became of an intent" description="Every intent a strategy proposes ends in exactly one of these.">
      <Table caption="Follow outcomes and their meaning" headers={["Outcome", "What it means"]}>
        {FOLLOW_OUTCOMES.map((outcome) => (
          <tr key={outcome}>
            <th scope="row">
              <Pill tone={outcomeTone(outcome)}>{outcome}</Pill>
            </th>
            <td className="cell-prose">{OUTCOME_MEANING[outcome]}</td>
          </tr>
        ))}
      </Table>
    </Panel>
  );
}

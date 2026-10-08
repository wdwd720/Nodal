/**
 * One strategy listing, with every version this session may see: its
 * lifecycle and moderation state, the manifest it is held to, its
 * validation, its reviews, its provenance chain, the simulations run on it
 * and the paper performance of installs of it.
 *
 * Evaluations are SIMULATED results over the snapshot set whoever ran them
 * supplied. Performance figures are PAPER results the backend computes from
 * each install's paper book -- its paper capital moved by its own fills,
 * valued at reference prices the backend reads itself. Both are labelled as
 * such wherever they appear, and never as a record of what any account
 * earned.
 */
import { useId, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useInstallVersion,
  useSimulateVersion,
  useStrategyListing,
  useSubmitVersion,
  type StrategyEvaluation,
  type StrategyInstall,
  type StrategyListingDetail,
  type StrategyVersionDetail,
  type SimulateInput,
  type InstallInput,
  type VersionAccountInput,
} from "../../../api/advanced/marketplace.ts";
import { Button, LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel, EmptyState, Explanation } from "../../../components/DataState.tsx";
import { Disclosure, Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { formatSeconds } from "../../../lib/advanced.ts";
import { SIMULATED_RESULTS_NOTICE } from "../../../lib/honesty.ts";
import {
  BUILD_FAILURE_MEANING,
  MAX_SIMULATION_SNAPSHOTS,
  STATUS_MEANING,
  canFork,
  canInstall,
  canSimulate,
  canSubmit,
  parseSnapshotSet,
  type StrategyStatus,
} from "../../../lib/marketplace.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { Codes, ScrollTable } from "../shared.tsx";
import { BasisPoints, BuildLog, SimulatedAmount, TerminationPill, VersionState, strategyHref, usePrepared } from "./parts.tsx";

export function ListingDetailPage(): ReactNode {
  const { listingId } = useParams();
  const listing = useStrategyListing(listingId);
  return (
    <Page
      title="Strategy listing"
      lead="One listing with every version this session may see, the manifest each is held to, and everything recorded about it."
      actions={<LinkButton to={strategyHref.home()}>Back to strategies</LinkButton>}
    >
      <SimulatedNotice />
      <Disclosure title="Simulated and paper results">
        <p>{SIMULATED_RESULTS_NOTICE}</p>
        <p>
          Evaluations run a version against a fixed set of snapshots supplied by whoever ran them. Paper performance is
          computed by the backend from each install&apos;s paper book: its paper capital moved by its own fills, valued at
          reference prices the backend reads itself, before the paper venue&apos;s disclosed charges. Nobody supplies the
          figures it is computed from.
        </p>
      </Disclosure>
      <AsyncPanel
        query={listing}
        loadingLabel="Loading the listing…"
        errorExtra={
          <p className="explain-body">
            The backend answers NOT_FOUND both for an id it has never recorded and for a listing none of whose versions
            this session may see, so the answer neither confirms nor denies that the listing exists.
          </p>
        }
      >
        {(found: StrategyListingDetail) => <ListingBody listing={found} />}
      </AsyncPanel>
    </Page>
  );
}

function ListingBody(props: { readonly listing: StrategyListingDetail }): ReactNode {
  const { listing } = props;
  const { ownAccountId } = useAdvancedAccount();
  const owner = listing.creator_account_id !== undefined && listing.creator_account_id === ownAccountId;
  return (
    <>
      <Panel
        title={listing.title}
        description="The listing: a container its creator owns. Versions are what is built, reviewed, installed and followed."
        actions={
          owner ? (
            <LinkButton to={strategyHref.newVersion(listing.id)}>Publish a new version</LinkButton>
          ) : undefined
        }
      >
        <FieldGrid columns={3}>
          <Field label="Listing">
            <Identifier value={listing.id} />
          </Field>
          <Field label="Listed" note={exactInstant(listing.created_at)}>
            {formatInstant(listing.created_at)}
          </Field>
          <Field label="Creator's account" note="Shown to the creator's account and to operators only.">
            {listing.creator_account_id === undefined ? (
              <span className="absent">not shown to this session</span>
            ) : (
              <Identifier value={listing.creator_account_id} />
            )}
          </Field>
          <Field label="Forked from">
            {listing.forked_from_version_id === undefined ? (
              <span className="absent">an original listing</span>
            ) : (
              <Identifier label="version" value={listing.forked_from_version_id} />
            )}
          </Field>
          <Field label="Versions shown">{String(listing.versions.length)}</Field>
        </FieldGrid>
      </Panel>
      {listing.versions.map((version) => (
        <VersionPanel key={version.id} version={version} owner={owner} />
      ))}
    </>
  );
}

function VersionPanel(props: { readonly version: StrategyVersionDetail; readonly owner: boolean }): ReactNode {
  const { version: v } = props;
  const m = v.manifest;
  return (
    <Panel title={`Version ${String(v.number)}`} description={`Intents name it ${v.strategy_ref}.`}>
      <FieldGrid columns={3}>
        <Field label="State" note={STATUS_MEANING[v.status as StrategyStatus]}>
          <VersionState status={v.status} moderation={v.moderation} />
          {v.rejection_code !== undefined && <Codes codes={[v.rejection_code]} />}
        </Field>
        <Field label="Risk class" note="The creator's own statement of how risky it is.">
          {v.risk_class}
        </Field>
        <Field label="Created by" note="Who wrote it is shown to the creator's account and to operators only.">
          {v.created_by_actor_type}
          {v.created_by_actor_id !== undefined && <span className="cell-note">{v.created_by_actor_id}</span>}
        </Field>
        <Field label="Published" note={exactInstant(v.created_at)}>
          {formatInstant(v.created_at)}
        </Field>
        <Field label="Last changed" note={exactInstant(v.updated_at)}>
          {formatInstant(v.updated_at)}
        </Field>
        <Field label="Source commit">
          <Identifier value={v.source_commit} />
        </Field>
      </FieldGrid>
      <p className="cell-prose">{v.description}</p>

      <h3>Manifest: everything this version may do</h3>
      <p className="note">
        Fixed when the version was published and hashed; the backend drops and records every proposal beyond it.
      </p>
      <FieldGrid columns={3}>
        <Field label="Intent kinds it may propose">
          <Codes codes={m.allowed_intent_kinds} none="none" />
        </Field>
        <Field label="Instruments it may trade">
          <Codes codes={m.allowed_instrument_ids} none="none by id" />
        </Field>
        <Field label="Instrument kinds it may trade">
          <Codes codes={m.allowed_instrument_kinds} none="none by kind" />
        </Field>
        <Field label="Notional cap per intent">
          <SimulatedAmount value={m.max_notional_per_intent} />
        </Field>
        <Field label="Intents per run, at most">{String(m.max_intents_per_run)}</Field>
        <Field label="Interval between runs, at least">{formatSeconds(m.min_interval_seconds)}</Field>
        <Field label="Manifest hash">
          <Identifier value={v.manifest_hash} />
        </Field>
        <Field label="Source digest">
          <Identifier value={v.source_digest} />
        </Field>
        <Field label="Manifest schema">
          <code className="mono-small">{m.schema}</code>
        </Field>
      </FieldGrid>

      <h3>What it declares</h3>
      <FieldGrid columns={3}>
        <Field label="Supported instruments">
          <Codes codes={v.supported_instrument_ids} none="none declared" />
        </Field>
        <Field label="Data it reads">
          <Codes codes={v.data_dependencies} none="none declared" />
        </Field>
        <Field label="Backtest evidence">
          {v.backtest_evidence_ref === "" ? <span className="absent">none referenced</span> : <Identifier value={v.backtest_evidence_ref} />}
        </Field>
        <Field label="Performance fee" note="Metadata the creator states. Nothing in the marketplace charges it.">
          <BasisPoints value={v.fees.performance_fee_bps} />
        </Field>
        <Field label="Management fee" note="Metadata only, like the performance fee.">
          <BasisPoints value={v.fees.management_fee_bps} />
        </Field>
        <Field label="High-water mark">{v.fees.high_water_mark ? "stated" : "not stated"}</Field>
      </FieldGrid>
      <ConfigTable config={v.config} />

      <Validation version={v} />
      <Reviews version={v} />
      <Provenance version={v} />
      <Evaluations evaluations={v.evaluations} />
      <PaperPerformance version={v} />
      <VersionActions version={v} owner={props.owner} />
    </Panel>
  );
}

function ConfigTable(props: { readonly config: Readonly<Record<string, string>> }): ReactNode {
  const entries = Object.entries(props.config);
  if (entries.length === 0) {
    return <p className="note">No configuration: every run sees an empty configuration.</p>;
  }
  return (
    <ScrollTable caption="Approved configuration every run sees" headers={["Key", "Value"]}>
      {entries.map(([key, value]) => (
        <tr key={key}>
          <th scope="row">
            <code className="mono-small">{key}</code>
          </th>
          <td>
            <code className="mono-small">{value}</code>
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

function Validation(props: { readonly version: StrategyVersionDetail }): ReactNode {
  const val = props.version.validation;
  return (
    <>
      <h3>Validation: scan and build</h3>
      {val === undefined ? (
        <p className="note">Not validated: this version has not been scanned and built.</p>
      ) : (
        <FieldGrid columns={3}>
          <Field label="Outcome">
            <Pill tone={val.outcome === "PASSED" ? "good" : "bad"}>{val.outcome}</Pill>
            {val.failure_reason !== undefined && (
              <>
                <Codes codes={[val.failure_reason]} />
                <span className="cell-note">{BUILD_FAILURE_MEANING[val.failure_reason]}</span>
              </>
            )}
          </Field>
          <Field label="Scan findings">
            <Codes codes={val.scan_codes} none="none" />
          </Field>
          <Field label="Artifact" note="The sha256 of the WebAssembly module every run executes.">
            {val.artifact_digest === undefined ? <span className="absent">no artifact</span> : <Identifier value={val.artifact_digest} />}
          </Field>
          <Field label="Toolchain">
            <code className="mono-small">{val.toolchain_version}</code>
          </Field>
          <Field label="SDK">
            <Identifier value={val.sdk_version} />
          </Field>
          <Field label="Runtime">
            <code className="mono-small">{val.runtime_version}</code>
          </Field>
        </FieldGrid>
      )}
      {val?.build_log !== undefined && <BuildLog log={val.build_log} />}
    </>
  );
}

function Reviews(props: { readonly version: StrategyVersionDetail }): ReactNode {
  const reviews = props.version.reviews;
  return (
    <>
      <h3>Reviews</h3>
      {reviews.length === 0 ? (
        <p className="note">No review recorded.</p>
      ) : (
        <ScrollTable caption={`Reviews of version ${String(props.version.number)}`} headers={["Decision", "Reason", "Note", "Reviewer", "Reviewed"]}>
          {reviews.map((r, index) => (
            <tr key={`${r.reviewed_at}-${String(index)}`}>
              <th scope="row">
                <Pill tone={r.decision === "APPROVE" ? "good" : "bad"}>{r.decision}</Pill>
              </th>
              <td>
                <code className="mono-small">{r.reason_code}</code>
              </td>
              <td className="cell-prose">{r.note === "" ? <span className="absent">no note</span> : r.note}</td>
              <td>
                {r.reviewer_actor_type}
                {r.reviewer_actor_id !== undefined && <span className="cell-note">{r.reviewer_actor_id}</span>}
              </td>
              <td>{formatInstant(r.reviewed_at)}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </>
  );
}

function Provenance(props: { readonly version: StrategyVersionDetail }): ReactNode {
  const p = props.version.provenance;
  return (
    <>
      <h3>Provenance</h3>
      <FieldGrid columns={3}>
        <Field label="Strategy">
          <code className="mono-small">{p.strategy_ref}</code>
        </Field>
        <Field label="Source commit">
          <Identifier value={p.source_commit} />
        </Field>
        <Field label="Source digest">
          <Identifier value={p.source_digest} />
        </Field>
        <Field label="Manifest hash">
          <Identifier value={p.manifest_hash} />
        </Field>
        <Field label="Built artifact">
          {p.artifact_digest === undefined ? <span className="absent">not built</span> : <Identifier value={p.artifact_digest} />}
        </Field>
        <Field label="Built with">
          {p.toolchain_version === undefined ? (
            <span className="absent">not built</span>
          ) : (
            <>
              <code className="mono-small">{p.toolchain_version}</code>
              {p.runtime_version !== undefined && <span className="cell-note">runs on {p.runtime_version}</span>}
            </>
          )}
        </Field>
        <Field label="Latest evaluation dataset">
          {p.dataset_version === undefined ? (
            <span className="absent">never simulated</span>
          ) : (
            <>
              <code className="mono-small">{p.dataset_version}</code>
              {p.evaluation_id !== undefined && <span className="cell-note">evaluation {p.evaluation_id}</span>}
            </>
          )}
        </Field>
      </FieldGrid>
      {p.forked_from.length === 0 ? (
        <p className="note">Not a fork: nothing upstream.</p>
      ) : (
        <ScrollTable caption="Fork chain, nearest parent first" headers={["Parent", "Version", "Source digest", "Manifest hash"]}>
          {p.forked_from.map((a) => (
            <tr key={a.version_id}>
              <th scope="row">
                <Link to={strategyHref.listing(a.listing_id)}>
                  <code className="mono-small">{a.listing_id}</code>
                </Link>
              </th>
              <td>
                version {String(a.number)}
                <span className="cell-note">
                  <code className="mono-small">{a.version_id}</code>
                </span>
              </td>
              <td>
                <Identifier value={a.source_digest} />
              </td>
              <td>
                <Identifier value={a.manifest_hash} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </>
  );
}

function Evaluations(props: { readonly evaluations: readonly StrategyEvaluation[] }): ReactNode {
  return (
    <>
      <h3>Evaluations: simulated results</h3>
      <p className="note">{SIMULATED_RESULTS_NOTICE}</p>
      {props.evaluations.length === 0 ? (
        <p className="note">No simulation recorded for this version.</p>
      ) : (
        <ScrollTable
          caption="Simulations of this version, newest first"
          headers={["Dataset", "Snapshots", "Runs that ended OK", "Proposals", "Within the manifest", "Violations", "Digests", "Recorded"]}
        >
          {props.evaluations.map((e) => (
            <tr key={e.id}>
              <th scope="row">
                <code className="mono-small">{e.dataset_version}</code>
              </th>
              <td>{String(e.snapshots_count)}</td>
              <td>{String(e.runs_ok)}</td>
              <td>{String(e.proposals_count)}</td>
              <td>{String(e.accepted_count)}</td>
              <td>{String(e.violations_count)}</td>
              <td>
                <span className="cell-note">
                  snapshots <code className="mono-small">{e.snapshots_digest}</code>
                </span>
                <span className="cell-note">
                  results <code className="mono-small">{e.results_digest}</code>
                </span>
                <span className="cell-note">
                  evidence <Identifier value={e.evidence_ref} />
                </span>
              </td>
              <td>{formatInstant(e.created_at)}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </>
  );
}

function PaperPerformance(props: { readonly version: StrategyVersionDetail }): ReactNode {
  const perf = props.version.performance;
  return (
    <>
      <h3>Paper performance of installs</h3>
      <p className="note">
        {SIMULATED_RESULTS_NOTICE} Each row is one install's paper-follow history as recorded; the installs belong to
        other accounts and are not named.
      </p>
      {perf.length === 0 ? (
        <p className="note">No paper performance recorded for this version.</p>
      ) : (
        <ScrollTable
          caption="Paper performance snapshots, newest first"
          headers={["Follow runs", "Window", "Return", "Largest drawdown", "Volatility", "Turnover", "History digest", "Computed"]}
        >
          {perf.map((p, index) => (
            <tr key={`${p.computed_at}-${String(index)}`}>
              <th scope="row">{String(p.follow_runs_count)}</th>
              <td>
                {formatInstant(p.first_as_of)}
                <span className="cell-note">to {formatInstant(p.last_as_of)}</span>
              </td>
              <td>
                <BasisPoints value={p.return_bps} />
              </td>
              <td>
                <BasisPoints value={p.max_drawdown_bps} />
              </td>
              <td>
                <BasisPoints value={p.volatility_bps} />
              </td>
              <td>
                <BasisPoints value={p.turnover_bps} />
              </td>
              <td>
                <code className="mono-small">{p.history_digest}</code>
              </td>
              <td>{formatInstant(p.computed_at)}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </>
  );
}

/* --------------------------------------------------------------------------
 * What this session may do with a version
 * ------------------------------------------------------------------------ */

function VersionActions(props: { readonly version: StrategyVersionDetail; readonly owner: boolean }): ReactNode {
  const { version: v, owner } = props;
  const { ownAccountId, canInspect } = useAdvancedAccount();
  return (
    <>
      <h3>Actions</h3>
      <div className="form-actions">
        {owner && canSubmit(v) && ownAccountId !== undefined && <SubmitAction versionId={v.id} accountId={ownAccountId} />}
        {canInstall(v) ? (
          ownAccountId === undefined ? (
            <Button disabledReason="This session owns no account to install for.">Install in PAPER mode</Button>
          ) : (
            <InstallAction versionId={v.id} accountId={ownAccountId} />
          )
        ) : (
          <Button disabledReason="Only an APPROVED, CLEARED version can be installed.">Install in PAPER mode</Button>
        )}
        {canFork(v) ? (
          <LinkButton to={strategyHref.fork(v.id)}>Fork this version</LinkButton>
        ) : (
          <Button disabledReason="Only an APPROVED or DEPRECATED version can be forked.">Fork this version</Button>
        )}
        {canInspect && <LinkButton to={strategyHref.review()}>Open the review queue</LinkButton>}
      </div>
      {owner && canSimulate(v) && ownAccountId !== undefined && <SimulateForm versionId={v.id} accountId={ownAccountId} />}
    </>
  );
}

function SubmitAction(props: { readonly versionId: string; readonly accountId: string }): ReactNode {
  const submit = useSubmitVersion();
  const action = usePrepared<VersionAccountInput>();
  return (
    <div>
      <Button
        variant="primary"
        busy={submit.isPending}
        busyLabel="Submitting…"
        onClick={() => {
          submit.mutate(action.prepared ?? action.prepare((idempotencyKey) => ({ ...props, idempotencyKey })));
        }}
      >
        Submit for review
      </Button>
      {submit.isSuccess && <p className="note">Submitted: the version is {submit.data.status}.</p>}
      {submit.isError && (
        <Explanation
          error={submit.error}
          retryLabel="Submit again with the same key"
          {...(action.prepared === undefined ? {} : { onRetry: () => submit.mutate(action.prepared as VersionAccountInput) })}
        />
      )}
    </div>
  );
}

function InstallAction(props: { readonly versionId: string; readonly accountId: string }): ReactNode {
  const install = useInstallVersion();
  const action = usePrepared<InstallInput>();
  const [confirming, setConfirming] = useState(false);
  if (install.isSuccess) {
    return <Installed install={install.data} />;
  }
  return (
    <div>
      {confirming ? (
        <div className="callout" role="group" aria-label="Confirm the install">
          <p>
            Installing adds this version to the account in PAPER mode, the only mode an install can have. Following it
            proposes intents that face Risk Kernel V2; nothing moves real money.
          </p>
          <div className="form-actions">
            <Button
              variant="primary"
              busy={install.isPending}
              busyLabel="Installing…"
              onClick={() => {
                install.mutate(action.prepared ?? action.prepare((idempotencyKey) => ({ ...props, idempotencyKey })));
              }}
            >
              Confirm the PAPER install
            </Button>
            <Button
              variant="quiet"
              onClick={() => {
                setConfirming(false);
                action.clear();
                install.reset();
              }}
            >
              Cancel
            </Button>
          </div>
        </div>
      ) : (
        <Button
          onClick={() => {
            setConfirming(true);
          }}
        >
          Install in PAPER mode
        </Button>
      )}
      {install.isError && (
        <Explanation
          error={install.error}
          retryLabel="Install again with the same key"
          {...(action.prepared === undefined ? {} : { onRetry: () => install.mutate(action.prepared as InstallInput) })}
        />
      )}
    </div>
  );
}

function Installed(props: { readonly install: StrategyInstall }): ReactNode {
  return (
    <div className="callout" role="status">
      <p>
        Installed in <Pill tone="info">{props.install.mode}</Pill> mode as <Identifier value={props.install.id} />.
      </p>
      <LinkButton to={strategyHref.installs()}>Go to your installs</LinkButton>
    </div>
  );
}

function SimulateForm(props: { readonly versionId: string; readonly accountId: string }): ReactNode {
  const simulate = useSimulateVersion();
  const action = usePrepared<SimulateInput>();
  const [dataset, setDataset] = useState("");
  const [snapshots, setSnapshots] = useState("");
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const datasetId = useId();
  const snapshotsId = useId();
  return (
    <form
      className="form"
      aria-label="Simulate this version"
      onSubmit={(event) => {
        event.preventDefault();
        if (dataset.trim() === "") {
          setProblem("Name the dataset version the snapshots come from.");
          return;
        }
        const set = parseSnapshotSet(snapshots);
        if (!set.ok) {
          setProblem(set.error);
          return;
        }
        setProblem(undefined);
        simulate.mutate(
          action.prepared ??
            action.prepare((idempotencyKey) => ({
              ...props,
              datasetVersion: dataset.trim(),
              snapshots: set.value,
              idempotencyKey,
            })),
        );
      }}
    >
      <h3>Simulate against a fixed snapshot set</h3>
      <p className="note">
        Runs this built version once per snapshot, in order, and records the evaluation. A simulation proposes nothing:
        every proposal is counted and kept in the run log, never submitted. At most {String(MAX_SIMULATION_SNAPSHOTS)}{" "}
        snapshots per request.
      </p>
      <div className="form-row">
        <label htmlFor={datasetId}>Dataset version</label>
        <input
          id={datasetId}
          className="input"
          value={dataset}
          autoComplete="off"
          onChange={(event) => {
            setDataset(event.target.value);
            action.clear();
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={snapshotsId}>Snapshots (a JSON array)</label>
        <textarea
          id={snapshotsId}
          className="input"
          rows={8}
          spellCheck={false}
          value={snapshots}
          onChange={(event) => {
            setSnapshots(event.target.value);
            action.clear();
          }}
        />
        <p className="field-note">
          Each snapshot has the as_of time it describes, its instruments (id, kind, decimals, mark and closes as decimal
          strings) and a portfolio (balance in simulated dollars as a decimal string, positions in base units).
        </p>
      </div>
      {problem !== undefined && (
        <p className="field-error" role="alert">
          {problem}
        </p>
      )}
      <div className="form-actions">
        <Button submit busy={simulate.isPending} busyLabel="Running the simulation…">
          Run the simulation
        </Button>
      </div>
      {simulate.isError && <Explanation error={simulate.error} />}
      {simulate.isSuccess && <SimulationResult evaluation={simulate.data} />}
    </form>
  );
}

function SimulationResult(props: { readonly evaluation: StrategyEvaluation }): ReactNode {
  const e = props.evaluation;
  const runs = e.runs ?? [];
  return (
    <div className="callout" role="status">
      <p>
        Recorded evaluation <Identifier value={e.id} /> on dataset <code className="mono-small">{e.dataset_version}</code>
        : {String(e.runs_ok)} of {String(e.snapshots_count)} runs ended OK. Simulated results only.
      </p>
      {runs.length === 0 ? (
        <EmptyState title="No runs listed" body="The backend returned the evaluation without its runs." />
      ) : (
        <ScrollTable caption="Runs of this simulation" headers={["Snapshot", "As of", "Ended", "Proposals", "Within the manifest", "Violations"]}>
          {runs.map((r) => (
            <tr key={r.seq}>
              <th scope="row">{String(r.seq)}</th>
              <td>{formatInstant(r.as_of)}</td>
              <td>
                <TerminationPill termination={r.termination} />
              </td>
              <td>{String(r.proposals)}</td>
              <td>{String(r.accepted)}</td>
              <td>{String(r.violations)}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </div>
  );
}

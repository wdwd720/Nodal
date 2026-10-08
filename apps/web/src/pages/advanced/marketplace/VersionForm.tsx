/**
 * Publishing a strategy: a new listing with its first version, the next
 * version of one of your listings, or a fork of a public version into a
 * listing of your own.
 *
 * Every version starts at DRAFT and is immutable from the moment it is
 * published: its manifest (the permissions it is held to at run time), its
 * source and its configuration can never change. Nothing is built until you
 * submit it and an operator validates it, and nobody else sees it until an
 * operator who is not you approves it.
 */
import { useId, useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";

import {
  useCreateListing,
  useForkVersion,
  useNewVersion,
  type CreateListingInput,
  type ForkInput,
  type NewVersionInput,
  type StrategyListingDetail,
  type StrategyVersion,
} from "../../../api/advanced/marketplace.ts";
import { Button, LinkButton } from "../../../components/Button.tsx";
import { EmptyState, Explanation } from "../../../components/DataState.tsx";
import { Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { INSTRUMENT_KINDS } from "../../../lib/advanced.ts";
import {
  EMPTY_VERSION_DRAFT,
  STRATEGY_INTENT_KINDS,
  STRATEGY_RISK_CLASSES,
  buildVersionInput,
  toggle,
  type StrategyRiskClass,
  type VersionDraft,
} from "../../../lib/marketplace.ts";
import { useAdvancedAccount } from "../area.ts";
import { strategyHref, usePrepared } from "./parts.tsx";

/* --------------------------------------------------------------------------
 * Pages
 * ------------------------------------------------------------------------ */

export function CreateListingPage(): ReactNode {
  const { ownAccountId } = useAdvancedAccount();
  const create = useCreateListing();
  const action = usePrepared<CreateListingInput>();
  const [title, setTitle] = useState("");
  const [draft, setDraft] = useState<VersionDraft>(EMPTY_VERSION_DRAFT);
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const titleId = useId();

  return (
    <Page
      title="Publish a strategy"
      lead="A new listing with its first version, in DRAFT. Only this account sees it until it is submitted, validated and approved by an operator who is not you."
      actions={<LinkButton to={strategyHref.home()}>Back to strategies</LinkButton>}
    >
      <SimulatedNotice />
      {ownAccountId === undefined ? (
        <OwnAccountMissing />
      ) : create.isSuccess ? (
        <Published listing={create.data} />
      ) : (
        <Panel title="The strategy" description="Nothing is published until you confirm at the end of the form.">
          <form
            className="form"
            noValidate
            onSubmit={(event) => {
              event.preventDefault();
              const version = buildVersionInput(draft, false);
              if (title.trim() === "") {
                setProblem("Give the listing a title.");
                return;
              }
              if (!version.ok) {
                setProblem(version.error);
                return;
              }
              setProblem(undefined);
              create.mutate(
                action.prepared ??
                  action.prepare((idempotencyKey) => ({
                    accountId: ownAccountId,
                    title: title.trim(),
                    version: version.value,
                    idempotencyKey,
                  })),
              );
            }}
          >
            <div className="form-row">
              <label htmlFor={titleId}>Title</label>
              <input
                id={titleId}
                className="input"
                value={title}
                maxLength={200}
                autoComplete="off"
                onChange={(event) => {
                  setTitle(event.target.value);
                  action.clear();
                }}
              />
            </div>
            <VersionFields
              draft={draft}
              inherit={false}
              onChange={(next) => {
                setDraft(next);
                action.clear();
              }}
            />
            <Submit problem={problem} pending={create.isPending} label="Publish the draft" />
            {create.isError && (
              <Explanation
                error={create.error}
                retryLabel="Publish again with the same key"
                {...(action.prepared === undefined ? {} : { onRetry: () => create.mutate(action.prepared as CreateListingInput) })}
              />
            )}
          </form>
        </Panel>
      )}
    </Page>
  );
}

export function NewVersionPage(): ReactNode {
  const { listingId } = useParams();
  const { ownAccountId } = useAdvancedAccount();
  const publish = useNewVersion();
  const action = usePrepared<NewVersionInput>();
  const [draft, setDraft] = useState<VersionDraft>(EMPTY_VERSION_DRAFT);
  const [problem, setProblem] = useState<string | undefined>(undefined);

  return (
    <Page
      title="Publish a new version"
      lead="The next version of one of your listings, in DRAFT. Earlier versions are untouched; this one goes through validation and review on its own."
      actions={
        listingId === undefined ? undefined : <LinkButton to={strategyHref.listing(listingId)}>Back to the listing</LinkButton>
      }
    >
      <SimulatedNotice />
      {ownAccountId === undefined || listingId === undefined ? (
        <OwnAccountMissing />
      ) : publish.isSuccess ? (
        <VersionPublished version={publish.data} />
      ) : (
        <Panel title="The new version" description={`For listing ${listingId}.`}>
          <form
            className="form"
            noValidate
            onSubmit={(event) => {
              event.preventDefault();
              const version = buildVersionInput(draft, false);
              if (!version.ok) {
                setProblem(version.error);
                return;
              }
              setProblem(undefined);
              publish.mutate(
                action.prepared ??
                  action.prepare((idempotencyKey) => ({
                    accountId: ownAccountId,
                    listingId,
                    version: version.value,
                    idempotencyKey,
                  })),
              );
            }}
          >
            <VersionFields
              draft={draft}
              inherit={false}
              onChange={(next) => {
                setDraft(next);
                action.clear();
              }}
            />
            <Submit problem={problem} pending={publish.isPending} label="Publish the version" />
            {publish.isError && (
              <Explanation
                error={publish.error}
                retryLabel="Publish again with the same key"
                {...(action.prepared === undefined ? {} : { onRetry: () => publish.mutate(action.prepared as NewVersionInput) })}
              />
            )}
          </form>
        </Panel>
      )}
    </Page>
  );
}

export function ForkPage(): ReactNode {
  const { versionId } = useParams();
  const { ownAccountId } = useAdvancedAccount();
  const fork = useForkVersion();
  const action = usePrepared<ForkInput>();
  const [title, setTitle] = useState("");
  const [draft, setDraft] = useState<VersionDraft>(EMPTY_VERSION_DRAFT);
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const titleId = useId();

  return (
    <Page
      title="Fork a strategy"
      lead="A new listing of your own whose provenance names the version it forks. It starts at DRAFT and goes through validation and review again."
      actions={<LinkButton to={strategyHref.home()}>Back to strategies</LinkButton>}
    >
      <SimulatedNotice />
      {ownAccountId === undefined || versionId === undefined ? (
        <OwnAccountMissing />
      ) : fork.isSuccess ? (
        <Published listing={fork.data} />
      ) : (
        <Panel
          title="The fork"
          description={`Forking version ${versionId}. Leave the source and the whole manifest empty to start from the parent's.`}
        >
          <form
            className="form"
            noValidate
            onSubmit={(event) => {
              event.preventDefault();
              const version = buildVersionInput(draft, true);
              if (title.trim() === "") {
                setProblem("Give the fork a title.");
                return;
              }
              if (!version.ok) {
                setProblem(version.error);
                return;
              }
              setProblem(undefined);
              fork.mutate(
                action.prepared ??
                  action.prepare((idempotencyKey) => ({
                    accountId: ownAccountId,
                    versionId,
                    title: title.trim(),
                    version: version.value,
                    idempotencyKey,
                  })),
              );
            }}
          >
            <div className="form-row">
              <label htmlFor={titleId}>Title</label>
              <input
                id={titleId}
                className="input"
                value={title}
                maxLength={200}
                autoComplete="off"
                onChange={(event) => {
                  setTitle(event.target.value);
                  action.clear();
                }}
              />
            </div>
            <VersionFields
              draft={draft}
              inherit
              onChange={(next) => {
                setDraft(next);
                action.clear();
              }}
            />
            <Submit problem={problem} pending={fork.isPending} label="Publish the fork" />
            {fork.isError && (
              <Explanation
                error={fork.error}
                retryLabel="Fork again with the same key"
                {...(action.prepared === undefined ? {} : { onRetry: () => fork.mutate(action.prepared as ForkInput) })}
              />
            )}
          </form>
        </Panel>
      )}
    </Page>
  );
}

function OwnAccountMissing(): ReactNode {
  return (
    <EmptyState
      title="No account of your own"
      body="Publishing acts for an account this session owns, and the backend returned none. An operator inspecting another account cannot publish for it."
    />
  );
}

function Submit(props: { readonly problem: string | undefined; readonly pending: boolean; readonly label: string }): ReactNode {
  return (
    <>
      {props.problem !== undefined && (
        <p className="field-error" role="alert">
          {props.problem}
        </p>
      )}
      <div className="form-actions">
        <Button variant="primary" submit busy={props.pending} busyLabel="Publishing…">
          {props.label}
        </Button>
      </div>
      <p className="field-note">
        The request is keyed once when you confirm and the same key is reused if you retry, so publishing twice by
        accident is impossible. The backend validates everything again and decides.
      </p>
    </>
  );
}

function Published(props: { readonly listing: StrategyListingDetail }): ReactNode {
  const first = props.listing.versions[0];
  return (
    <Panel title="Published as a draft" description="Only this account and operators can see it until it is approved.">
      <p>
        <strong>{props.listing.title}</strong> is listing <Identifier value={props.listing.id} />
        {first !== undefined && (
          <>
            , version {String(first.number)} in <Pill tone="info">{first.status}</Pill>
          </>
        )}
        .
      </p>
      <div className="form-actions">
        <LinkButton to={strategyHref.listing(props.listing.id)} variant="primary">
          Open the listing to submit it
        </LinkButton>
      </div>
    </Panel>
  );
}

function VersionPublished(props: { readonly version: StrategyVersion }): ReactNode {
  return (
    <Panel title="Published as a draft" description="Submit it from the listing when it is ready for validation.">
      <p>
        Version {String(props.version.number)} is <Pill tone="info">{props.version.status}</Pill> as{" "}
        <code className="mono-small">{props.version.strategy_ref}</code>.
      </p>
      <div className="form-actions">
        <LinkButton to={strategyHref.listing(props.version.listing_id)} variant="primary">
          Open the listing
        </LinkButton>
      </div>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * A version's fields
 * ------------------------------------------------------------------------ */

function VersionFields(props: {
  readonly draft: VersionDraft;
  readonly inherit: boolean;
  readonly onChange: (next: VersionDraft) => void;
}): ReactNode {
  const { draft } = props;
  const set = (patch: Partial<VersionDraft>): void => {
    props.onChange({ ...draft, ...patch });
  };
  const ids = {
    description: useId(),
    risk: useId(),
    commit: useId(),
    file: useId(),
    source: useId(),
    instruments: useId(),
    notional: useId(),
    perRun: useId(),
    interval: useId(),
    supported: useId(),
    deps: useId(),
    perf: useId(),
    mgmt: useId(),
    config: useId(),
    evidence: useId(),
  };
  return (
    <>
      <div className="form-row">
        <label htmlFor={ids.description}>What the strategy does</label>
        <textarea
          id={ids.description}
          className="input"
          rows={3}
          maxLength={4000}
          value={draft.description}
          onChange={(event) => {
            set({ description: event.target.value });
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={ids.risk}>Risk class (your own statement)</label>
        <select
          id={ids.risk}
          className="input"
          value={draft.riskClass}
          onChange={(event) => {
            const chosen = STRATEGY_RISK_CLASSES.find((value) => value === event.target.value);
            if (chosen !== undefined) set({ riskClass: chosen as StrategyRiskClass });
          }}
        >
          {STRATEGY_RISK_CLASSES.map((value) => (
            <option key={value} value={value}>
              {value}
            </option>
          ))}
        </select>
      </div>
      <div className="form-row">
        <label htmlFor={ids.commit}>Source commit</label>
        <input
          id={ids.commit}
          className="input"
          value={draft.sourceCommit}
          maxLength={100}
          autoComplete="off"
          onChange={(event) => {
            set({ sourceCommit: event.target.value });
          }}
        />
        <p className="field-note">Where the source was taken from; it becomes part of the version's provenance.</p>
      </div>
      <div className="form-row">
        <label htmlFor={ids.file}>Source file name</label>
        <input
          id={ids.file}
          className="input"
          value={draft.fileName}
          autoComplete="off"
          spellCheck={false}
          onChange={(event) => {
            set({ fileName: event.target.value });
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={ids.source}>Source (Go, package main, against the strategy SDK)</label>
        <textarea
          id={ids.source}
          className="input"
          rows={14}
          spellCheck={false}
          value={draft.source}
          onChange={(event) => {
            set({ source: event.target.value });
          }}
        />
        <p className="field-note">
          {props.inherit
            ? "Leave empty to start from the parent's source. "
            : ""}
          At most 65536 bytes. It is scanned before anything is built: imports outside a small allowlist, compiler
          directives and escape hatches are refused.
        </p>
      </div>

      <fieldset className="form-row">
        <legend>Manifest: the intent kinds it may propose</legend>
        {STRATEGY_INTENT_KINDS.map((kind) => (
          <label key={kind} className="checkbox">
            <input
              type="checkbox"
              checked={draft.intentKinds.includes(kind)}
              onChange={() => {
                set({ intentKinds: toggle(STRATEGY_INTENT_KINDS, draft.intentKinds, kind) });
              }}
            />
            <span>{kind}</span>
          </label>
        ))}
      </fieldset>
      <div className="form-row">
        <label htmlFor={ids.instruments}>Manifest: instruments it may trade (ids, one per line)</label>
        <textarea
          id={ids.instruments}
          className="input"
          rows={3}
          spellCheck={false}
          value={draft.instrumentIds}
          onChange={(event) => {
            set({ instrumentIds: event.target.value });
          }}
        />
      </div>
      <fieldset className="form-row">
        <legend>Manifest: instrument kinds it may trade</legend>
        {INSTRUMENT_KINDS.map((kind) => (
          <label key={kind} className="checkbox">
            <input
              type="checkbox"
              checked={draft.instrumentKinds.includes(kind)}
              onChange={() => {
                set({ instrumentKinds: toggle(INSTRUMENT_KINDS, draft.instrumentKinds, kind) });
              }}
            />
            <span>{kind}</span>
          </label>
        ))}
      </fieldset>
      <div className="form-row">
        <label htmlFor={ids.notional}>Manifest: notional cap per intent, in simulated dollars</label>
        <input
          id={ids.notional}
          className="input"
          inputMode="decimal"
          autoComplete="off"
          value={draft.maxNotional}
          onChange={(event) => {
            set({ maxNotional: event.target.value });
          }}
        />
        <p className="field-note">Sent exactly as typed, as a decimal string.</p>
      </div>
      <div className="form-row">
        <label htmlFor={ids.perRun}>Manifest: intents per run, at most (1 to 100)</label>
        <input
          id={ids.perRun}
          className="input"
          inputMode="numeric"
          autoComplete="off"
          value={draft.maxIntentsPerRun}
          onChange={(event) => {
            set({ maxIntentsPerRun: event.target.value });
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={ids.interval}>Manifest: seconds between runs, at least (60 or more)</label>
        <input
          id={ids.interval}
          className="input"
          inputMode="numeric"
          autoComplete="off"
          value={draft.minIntervalSeconds}
          onChange={(event) => {
            set({ minIntervalSeconds: event.target.value });
          }}
        />
        {props.inherit && (
          <p className="field-note">Leave every manifest field empty to take the parent's manifest unchanged.</p>
        )}
      </div>

      <div className="form-row">
        <label htmlFor={ids.supported}>Supported instruments (ids, one per line)</label>
        <textarea
          id={ids.supported}
          className="input"
          rows={2}
          spellCheck={false}
          value={draft.supportedInstrumentIds}
          onChange={(event) => {
            set({ supportedInstrumentIds: event.target.value });
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={ids.deps}>Data it reads (lower-case names, comma separated)</label>
        <input
          id={ids.deps}
          className="input"
          autoComplete="off"
          value={draft.dataDependencies}
          onChange={(event) => {
            set({ dataDependencies: event.target.value });
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={ids.perf}>Performance fee metadata, in basis points</label>
        <input
          id={ids.perf}
          className="input"
          inputMode="numeric"
          autoComplete="off"
          value={draft.performanceFeeBps}
          onChange={(event) => {
            set({ performanceFeeBps: event.target.value });
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={ids.mgmt}>Management fee metadata, in basis points</label>
        <input
          id={ids.mgmt}
          className="input"
          inputMode="numeric"
          autoComplete="off"
          value={draft.managementFeeBps}
          onChange={(event) => {
            set({ managementFeeBps: event.target.value });
          }}
        />
        <label className="checkbox">
          <input
            type="checkbox"
            checked={draft.highWaterMark}
            onChange={() => {
              set({ highWaterMark: !draft.highWaterMark });
            }}
          />
          <span>States a high-water mark</span>
        </label>
        <p className="field-note">Fees are metadata you state. Nothing in the marketplace charges them.</p>
      </div>
      <div className="form-row">
        <label htmlFor={ids.config}>Configuration every run sees (key=value, one per line)</label>
        <textarea
          id={ids.config}
          className="input"
          rows={3}
          spellCheck={false}
          value={draft.config}
          onChange={(event) => {
            set({ config: event.target.value });
          }}
        />
      </div>
      <div className="form-row">
        <label htmlFor={ids.evidence}>Backtest evidence reference (optional)</label>
        <input
          id={ids.evidence}
          className="input"
          autoComplete="off"
          value={draft.backtestEvidenceRef}
          onChange={(event) => {
            set({ backtestEvidenceRef: event.target.value });
          }}
        />
      </div>
    </>
  );
}

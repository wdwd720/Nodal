/**
 * MARGIN: the account's margin account as the margin engine evaluates it at
 * the instant of the read — collateral by asset with every haircut, equity,
 * initial and maintenance margin, effective leverage, margin available, the
 * liquidation buffer, loans with the interest accrued on them, the open
 * margin call and the latest liquidation case — and the commands its owner
 * can give.
 *
 * The page formats and never computes (PART 110). Dollar figures are the
 * engine's own cent rendering of sandbox-dollar base units, shown beside the
 * exact base units. The engine reports no liquidation price, so the page
 * shows none rather than estimating one.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { ApiProblem } from "@controlplane/generated-client";

import {
  useCloseMarginAccount,
  useLeveragedBuy,
  useMarginAccount,
  useMarginBorrow,
  useMarginRepay,
  useOpenMarginAccount,
  usePostCollateral,
  useReleaseCollateral,
  type AmountRequest,
  type CollateralRequest,
  type MarginAccount,
  type MarginAccountDetail,
  type MarginCollateralResult,
  type MarginEvaluation,
  type MarginLeveragedBuyResult,
  type MarginLoanResult,
  type OpenRequest,
} from "../../../api/advanced/margin.ts";
import { useAssets, type Asset } from "../../../api/queries.ts";
import { EmptyState, Explanation, Loading } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Bps, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import {
  ACCOUNT_STATUS_MEANING,
  LEVERAGED_MARGIN_MODES,
  MARGIN_RUN_MODES,
  accountStatusTone,
  assetById,
  isLeveragedMode,
  marginModeLabel,
  simulatedAssets,
  type LeveragedMarginMode,
} from "../../../lib/margin.ts";
import { parseQuantityInput } from "../../../lib/money.ts";
import { formatInstant } from "../../../lib/time.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import { ModePill, NoAccount, ScrollTable, ValueDomainPill } from "../shared.tsx";
import {
  AssetName,
  AssetQty,
  CallSummary,
  CaseSummary,
  CommandFlow,
  EvaluationMeta,
  HoldingsTable,
  Leverage,
  MarginNav,
  caseHref,
  type Prepared,
} from "./parts.tsx";

export function MarginAccountPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  return (
    <Page
      title="Margin"
      lead="This account's margin account, evaluated by the margin engine at the instant of the read: collateral, equity, margin, leverage, loans and liquidation."
    >
      <SimulatedNotice />
      <MarginNav />
      {accountId === undefined ? (
        <Panel title="Margin account">
          <NoAccount />
        </Panel>
      ) : (
        <MarginAccountView key={accountId} accountId={accountId} />
      )}
    </Page>
  );
}

function MarginAccountView(props: { readonly accountId: string }): ReactNode {
  const account = useAdvancedAccount();
  const margin = useMarginAccount(props.accountId);
  const assets = useAssets();
  const registry = assets.data ?? [];
  const canAct = !account.inspecting && account.ownAccountId === props.accountId;

  if (margin.isPending) {
    return (
      <Panel title="Margin account">
        <Loading label="Asking the margin engine to evaluate this account…" />
      </Panel>
    );
  }
  if (margin.isError) {
    const notFound = margin.error instanceof ApiProblem && margin.error.code === "NOT_FOUND";
    return (
      <>
        <Panel title="Margin account" description="As the backend answered for this account.">
          {notFound ? (
            <EmptyState
              title="No margin account"
              body="The backend has no margin account for this account. Nothing is evaluated, borrowed or held on margin until one is opened."
            />
          ) : (
            <Explanation
              error={margin.error}
              onRetry={() => {
                void margin.refetch();
              }}
            />
          )}
        </Panel>
        {notFound && canAct && (
          <Panel
            title="Open a margin account"
            description="Opens this account's margin account in PAPER or SANDBOX mode, with the sandbox dollar as its unit of account. The backend decides whether it may."
          >
            <OpenAccountForm accountId={props.accountId} assets={registry} />
          </Panel>
        )}
      </>
    );
  }

  const detail = margin.data;
  const ev = detail.evaluation;
  const unit = assetById(registry, ev.numeraire_asset_id)?.symbol ?? "numeraire units";
  return (
    <>
      <SummaryPanel detail={detail} assets={registry} />
      <FiguresPanel evaluation={ev} unit={unit} />
      <Panel
        title="Collateral by asset"
        description="Every asset the margin account holds, marked, haircut factor by factor, and counted at its collateral value. Values are in base units of the numeraire."
      >
        {ev.holdings.length === 0 ? (
          <EmptyState title="No collateral" body="The margin account holds nothing. Its collateral is zero because it holds nothing, not because a figure is missing." />
        ) : (
          <HoldingsTable evaluation={ev} assets={registry} unit={unit} />
        )}
      </Panel>
      <BorrowingPanel detail={detail} unit={unit} />
      <LiquidationPanel detail={detail} unit={unit} />
      {canAct ? (
        <ActionsPanel detail={detail} assets={registry} unit={unit} />
      ) : (
        <Panel title="Commands" description="Who may act on a margin account.">
          <p className="note">
            Commands act only on an account the signed-in principal owns. This account is being read, not acted on, so
            no command is offered here.
          </p>
        </Panel>
      )}
    </>
  );
}

function SummaryPanel(props: { readonly detail: MarginAccountDetail; readonly assets: readonly Asset[] }): ReactNode {
  const { detail } = props;
  const ev = detail.evaluation;
  return (
    <Panel title="Margin account" description="The account's standing, as stored and as the engine evaluates it now.">
      <FieldGrid columns={3}>
        <Field label="Status" note={ACCOUNT_STATUS_MEANING[detail.status]}>
          <span data-field="status">
            <Pill tone={accountStatusTone(detail.status)}>{detail.status}</Pill>
          </span>
        </Field>
        <Field
          label="Health now"
          note="The engine's evaluation at this instant. The stored status moves only when the account is marked to market or acted on, so it can lag this."
        >
          <span data-field="health">
            <Pill tone={accountStatusTone(ev.health)}>{ev.health}</Pill>
          </span>
        </Field>
        <Field label="Margin mode" note="How the account margins its positions.">
          {marginModeLabel(detail.margin_mode)}
          {detail.isolated_asset_id !== undefined && (
            <span className="cell-note">
              isolated asset <AssetName assets={props.assets} id={detail.isolated_asset_id} />
            </span>
          )}
        </Field>
        <Field label="Mode" note={ADVANCED_MODE_DESCRIPTIONS[detail.mode] ?? "A mode this interface has no description for."}>
          <ModePill mode={detail.mode} />
        </Field>
        <Field label="Numeraire" note="The unit every figure of the account is stated in: the sandbox dollar, valued at par.">
          <AssetName assets={props.assets} id={detail.numeraire_asset_id} />
        </Field>
        <Field label="Opened" note="When the margin account was opened.">
          {formatInstant(detail.opened_at)}
        </Field>
      </FieldGrid>
      <p className="meta-row">
        <ValueDomainPill value={detail.value_domain} />
        <span className="mono-small">policy {detail.policy_version}</span>
        <span className="mono-small">margin account {detail.id}</span>
      </p>
    </Panel>
  );
}

/**
 * One figure of the evaluation: the engine's own cent rendering (rounded so
 * the account can only look less healthy) and, beside it, the exact base
 * units it was rendered from.
 */
function EvaluationFigure(props: {
  readonly field: string;
  readonly label: string;
  readonly note: string;
  readonly usd: string;
  readonly units: string;
  readonly decimals: number;
  readonly unit: string;
  readonly emphasis?: boolean;
}): ReactNode {
  return (
    <Field label={props.label} note={props.note} emphasis={props.emphasis === true}>
      <span data-field={props.field}>
        <Usd value={props.usd} />
      </span>
      <span className="cell-note">
        exactly <Qty value={props.units} decimals={props.decimals} symbol={props.unit} />
      </span>
    </Field>
  );
}

function FiguresPanel(props: { readonly evaluation: MarginEvaluation; readonly unit: string }): ReactNode {
  const ev = props.evaluation;
  const common = { decimals: ev.numeraire_decimals, unit: props.unit };
  return (
    <Panel
      title="Equity and margin"
      description="The engine's evaluation. Dollar figures are its own cent rendering of the exact base units beside them, rounded so the account can only look less healthy."
    >
      <FieldGrid columns={3}>
        <EvaluationFigure
          {...common}
          field="equity"
          label="Equity"
          note="Eligible collateral plus realized and unrealized P&L, less borrow liabilities, accrued fees and accrued interest."
          usd={ev.usd.equity}
          units={ev.equity}
          emphasis
        />
        <EvaluationFigure
          {...common}
          field="initial_margin"
          label="Initial margin"
          note="The margin the account's exposure uses: the class rate times the risk weight, on each exposure."
          usd={ev.usd.initial_margin}
          units={ev.initial_margin}
          emphasis
        />
        <EvaluationFigure
          {...common}
          field="maintenance_margin"
          label="Maintenance margin"
          note="The equity floor. Below it the account is in a margin call and eligible for liquidation."
          usd={ev.usd.maintenance_margin}
          units={ev.maintenance_margin}
          emphasis
        />
        <EvaluationFigure
          {...common}
          field="liquidation_buffer"
          label="Liquidation buffer"
          note="Equity less maintenance margin: how far the account is from liquidation. Negative once it is eligible."
          usd={ev.usd.liquidation_buffer}
          units={ev.liquidation_buffer}
        />
        <EvaluationFigure
          {...common}
          field="margin_available"
          label="Margin available"
          note="What could be withdrawn or committed to new risk: never above the capacity the leverage cap leaves."
          usd={ev.usd.margin_available}
          units={ev.margin_available}
        />
        <EvaluationFigure
          {...common}
          field="warning_threshold"
          label="Warning threshold"
          note="Maintenance margin plus the warning buffer. Below it the account is in WARNING."
          usd={ev.usd.warning_threshold}
          units={ev.warning_threshold}
        />
        <EvaluationFigure
          {...common}
          field="eligible_collateral"
          label="Eligible collateral"
          note="Every holding at its collateral value, after its haircut."
          usd={ev.usd.eligible_collateral}
          units={ev.eligible_collateral}
        />
        <EvaluationFigure
          {...common}
          field="market_value"
          label="Market value"
          note="Every holding at its mark, before any haircut."
          usd={ev.usd.market_value}
          units={ev.market_value}
        />
        <EvaluationFigure
          {...common}
          field="gross_exposure"
          label="Gross exposure"
          note="The market value of every non-numeraire holding, and any derivative exposure."
          usd={ev.usd.gross_exposure}
          units={ev.gross_exposure}
        />
        <Leverage evaluation={ev} />
        <Field
          label="Liquidation price"
          note="The margin engine reports no liquidation price for an account, so none is estimated here. The liquidation buffer is the engine's own measure of distance to liquidation."
        >
          <span className="absent" data-field="liquidation_price">
            not reported by the engine
          </span>
        </Field>
      </FieldGrid>
      <EvaluationMeta evaluation={ev} />
    </Panel>
  );
}

function BorrowingPanel(props: { readonly detail: MarginAccountDetail; readonly unit: string }): ReactNode {
  const { detail, unit } = props;
  const ev = detail.evaluation;
  const decimals = ev.numeraire_decimals;
  return (
    <Panel
      title="Borrowing"
      description="What the account owes the lending pool. Interest is posted daily into the loan; the interest accrued since the last posting is owed as well."
    >
      <FieldGrid columns={2}>
        <EvaluationFigure
          field="borrow_liabilities"
          label="Borrow balance"
          note="Loan principal and interest already posted into it."
          usd={ev.usd.borrow_liabilities}
          units={ev.borrow_liabilities}
          decimals={decimals}
          unit={unit}
          emphasis
        />
        <EvaluationFigure
          field="accrued_interest"
          label="Accrued interest"
          note="Interest accrued since the last posting, owed in addition to the borrow balance."
          usd={ev.usd.accrued_interest}
          units={ev.accrued_interest}
          decimals={decimals}
          unit={unit}
          emphasis
        />
      </FieldGrid>
      {detail.loans.length === 0 ? (
        <EmptyState title="No open loans" body="The account has no open borrow lot." />
      ) : (
        <ScrollTable caption="Open borrow lots" headers={["Lot", "Principal", "Posted interest", "Annual rate", "Interest posted through"]}>
          {detail.loans.map((loan) => (
            <tr key={loan.lot_id}>
              <th scope="row">
                <Identifier value={loan.lot_id} />
              </th>
              <td>
                <Qty value={loan.principal} decimals={decimals} symbol={unit} />
              </td>
              <td>
                <Qty value={loan.interest} decimals={decimals} symbol={unit} />
              </td>
              <td>
                <Bps value={loan.rate_bps} />
              </td>
              <td>{formatInstant(loan.accrued_through)}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </Panel>
  );
}

function LiquidationPanel(props: { readonly detail: MarginAccountDetail; readonly unit: string }): ReactNode {
  const { detail, unit } = props;
  return (
    <Panel
      title="Margin call and liquidation"
      description="The open margin call, if there is one, and the account's most recent liquidation case."
      actions={<Link to={advancedHref("margin", "history")}>Every call and case</Link>}
    >
      <h3>Open margin call</h3>
      {detail.open_call === undefined ? (
        <EmptyState title="No open margin call" body="The backend reports no open margin call for this account." />
      ) : (
        <CallSummary call={detail.open_call} unit={unit} />
      )}
      <h3>Latest liquidation case</h3>
      {detail.latest_case === undefined ? (
        <EmptyState title="No liquidation case" body="The backend has opened no liquidation case for this account." />
      ) : (
        <>
          <CaseSummary kase={detail.latest_case} unit={unit} />
          <p>
            <Link to={caseHref(detail.latest_case.id)}>The case and every step it executed</Link>
          </p>
        </>
      )}
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Commands of the account's owner
 * ------------------------------------------------------------------------ */

function ActionsPanel(props: {
  readonly detail: MarginAccountDetail;
  readonly assets: readonly Asset[];
  readonly unit: string;
}): ReactNode {
  const { detail } = props;
  const leveraged = isLeveragedMode(detail.margin_mode);
  const held = detail.evaluation.holdings.map((holding) => holding.asset_id);
  return (
    <Panel
      title="Commands"
      description="Each command is reviewed, then confirmed; confirming creates its idempotency key, and a retry reuses it. The margin service and Risk Kernel V2 decide every one."
    >
      <h3>Post collateral</h3>
      <CollateralForm direction="post" detail={detail} assets={props.assets} choices={simulatedAssets(props.assets).map((asset) => asset.id)} />
      <h3>Release collateral</h3>
      <CollateralForm direction="release" detail={detail} assets={props.assets} choices={held} />
      <h3>Borrow</h3>
      {leveraged ? (
        <LoanForm kind="borrow" detail={detail} unit={props.unit} />
      ) : (
        <p className="note">A fully paid margin account does not borrow.</p>
      )}
      <h3>Repay</h3>
      <LoanForm kind="repay" detail={detail} unit={props.unit} />
      <h3>Buy on margin</h3>
      {leveraged ? (
        <LeveragedBuyForm detail={detail} assets={props.assets} unit={props.unit} />
      ) : (
        <p className="note">A fully paid margin account takes no leverage.</p>
      )}
      <h3>Close the margin account</h3>
      <CloseForm detail={detail} />
    </Panel>
  );
}

function AssetSelect(props: {
  readonly id: string;
  readonly label: string;
  readonly assets: readonly Asset[];
  readonly choices: readonly string[];
  readonly value: string;
  readonly onChange: (value: string) => void;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <select
        id={props.id}
        className="input"
        value={props.value}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      >
        <option value="">Choose an asset</option>
        {props.choices.map((choice) => (
          <option key={choice} value={choice}>
            {assetById(props.assets, choice)?.symbol ?? choice}
          </option>
        ))}
      </select>
    </div>
  );
}

function AmountInput(props: {
  readonly id: string;
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly help: string;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <input
        id={props.id}
        className="input"
        inputMode="decimal"
        autoComplete="off"
        value={props.value}
        aria-describedby={`${props.id}-help`}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
      <p id={`${props.id}-help`} className="field-note">
        {props.help}
      </p>
    </div>
  );
}

/** An amount of an asset typed by a person, as exact base units, or why it cannot be one. */
function prepareQuantity(raw: string, asset: Asset | undefined): Prepared<string> {
  if (asset === undefined) return { ok: false, reason: "Choose an asset the registry lists, so its precision is known." };
  const parsed = parseQuantityInput(raw, asset.decimals);
  return parsed.ok ? { ok: true, request: parsed.value } : { ok: false, reason: parsed.error };
}

function OpenAccountForm(props: { readonly accountId: string; readonly assets: readonly Asset[] }): ReactNode {
  const open = useOpenMarginAccount();
  const [marginMode, setMarginMode] = useState<LeveragedMarginMode>("CROSS");
  const [mode, setMode] = useState<(typeof MARGIN_RUN_MODES)[number]>("PAPER");
  const [isolated, setIsolated] = useState("");
  const choices = simulatedAssets(props.assets).map((asset) => asset.id);

  const prepared: Prepared<OpenRequest> =
    marginMode === "ISOLATED" && isolated === ""
      ? { ok: false, reason: "An isolated margin account names the one risky asset it margins." }
      : {
          ok: true,
          request: {
            accountId: props.accountId,
            marginMode,
            mode,
            isolatedAssetId: marginMode === "ISOLATED" ? isolated : undefined,
          },
        };

  return (
    <form
      className="form"
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <div className="form-row">
        <label htmlFor="margin-open-mode">Margin mode</label>
        <select
          id="margin-open-mode"
          className="input"
          value={marginMode}
          onChange={(event) => {
            const next = LEVERAGED_MARGIN_MODES.find((candidate) => candidate === event.target.value);
            if (next !== undefined) setMarginMode(next);
          }}
        >
          {LEVERAGED_MARGIN_MODES.map((candidate) => (
            <option key={candidate} value={candidate}>
              {marginModeLabel(candidate)}
            </option>
          ))}
        </select>
      </div>
      <div className="form-row">
        <label htmlFor="margin-open-run-mode">Run mode</label>
        <select
          id="margin-open-run-mode"
          className="input"
          value={mode}
          onChange={(event) => {
            const next = MARGIN_RUN_MODES.find((candidate) => candidate === event.target.value);
            if (next !== undefined) setMode(next);
          }}
        >
          {MARGIN_RUN_MODES.map((candidate) => (
            <option key={candidate} value={candidate}>
              {candidate} — {ADVANCED_MODE_DESCRIPTIONS[candidate] ?? ""}
            </option>
          ))}
        </select>
      </div>
      {marginMode === "ISOLATED" && (
        <AssetSelect
          id="margin-open-isolated"
          label="The risky asset this account margins"
          assets={props.assets}
          choices={choices}
          value={isolated}
          onChange={setIsolated}
        />
      )}
      <CommandFlow<OpenRequest, MarginAccount>
        label="Open the margin account"
        prepared={prepared}
        command={open}
        refusedNote="No margin account was opened and nothing moved."
        describe={(request) => (
          <FieldGrid columns={3}>
            <Field label="Margin mode">{marginModeLabel(request.marginMode)}</Field>
            <Field label="Run mode">{request.mode}</Field>
            <Field label="Isolated asset">
              {request.isolatedAssetId === undefined ? (
                <span className="absent">none</span>
              ) : (
                <AssetName assets={props.assets} id={request.isolatedAssetId} />
              )}
            </Field>
          </FieldGrid>
        )}
        renderResult={(account) => (
          <p className="note" role="status">
            The backend answered with margin account <code className="mono-small">{account.id}</code>, status{" "}
            <Pill tone={accountStatusTone(account.status)}>{account.status}</Pill>.
          </p>
        )}
      />
    </form>
  );
}

function CollateralForm(props: {
  readonly direction: "post" | "release";
  readonly detail: MarginAccountDetail;
  readonly assets: readonly Asset[];
  readonly choices: readonly string[];
}): ReactNode {
  const post = usePostCollateral();
  const release = useReleaseCollateral();
  const command = props.direction === "post" ? post : release;
  const [assetId, setAssetId] = useState("");
  const [amount, setAmount] = useState("");
  const asset = assetById(props.assets, assetId);
  const quantity = assetId === "" ? ({ ok: false, reason: "Choose an asset." } as const) : prepareQuantity(amount, asset);
  const prepared: Prepared<CollateralRequest> = quantity.ok
    ? { ok: true, request: { accountId: props.detail.account_id, assetId, quantity: quantity.request } }
    : quantity;
  const verb = props.direction === "post" ? "Post collateral" : "Release collateral";
  const id = `margin-${props.direction}`;

  return (
    <form
      className="form"
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <AssetSelect
        id={`${id}-asset`}
        label={props.direction === "post" ? "Asset to post from the wallet" : "Asset to release to the wallet"}
        assets={props.assets}
        choices={props.choices}
        value={assetId}
        onChange={setAssetId}
      />
      <AmountInput
        id={`${id}-amount`}
        label="Amount"
        value={amount}
        onChange={setAmount}
        help="In whole units of the asset; it is sent as exact base units, digit for digit."
      />
      <CommandFlow<CollateralRequest, MarginCollateralResult>
        label={verb}
        prepared={prepared}
        command={command}
        refusedNote="No collateral moved."
        describe={(request) => (
          <FieldGrid columns={2}>
            <Field label="Asset">
              <AssetName assets={props.assets} id={request.assetId} />
            </Field>
            <Field label="Quantity">
              <AssetQty assets={props.assets} id={request.assetId} value={request.quantity} />
            </Field>
          </FieldGrid>
        )}
        renderResult={(result) => (
          <ResultNote replayed={result.replayed} status={result.status}>
            Collateral move <code className="mono-small">{result.move_id}</code>, journal{" "}
            <code className="mono-small">{result.journal_transaction_id}</code>.
          </ResultNote>
        )}
      />
    </form>
  );
}

function LoanForm(props: { readonly kind: "borrow" | "repay"; readonly detail: MarginAccountDetail; readonly unit: string }): ReactNode {
  const borrow = useMarginBorrow();
  const repay = useMarginRepay();
  const command = props.kind === "borrow" ? borrow : repay;
  const [amount, setAmount] = useState("");
  const decimals = props.detail.evaluation.numeraire_decimals;
  const parsed = parseQuantityInput(amount, decimals);
  const prepared: Prepared<AmountRequest> = parsed.ok
    ? { ok: true, request: { accountId: props.detail.account_id, amount: parsed.value } }
    : { ok: false, reason: parsed.error };
  const id = `margin-${props.kind}`;
  return (
    <form
      className="form"
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <AmountInput
        id={`${id}-amount`}
        label={props.kind === "borrow" ? `Amount to borrow, in ${props.unit}` : `Amount to repay, in ${props.unit}`}
        value={amount}
        onChange={setAmount}
        help={
          props.kind === "borrow"
            ? "Borrowed from the lending pool into the wallet. Risk Kernel V2 refuses a loan that would leave equity below initial margin or leverage above the cap."
            : "Repaid from the wallet: interest is accrued to the instant and paid first, oldest loan first."
        }
      />
      <CommandFlow<AmountRequest, MarginLoanResult>
        label={props.kind === "borrow" ? "Borrow" : "Repay"}
        prepared={prepared}
        command={command}
        refusedNote={props.kind === "borrow" ? "Nothing was borrowed." : "Nothing was repaid."}
        describe={(request) => (
          <Field label="Amount">
            <Qty value={request.amount} decimals={decimals} symbol={props.unit} />
          </Field>
        )}
        renderResult={(result) => (
          <>
            <ResultNote replayed={result.replayed} status={result.status}>
              Journal <code className="mono-small">{result.journal_transaction_id}</code>
              {result.lot_id !== undefined && (
                <>
                  , loan lot <code className="mono-small">{result.lot_id}</code>
                </>
              )}
              .
            </ResultNote>
            {result.allocations.length > 0 && (
              <ScrollTable caption="How the repayment was allocated" headers={["Lot", "Interest", "Principal"]}>
                {result.allocations.map((allocation) => (
                  <tr key={allocation.lot_id}>
                    <th scope="row">
                      <Identifier value={allocation.lot_id} />
                    </th>
                    <td>
                      <Qty value={allocation.interest} decimals={decimals} symbol={props.unit} />
                    </td>
                    <td>
                      <Qty value={allocation.principal} decimals={decimals} symbol={props.unit} />
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
          </>
        )}
      />
    </form>
  );
}

function LeveragedBuyForm(props: {
  readonly detail: MarginAccountDetail;
  readonly assets: readonly Asset[];
  readonly unit: string;
}): ReactNode {
  const buy = useLeveragedBuy();
  const { detail } = props;
  const choices =
    detail.isolated_asset_id !== undefined
      ? [detail.isolated_asset_id]
      : simulatedAssets(props.assets)
          .map((asset) => asset.id)
          .filter((id) => id.toLowerCase() !== detail.numeraire_asset_id.toLowerCase());
  const [assetId, setAssetId] = useState("");
  const [amount, setAmount] = useState("");
  const quantity = assetId === "" ? ({ ok: false, reason: "Choose an asset." } as const) : prepareQuantity(amount, assetById(props.assets, assetId));
  const prepared: Prepared<CollateralRequest> = quantity.ok
    ? { ok: true, request: { accountId: detail.account_id, assetId, quantity: quantity.request } }
    : quantity;
  const decimals = detail.evaluation.numeraire_decimals;
  return (
    <form
      className="form"
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <AssetSelect
        id="margin-buy-asset"
        label="Asset to buy on margin"
        assets={props.assets}
        choices={choices}
        value={assetId}
        onChange={setAssetId}
      />
      <AmountInput
        id="margin-buy-amount"
        label="Quantity"
        value={amount}
        onChange={setAmount}
        help="The whole cost is borrowed; the buy fills on paper at the mark plus the disclosed slippage, and the asset becomes collateral."
      />
      <CommandFlow<CollateralRequest, MarginLeveragedBuyResult>
        label="Buy on margin"
        prepared={prepared}
        command={buy}
        refusedNote="Nothing was borrowed or bought."
        describe={(request) => (
          <FieldGrid columns={2}>
            <Field label="Asset">
              <AssetName assets={props.assets} id={request.assetId} />
            </Field>
            <Field label="Quantity">
              <AssetQty assets={props.assets} id={request.assetId} value={request.quantity} />
            </Field>
          </FieldGrid>
        )}
        renderResult={(result) => (
          <ResultNote replayed={result.replayed} status={result.status}>
            Cost borrowed <Qty value={result.cost} decimals={decimals} symbol={props.unit} />
            {result.slippage_bps !== undefined && <> at the mark plus {String(result.slippage_bps)} bps of disclosed slippage</>}
            ; trade journal <code className="mono-small">{result.trade_journal_transaction_id}</code>.
          </ResultNote>
        )}
      />
    </form>
  );
}

function CloseForm(props: { readonly detail: MarginAccountDetail }): ReactNode {
  const close = useCloseMarginAccount();
  return (
    <CommandFlow<{ readonly accountId: string }, MarginAccount>
      label="Close the margin account"
      variant="danger"
      prepared={{ ok: true, request: { accountId: props.detail.account_id } }}
      command={close}
      refusedNote="The margin account was not closed. The backend closes only an account that holds nothing and owes the lending pool nothing."
      describe={() => <p>Close margin account {props.detail.id}.</p>}
      renderResult={(account) => (
        <p className="note" role="status">
          The backend answered: status <Pill tone={accountStatusTone(account.status)}>{account.status}</Pill>.
        </p>
      )}
    />
  );
}

/** A command's outcome, saying when the service found it already applied. */
function ResultNote(props: { readonly replayed: boolean; readonly status: string; readonly children: ReactNode }): ReactNode {
  return (
    <p className="note" role="status">
      {props.replayed
        ? "The backend found this request already applied and applied nothing again. "
        : "Applied. "}
      {props.children} The account is now <Pill tone={accountStatusTone(props.status)}>{props.status}</Pill>.
    </p>
  );
}

/**
 * BUYING POWER V2 (advanced goal §20): what an account's simulated holdings
 * can do — trade, add risk, use margin, withdraw — with every component of
 * the answer and the backend's own explanation of it, line by line.
 *
 * The page formats; it never computes. It does not add the explanation lines
 * up to check them against their totals: arithmetic on money is the
 * backend's (PART 110), and the lines are shown so that a reader can check.
 *
 * When the backend cannot value the account it says why, and the page shows
 * that refusal with its code instead of figures. In a deployment that has not
 * provisioned the sandbox dollar — the unit every figure here is stated in —
 * that refusal is NOT_FOUND, and an account the backend cannot value is not an
 * account worth zero, so no zero is drawn.
 */
import type { ReactNode } from "react";

import {
  problemCode,
  useBuyingPowerV2,
  type BuyingPowerV2,
  type BuyingPowerV2Line,
} from "../../../api/advanced/core.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { BUYING_POWER_V2_GROUPS, componentLabel, groupByField } from "../../../lib/advanced.ts";
import { useAdvancedAccount } from "../area.ts";
import { NoAccount, ScrollTable, ValueDomainPill } from "../shared.tsx";

export function BuyingPowerSection(): ReactNode {
  const { accountId } = useAdvancedAccount();
  const buyingPower = useBuyingPowerV2(accountId);

  return (
    <Page
      title="Buying power V2"
      lead="What this account's simulated holdings can do, computed by the backend for each request and explained line by line."
    >
      <SimulatedNotice />

      {accountId === undefined ? (
        <Panel title="Buying power V2">
          <NoAccount />
        </Panel>
      ) : buyingPower.isSuccess ? (
        <Breakdown data={buyingPower.data} />
      ) : (
        <Panel title="Buying power V2" description="As the backend computes it for this account.">
          <AsyncPanel
            query={buyingPower}
            loadingLabel="Asking the backend for buying power V2…"
            errorExtra={refusalNote(buyingPower.error)}
          >
            {() => null}
          </AsyncPanel>
        </Panel>
      )}
    </Page>
  );
}

/** What a NOT_FOUND means on this page. The backend's own words are shown above it. */
function refusalNote(error: unknown): ReactNode {
  if (problemCode(error) !== "NOT_FOUND") return undefined;
  return (
    <p className="explain-body">
      NOT_FOUND here means the backend could not find something buying power V2 needs: the sandbox dollar every
      figure is stated in, or the account itself. Its own words above say which. No figure is shown in place of
      the answer, because an account the backend cannot value is not an account worth zero.
    </p>
  );
}

function Breakdown(props: { readonly data: BuyingPowerV2 }): ReactNode {
  const { data } = props;
  return (
    <>
      {BUYING_POWER_V2_GROUPS.map((group) => (
        <Panel key={group.title} title={group.title} description={group.description}>
          <FieldGrid columns={group.emphasis ? 4 : 3}>
            {group.components.map((component) => (
              <Field key={component.field} label={component.label} note={component.note} emphasis={group.emphasis}>
                <span data-field={component.field}>
                  <Usd value={data[component.field]} />
                </span>
              </Field>
            ))}
          </FieldGrid>
          {group.emphasis && (
            <>
              <p className="meta-row">
                <ValueDomainPill value={data.value_domain} />
                <span className="mono-small">policy {data.policy_version}</span>
              </p>
              <AsOf at={data.as_of} label="Computed at" />
            </>
          )}
        </Panel>
      ))}

      <Panel
        title="How collateral and equity are made up"
        description="The backend's own explanation lines, in the order it sent them, grouped by the figure each one contributes to. They are shown, not re-added: the arithmetic is the backend's."
      >
        {data.lines.length === 0 ? (
          <EmptyState
            title="No explanation lines"
            body="The backend sent no explanation lines with this answer, so there is nothing to show beneath the figures above."
          />
        ) : (
          groupByField(data.lines).map((group) => (
            <LineGroup key={group.field} field={group.field} lines={group.lines} />
          ))
        )}
      </Panel>

      <Panel
        title="Reasons"
        description="Machine-readable statements the backend attached to this answer — a haircut applied, capital reserved, an account block — each with the amount it concerns."
      >
        {data.reasons.length === 0 ? (
          <EmptyState title="No reasons attached" body="The backend attached no reason codes to this answer." />
        ) : (
          <ScrollTable caption="Reason codes attached to this answer" headers={["Code", "Detail", "Amount"]}>
            {data.reasons.map((reason, index) => (
              <tr key={`${reason.code}-${String(index)}`}>
                <th scope="row">
                  <code className="mono-small">{reason.code}</code>
                </th>
                <td className="cell-prose">{reason.detail}</td>
                <td>
                  <Usd value={reason.amount} />
                </td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </Panel>
    </>
  );
}

function LineGroup(props: { readonly field: string; readonly lines: readonly BuyingPowerV2Line[] }): ReactNode {
  const label = componentLabel(props.field);
  return (
    <>
      <h3>{label}</h3>
      <ScrollTable caption={`Lines making up ${label}`} headers={["Component", "Asset", "Amount", "Detail"]}>
        {props.lines.map((line, index) => (
          <tr key={`${props.field}-${String(index)}`}>
            <th scope="row">
              <code className="mono-small">{line.component}</code>
            </th>
            <td>
              {line.asset_id === undefined || line.asset_id === "" ? (
                <span className="absent">not asset-specific</span>
              ) : (
                <Identifier value={line.asset_id} />
              )}
            </td>
            <td>
              <Usd value={line.amount_usd} />
            </td>
            <td className="cell-prose">{line.detail}</td>
          </tr>
        ))}
      </ScrollTable>
    </>
  );
}

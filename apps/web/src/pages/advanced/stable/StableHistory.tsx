/**
 * The account's issuance and redemption requests, newest first, and each one
 * on its own page. A request read by its id is answered by the backend only
 * to someone who may read its account; anyone else gets NOT_FOUND, which the
 * page shows with its code.
 */
import { useState, type ReactNode } from "react";
import { useParams } from "react-router-dom";

import type { CursorPage } from "../../../api/advanced/core.ts";
import {
  useStableIssuance,
  useStableIssuances,
  useStableRedemption,
  useStableRedemptions,
  type StableIssuance,
  type StableRedemption,
} from "../../../api/advanced/stable.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount } from "../shared.tsx";
import { StableNav } from "./parts.tsx";
import { IssuanceFields, IssuanceTable, RedemptionFields, RedemptionTable } from "./requests.tsx";
import { stableHref } from "./paths.ts";

export function IssuancesPage(): ReactNode {
  const account = useAdvancedAccount();
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const page = useStableIssuances(account.accountId, trail.current);
  return (
    <Page title="Stable asset issuances" lead="The account's issuance (mint) requests, newest first, each with its status and reconciliation state.">
      <SimulatedNotice />
      <StableNav />
      <Panel title="Issuance requests" description="As the stable asset controller recorded them. A request whose submission outcome is unknown is shown as unknown.">
        {account.accountId === undefined ? (
          <NoAccount />
        ) : (
          <AsyncPanel
            query={page}
            loadingLabel="Loading the issuance requests…"
            empty={{
              isEmpty: (data: CursorPage<StableIssuance>) => data.items.length === 0 && trail.previous.length === 0,
              title: "No issuance requests",
              body: "The backend reports no issuance request for this account.",
            }}
          >
            {(data: CursorPage<StableIssuance>) => (
              <>
                <IssuanceTable items={data.items} />
                <CursorPager
                  noun="issuances"
                  trail={trail}
                  nextCursor={data.nextCursor}
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
        )}
      </Panel>
    </Page>
  );
}

export function IssuanceDetailPage(): ReactNode {
  const { issuanceId } = useParams();
  const account = useAdvancedAccount();
  const issuance = useStableIssuance(account.accountId, issuanceId);
  return (
    <Page
      title="Issuance request"
      lead="One issuance request: the units, the payment, the simulated chain transaction, and where the request stands."
      actions={<LinkButton to={stableHref("issuances")}>All issuances</LinkButton>}
    >
      <SimulatedNotice />
      <StableNav />
      <Panel title="The request">
        <AsyncPanel query={issuance} loadingLabel="Loading the issuance request…">
          {(data: StableIssuance) => <IssuanceFields issuance={data} />}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

export function RedemptionsPage(): ReactNode {
  const account = useAdvancedAccount();
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const page = useStableRedemptions(account.accountId, trail.current);
  return (
    <Page
      title="Stable asset redemptions"
      lead="The account's redemption (burn) requests, newest first, each with its status and reconciliation state."
    >
      <SimulatedNotice />
      <StableNav />
      <Panel title="Redemption requests" description="As the stable asset controller recorded them. A request whose submission outcome is unknown is shown as unknown.">
        {account.accountId === undefined ? (
          <NoAccount />
        ) : (
          <AsyncPanel
            query={page}
            loadingLabel="Loading the redemption requests…"
            empty={{
              isEmpty: (data: CursorPage<StableRedemption>) => data.items.length === 0 && trail.previous.length === 0,
              title: "No redemption requests",
              body: "The backend reports no redemption request for this account.",
            }}
          >
            {(data: CursorPage<StableRedemption>) => (
              <>
                <RedemptionTable items={data.items} />
                <CursorPager
                  noun="redemptions"
                  trail={trail}
                  nextCursor={data.nextCursor}
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
        )}
      </Panel>
    </Page>
  );
}

export function RedemptionDetailPage(): ReactNode {
  const { redemptionId } = useParams();
  const account = useAdvancedAccount();
  const redemption = useStableRedemption(account.accountId, redemptionId);
  return (
    <Page
      title="Redemption request"
      lead="One redemption request: the units burned, the payout, the simulated chain transaction, and where the request stands."
      actions={<LinkButton to={stableHref("redemptions")}>All redemptions</LinkButton>}
    >
      <SimulatedNotice />
      <StableNav />
      <Panel title="The request">
        <AsyncPanel query={redemption} loadingLabel="Loading the redemption request…">
          {(data: StableRedemption) => <RedemptionFields redemption={data} />}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

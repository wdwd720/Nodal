/**
 * STRATEGIES: the strategy marketplace of the advanced area
 * (internal/marketplace) and the sandbox its strategies run in
 * (internal/sandboxrt).
 *
 * A strategy is source code that runs in a WebAssembly sandbox and can only
 * ever PROPOSE an intent. Every proposal its manifest permits becomes an
 * economic intent that faces routing and Risk Kernel V2 like any other, and
 * installs run in PAPER mode only: the marketplace never allocates anything
 * but simulated value. Listings are reviewed by an operator who is not their
 * creator before anybody else can see or install them.
 *
 * Pages: the catalogue and the account's own listings (here), one listing
 * with every version, the forms that publish, version and fork a strategy,
 * the account's installs with their paper-follow history, the account's
 * sandbox runs, and an operator's review queue. Every figure on them comes
 * from the API.
 */
import { useState, type ReactNode } from "react";
import { Link, Route, Routes } from "react-router-dom";

import {
  useAccountListings,
  useStrategyCatalog,
  type StrategyListing,
} from "../../../api/advanced/marketplace.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel, Pill, Table } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { MODERATION_MEANING, STATUS_MEANING, STRATEGY_MODERATIONS, STRATEGY_STATUSES, moderationTone, statusTone } from "../../../lib/marketplace.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { AdvancedNotFound, Codes, CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { InstallsPage } from "./Installs.tsx";
import { ListingDetailPage } from "./ListingDetail.tsx";
import { ReviewQueuePage } from "./ReviewQueue.tsx";
import { RunsPage } from "./Runs.tsx";
import { VersionState, strategyHref } from "./parts.tsx";
import { CreateListingPage, ForkPage, NewVersionPage } from "./VersionForm.tsx";

/** The section: the catalogue, a listing, the forms, installs, runs and the review queue. */
export function StrategiesSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<StrategiesHome />} />
      <Route path="new" element={<CreateListingPage />} />
      <Route path="listings/:listingId" element={<ListingDetailPage />} />
      <Route path="listings/:listingId/versions/new" element={<NewVersionPage />} />
      <Route path="versions/:versionId/fork" element={<ForkPage />} />
      <Route path="installs" element={<InstallsPage />} />
      <Route path="runs" element={<RunsPage />} />
      <Route path="review" element={<ReviewQueuePage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}

function StrategiesHome(): ReactNode {
  const { accountId, canInspect } = useAdvancedAccount();
  return (
    <Page
      title="Strategies"
      lead="Sandboxed strategies others have published and an operator approved, and the ones this account published. A strategy only ever proposes intents, and installs run in PAPER mode only."
      actions={
        <>
          <LinkButton to={strategyHref.create()} variant="primary">
            Publish a strategy
          </LinkButton>
          <LinkButton to={strategyHref.installs()}>Installs and paper-follow</LinkButton>
          <LinkButton to={strategyHref.runs()}>Sandbox runs</LinkButton>
          {canInspect && <LinkButton to={strategyHref.review()}>Review queue</LinkButton>}
        </>
      }
    >
      <SimulatedNotice />
      <CatalogPanel />
      {accountId === undefined ? (
        <Panel title="Your listings">
          <NoAccount />
        </Panel>
      ) : (
        // Keyed by account: switching account starts the list again at its first page.
        <AccountListingsPanel key={`listings-${accountId}`} accountId={accountId} />
      )}
      <LifecycleGlossary />
    </Page>
  );
}

function CatalogPanel(): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const catalog = useStrategyCatalog(trail.current);
  return (
    <Panel
      title="Approved strategies"
      description="Listings with a version an operator other than its creator approved and cleared, newest first, as the backend lists them."
    >
      <AsyncPanel
        query={catalog}
        loadingLabel="Loading the approved strategies…"
        empty={{
          isEmpty: (page: CursorPage<StrategyListing>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No approved strategies",
          body: "The backend lists no approved strategy. That is an empty catalogue, not an error.",
        }}
      >
        {(page: CursorPage<StrategyListing>) => (
          <>
            <ListingTable caption="Approved strategies on this page" items={page.items} />
            <CursorPager
              noun="strategies"
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

function AccountListingsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const listings = useAccountListings(props.accountId, trail.current);
  return (
    <Panel
      title="Your listings"
      description="Every listing this account created, in every status, with its newest version. Drafts and rejected versions are visible to this account and to operators only."
    >
      <AsyncPanel
        query={listings}
        loadingLabel="Loading your listings…"
        empty={{
          isEmpty: (page: CursorPage<StrategyListing>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No listings",
          body: "This account has published no strategy.",
        }}
      >
        {(page: CursorPage<StrategyListing>) => (
          <>
            <ListingTable caption="Your listings on this page" items={page.items} />
            <CursorPager
              noun="listings"
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

function ListingTable(props: { readonly caption: string; readonly items: readonly StrategyListing[] }): ReactNode {
  return (
    <ScrollTable caption={props.caption} headers={["Strategy", "Version", "State", "Risk class", "Description", "Listed"]}>
      {props.items.map((listing) => {
        const v = listing.latest_version;
        return (
          <tr key={listing.id}>
            <th scope="row">
              <Link to={strategyHref.listing(listing.id)}>{listing.title}</Link>
              {listing.forked_from_version_id !== undefined && <span className="cell-note">a fork</span>}
            </th>
            <td>
              version {String(v.number)}
              <span className="cell-note">
                <code className="mono-small">{v.strategy_ref}</code>
              </span>
            </td>
            <td>
              <VersionState status={v.status} moderation={v.moderation} />
              {v.rejection_code !== undefined && <Codes codes={[v.rejection_code]} />}
            </td>
            <td>{v.risk_class}</td>
            <td className="cell-prose">{v.description}</td>
            <td>{formatInstant(listing.created_at)}</td>
          </tr>
        );
      })}
    </ScrollTable>
  );
}

function LifecycleGlossary(): ReactNode {
  return (
    <Panel
      title="What the states mean"
      description="The lifecycle is enforced by the backend and its database: a version moves only along these edges, and an approval needs a review by an operator who is not its creator."
    >
      <Table caption="Version statuses and their meaning" headers={["Status", "What it means"]}>
        {STRATEGY_STATUSES.map((status) => (
          <tr key={status}>
            <th scope="row">
              <Pill tone={statusTone(status)}>{status}</Pill>
            </th>
            <td className="cell-prose">{STATUS_MEANING[status]}</td>
          </tr>
        ))}
      </Table>
      <Table caption="Moderation states and their meaning" headers={["Moderation", "What it means"]}>
        {STRATEGY_MODERATIONS.map((moderation) => (
          <tr key={moderation}>
            <th scope="row">
              <Pill tone={moderationTone(moderation)}>{moderation}</Pill>
            </th>
            <td className="cell-prose">{MODERATION_MEANING[moderation]}</td>
          </tr>
        ))}
      </Table>
    </Panel>
  );
}

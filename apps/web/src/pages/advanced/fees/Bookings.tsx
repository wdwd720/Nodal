/**
 * FEES — the fees charged to this account.
 *
 * Every booking charges one disclosure to its payer, every component on its
 * own pair of accounts, exactly once. The list shows each booking with its
 * spread line — the schedule version that priced it, the reference and
 * execution prices, the spread rate and amount — and its charges by
 * component. A booking read by its id carries its whole disclosure and the
 * backend's end-to-end re-proof: the disclosure still verifies against its
 * schedule, the booking's amounts are the disclosure's, and its journal
 * carries exactly those charges.
 *
 * Operators additionally reverse a booking with a reason code, by a
 * compensating journal of exactly its charges, with a recent strong sign-in.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useFeeBooking,
  useFeeBookings,
  useReverseBooking,
  type FeeBooking,
  type FeeBookingDetail,
  type ReverseRequest,
} from "../../../api/advanced/fees.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { useAssets, type Asset } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { BOOKING_MODE_MEANING, BOOKING_STATUS_MEANING, bookingStatusTone, parseReasonCode } from "../../../lib/fees.ts";
import { assetById } from "../../../lib/margin.ts";
import { parseText } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount } from "../shared.tsx";
import { CommandFlow, CommandForm, TextInput, prepare } from "../treasury/command.tsx";
import { ChargesView, DisclosureView, FeesNav, RecordIds, SpreadLineView, VerificationView, feesHref, useIsOperator, type QuoteUnit } from "./parts.tsx";

/** The unit a booking's quote-asset charges are in: the registry's decimals, or the disclosure's when it is at hand. */
function unitOf(assets: readonly Asset[], booking: FeeBooking, decimals?: number): QuoteUnit | undefined {
  const asset = assetById(assets, booking.charges.quote_asset_id);
  if (decimals !== undefined) return { decimals, symbol: asset?.symbol };
  if (asset === undefined) return undefined;
  return { decimals: asset.decimals, symbol: asset.symbol };
}

function BookingSummary(props: { readonly booking: FeeBooking; readonly assets: readonly Asset[]; readonly decimals?: number }): ReactNode {
  const { booking } = props;
  const unit = unitOf(props.assets, booking, props.decimals);
  return (
    <>
      <p className="meta-row">
        <span data-field="booking_status">
          <Pill tone={bookingStatusTone(booking.status)}>{booking.status}</Pill>
        </span>
        <span className="mono-small">{booking.booking_mode}</span>
        <span className="mono-small">{booking.journal_kind}</span>
        <span className="mono-small">{booking.mode}</span>
        <span className="mono-small">booked {formatInstant(booking.booked_at)}</span>
      </p>
      <p className="field-note">
        {BOOKING_STATUS_MEANING[booking.status]} {BOOKING_MODE_MEANING[booking.booking_mode]}
      </p>
      {booking.status === "REVERSED" && (
        <p className="note" role="status">
          Reversed {booking.reversed_at === undefined ? "" : formatInstant(booking.reversed_at)} with reason code{" "}
          <code className="mono-small">{booking.reversal_reason_code ?? "not stated"}</code>
          {booking.reversal_journal_transaction_id !== undefined && (
            <>
              {" "}
              by journal <code className="mono-small">{booking.reversal_journal_transaction_id}</code>
            </>
          )}
          .
        </p>
      )}
      <h3>Spread line</h3>
      <SpreadLineView line={booking.spread_line} assets={props.assets} />
      <h3>Charged, component by component — simulated</h3>
      {unit === undefined ? (
        <p className="note" role="status">
          The asset registry does not list the quote asset <code className="mono-small">{booking.charges.quote_asset_id}</code>, so the charges are
          shown on the booking's own page, with its disclosure.
        </p>
      ) : (
        <ChargesView charges={booking.charges} unit={unit} assets={props.assets} />
      )}
    </>
  );
}

export function FeeBookingsPage(): ReactNode {
  const account = useAdvancedAccount();
  return (
    <Page title="Fee bookings" lead="Every fee and spread charged to this account, each traceable to the disclosure and schedule version that priced it.">
      <SimulatedNotice />
      <FeesNav />
      {account.accountId === undefined ? <NoAccount /> : <BookingsPanel accountId={account.accountId} />}
    </Page>
  );
}

function BookingsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const bookings = useFeeBookings(props.accountId, trail.current);
  const assets = useAssets();
  return (
    <Panel title="This account's fee bookings" description="Newest first. A reversed booking stays on the list, marked, with the journal that returned its charges.">
      <AsyncPanel
        query={bookings}
        loadingLabel="Loading the fee bookings…"
        empty={{
          isEmpty: (page: CursorPage<FeeBooking>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No fees charged",
          body: "The backend has booked no fee or spread to this account.",
        }}
      >
        {(page: CursorPage<FeeBooking>) => (
          <>
            {page.items.map((booking) => (
              <section key={booking.id} className="callout" aria-label={`Fee booking ${booking.id}`} data-booking={booking.id}>
                <h3>
                  <Link to={feesHref("bookings", booking.id)}>Booking {formatInstant(booking.booked_at)}</Link>
                </h3>
                <BookingSummary booking={booking} assets={assets.data ?? []} />
              </section>
            ))}
            <CursorPager
              noun="bookings"
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

/** One booking, its disclosure and its end-to-end re-proof. A booking of another account answers NOT_FOUND. */
export function FeeBookingPage(): ReactNode {
  const { bookingId = "" } = useParams();
  const account = useAdvancedAccount();
  const booking = useFeeBooking(account.accountId, bookingId);
  const assets = useAssets();
  const operator = useIsOperator();
  return (
    <Page title="Fee booking" lead="One charge: its disclosure, its spread line, its charges by component, and the backend's re-proof of all three against the journal.">
      <SimulatedNotice />
      <FeesNav />
      <AsyncPanel query={booking} loadingLabel="Loading the booking…">
        {(detail: FeeBookingDetail) => (
          <>
            <Panel title="The booking" description="Re-proved end to end: the disclosure verifies against its schedule, the amounts are the disclosure's, and the journal carries exactly them.">
              <VerificationView verification={detail.verification} />
              <FieldGrid columns={3}>
                <Field label="Payer">
                  <Identifier value={detail.account_id} />
                </Field>
                <Field label="Disclosure">
                  <Link to={feesHref("quotes", detail.disclosure_id)}>{detail.disclosure_id}</Link>
                </Field>
                <Field label="Journal">
                  <Identifier value={detail.journal_transaction_id} />
                </Field>
              </FieldGrid>
              <BookingSummary booking={detail} assets={assets.data ?? []} decimals={detail.disclosure.input.quote_decimals} />
              <RecordIds ids={[["booking", detail.id], ["effect", detail.effect_id], ["intent", detail.intent_id], ["evidence", detail.evidence_ref]]} />
            </Panel>
            <Panel title="The disclosure it charged" description="Exactly as the fee engine itemized it when the order was priced.">
              <DisclosureView disclosure={detail.disclosure} />
            </Panel>
            {operator && detail.status === "BOOKED" && <ReversePanel bookingId={detail.id} />}
          </>
        )}
      </AsyncPanel>
    </Page>
  );
}

function ReversePanel(props: { readonly bookingId: string }): ReactNode {
  const reverse = useReverseBooking();
  const [code, setCode] = useState("");
  const [reason, setReason] = useState("");
  const c = parseReasonCode(code);
  const r = parseText(reason, "The reason", 500);
  const prepared = prepare<ReverseRequest>([c.ok ? undefined : c.error, r.ok ? undefined : r.error], () => ({
    id: props.bookingId,
    reasonCode: c.ok ? c.value : "",
    reason: r.ok ? r.value : "",
  }));
  return (
    <Panel
      title="Reverse this booking"
      description="Returns exactly the booking's charges by a compensating journal with a reason code; the booking becomes REVERSED and its history is kept. It moves simulated value, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Reverse this booking">
        <TextInput id="fees-reverse-code" label="Reason code" value={code} onChange={setCode} note="Upper-case letters, digits and underscores." />
        <TextInput id="fees-reverse-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<ReverseRequest, FeeBooking>
          label="Reverse the booking"
          variant="danger"
          prepared={prepared}
          command={reverse}
          refusedNote="The booking is unchanged."
          describe={(request) => (
            <p>
              Reverse booking <code className="mono-small">{request.id}</code> with <code className="mono-small">{request.reasonCode}</code>:{" "}
              {request.reason}
            </p>
          )}
          renderResult={(booking) => (
            <p>
              <Pill tone={bookingStatusTone(booking.status)}>{booking.status}</Pill> reason code{" "}
              <code className="mono-small">{booking.reversal_reason_code ?? "not stated"}</code>
            </p>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

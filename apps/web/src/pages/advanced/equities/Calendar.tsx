/**
 * THE CALENDAR: the session in force on the XNYS calendar, the settlement
 * cycle and every recorded closure; and, for an operator, the closures they
 * add and the two worker steps this deployment runs only when asked — the
 * venue run (accepted orders placed, resting ones polled) and the settlement
 * run (everything due today settled, one account per transaction).
 */
import type { ReactNode } from "react";

import { useEquityCalendar } from "../../../api/advanced/equities.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Page, Panel } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { useAdvancedAccount } from "../area.ts";
import { ScrollTable } from "../shared.tsx";
import { AddHolidayForm, OperatorOnly, SettlementRunForm, VenueRunForm } from "./forms.tsx";
import { EquitiesNav, SessionFields } from "./parts.tsx";

export function CalendarPage(): ReactNode {
  const { canInspect } = useAdvancedAccount();
  const calendar = useEquityCalendar();
  return (
    <Page title="Equities calendar" lead="When the simulated equity market trades and when its trades settle.">
      <SimulatedNotice />
      <EquitiesNav />
      <AsyncPanel query={calendar} loadingLabel="Loading the calendar…">
        {(c) => (
          <>
            <Panel title="Now" description="The session the backend computes for this instant.">
              <SessionFields session={c.state} />
              <FieldGrid columns={2}>
                <Field label="Settlement cycle" note="Trading days between a trade and its settlement.">
                  T+{String(c.settlement_cycle_days)}
                </Field>
              </FieldGrid>
            </Panel>
            <Panel title="Closures" description="Full-day closures recorded for the calendar; weekends are closed as well.">
              {c.holidays.length === 0 ? (
                <EmptyState title="No recorded closures" body="The backend lists no full-day closure; only weekends are closed." />
              ) : (
                <ScrollTable caption="Recorded closures" headers={["Date", "Name"]}>
                  {c.holidays.map((h) => (
                    <tr key={h.date}>
                      <th scope="row">{h.date}</th>
                      <td>{h.name}</td>
                    </tr>
                  ))}
                </ScrollTable>
              )}
            </Panel>
          </>
        )}
      </AsyncPanel>
      {canInspect && (
        <>
          <Panel title="Record a closure" description="The market is closed on it all day: orders are refused and nothing settles on it.">
            <OperatorOnly>
              <AddHolidayForm />
            </OperatorOnly>
          </Panel>
          <Panel
            title="Venue run"
            description="Places every accepted order at the paper venue and polls every resting one, applying each answer exactly once."
          >
            <OperatorOnly>
              <VenueRunForm />
            </OperatorOnly>
          </Panel>
          <Panel
            title="Settlement run"
            description="Settles everything due on today's trade date: shares and proceeds move from unsettled into the wallet."
          >
            <OperatorOnly>
              <SettlementRunForm />
            </OperatorOnly>
          </Panel>
        </>
      )}
    </Page>
  );
}

/**
 * Every route the built application answers, in one place.
 *
 * The cross-cutting suites — accessibility, honesty, no-dead-controls — each
 * used to keep their own copy of the route list, and each copy drifted at a
 * different speed. A route that exists but is missing from one of those lists
 * is a route nobody checks, which is the failure mode those suites exist to
 * prevent. One list, imported three times.
 *
 * Adding a page is one entry here, and then it is covered by axe, by the
 * forbidden-vocabulary scan, by the 375px reflow check and by the dead-control
 * walk without touching any of them.
 */

export interface RouteUnderTest {
  readonly path: string;
  /** The page's one `<h1>`. */
  readonly heading: string;
  /** The label of the navigation link that reaches it, where one exists. */
  readonly nav?: string;
}

/** D-077's public site. Rendered with no session at all. */
export const PUBLIC_ROUTES: readonly RouteUnderTest[] = [
  { path: "/", heading: "A control plane between your capital and markets." },
  { path: "/product", heading: "The product", nav: "Product" },
  { path: "/product/markets", heading: "Markets", nav: "Markets" },
  { path: "/product/agents", heading: "Agents", nav: "Agents" },
  { path: "/how-it-works", heading: "How it works", nav: "How it works" },
  { path: "/security", heading: "Security", nav: "Security" },
  { path: "/learn", heading: "Learn", nav: "Learn" },
  { path: "/get-started", heading: "Get started" },
  { path: "/sign-in", heading: "Sign in" },
  // The five legal documents, served by `GET /v1/terms` (D-080). The heading is
  // the page's own, so the address and the title agree before the fetch lands;
  // the document's title comes from the registry.
  { path: "/terms", heading: "Terms of Service" },
  { path: "/privacy", heading: "Privacy Notice" },
  { path: "/risk", heading: "Risk Disclosure" },
  { path: "/credits-terms", heading: "Credits Terms" },
  { path: "/withdrawal-disclosure", heading: "Withdrawal and Verification Disclosure" },
];

/**
 * The application. Every one of these needs a session, and a signed-out visitor
 * is sent to `/sign-in?return=` rather than shown an empty page.
 *
 * The hosted rail of the previous product — `/trade`, `/add-funds`, `/lab`,
 * `/strategy`, `/nodal-economy`, `/payouts`, `/marketplace` — was removed by
 * D-077 and is deliberately absent. The pages other branches own —
 * `/buy-credits`, `/withdraw`, `/verify`, `/notifications`, `/agents/new`,
 * `/settings/security`, `/settings/account` — are added here as they land.
 * `/markets/:marketId` has landed and is in `DYNAMIC_APP_ROUTES` below, because
 * it needs an identifier that only a scenario can supply.
 */
export const APP_ROUTES: readonly RouteUnderTest[] = [
  { path: "/home", heading: "Home", nav: "Home" },
  { path: "/markets", heading: "Markets", nav: "Markets" },
  { path: "/markets/products", heading: "Marketplace", nav: "Products" },
  { path: "/create-asset", heading: "Create asset", nav: "Create asset" },
  { path: "/agents", heading: "Agents", nav: "Agents" },
  // No `nav`: the create flow is reached from the agents page rather than the
  // rail, because it is an action and not a destination.
  { path: "/agents/new", heading: "Create an agent" },
  // No `nav`: Withdraw is a PRIMARY ACTION in the masthead rather than a
  // section in the rail, and `nav` names a link inside the "Sections"
  // navigation. It is shown to everybody — including the accounts that cannot
  // use it, which is the point of goal §19 — but it is not a destination.
  { path: "/withdraw", heading: "Withdraw" },
  // No `nav`: verification is reached from Withdraw, because that is the only
  // thing in the product that needs it — goal §19's "nothing else in the
  // product needs this" is a routing fact as much as a sentence.
  { path: "/verify", heading: "Verify your identity" },
  { path: "/portfolio", heading: "Portfolio", nav: "Portfolio" },
  { path: "/activity", heading: "Activity", nav: "Activity" },
  // No `nav`: Buy Credits is a primary action in the header rather than a rail
  // destination, which is where USER_JOURNEY §2 puts it.
  { path: "/buy-credits", heading: "Buy Credits" },
  // No `nav`: the shell reaches notifications through the bell and the account
  // menu, neither of which is the "Sections" navigation the nav walk uses.
  { path: "/notifications", heading: "Notifications" },
  // No `nav`: Settings is reached from the account menu rather than the rail,
  // which is where USER_JOURNEY §2 puts it.
  { path: "/settings", heading: "Settings and security" },
  { path: "/settings/security", heading: "Security" },
  { path: "/settings/account", heading: "Account" },
];

/**
 * The application routes whose address carries an identifier.
 *
 * They are listed apart from `APP_ROUTES` because the cross-cutting sweeps walk
 * that list by navigating to each `path`, and `/markets/:marketId` is not an
 * address — a sweep would ask the API for a market called ":marketId" and check
 * the 404 page. Each of these is covered instead by the scenario that owns it,
 * which has a real identifier to put in the gap: `scenarios/c-trade.spec.ts`
 * runs axe and the 375px reflow check against the market it opened.
 */
export const DYNAMIC_APP_ROUTES: readonly RouteUnderTest[] = [
  { path: "/markets/:marketId", heading: "the market symbol and name" },
];

/**
 * Onboarding. A session, but not the application shell and not the gate — a
 * person on these screens has not finished arriving, and the shell's
 * destinations are the ones the gate would send them back from.
 *
 * They are listed apart from `APP_ROUTES` because a check that walks the
 * application must not walk these: `/welcome` is a form, and `/welcome/terms`
 * reports whatever the API says is outstanding, which on an onboarded account
 * is nothing.
 */
export const ONBOARDING_ROUTES: readonly RouteUnderTest[] = [
  { path: "/welcome", heading: "Welcome" },
  { path: "/welcome/terms", heading: "What you are agreeing to" },
  // `/welcome/done` greets by display name, so its heading is not fixed and it
  // is checked by the scenario rather than by the route-list sweeps.
];

/** Everything, for a check that genuinely applies to every page. */
export const ALL_ROUTES: readonly RouteUnderTest[] = [...PUBLIC_ROUTES, ...ONBOARDING_ROUTES, ...APP_ROUTES];

/** The widths the design system claims to work at (UI_UX_SYSTEM §8). */
export const NARROW_WIDTH = 375;
export const NARROW_HEIGHT = 812;

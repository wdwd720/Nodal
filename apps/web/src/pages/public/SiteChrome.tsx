/**
 * The frame every signed-out page sits in.
 *
 * It is a second shell rather than a variant of `AppShell`, and that is the
 * right call rather than a duplication: the signed-out visitor has no session,
 * no account, no stream and no notion of Home; the application shell's entire
 * job — keeping three kinds of value apart in a navigation rail — is about
 * things this visitor does not have yet. Sharing one component would have meant
 * a shell full of `signedIn ?` branches, and every one of those is a chance to
 * render an account control to somebody with no account.
 *
 * What the two do share is everything that matters structurally: the same skip
 * link, one `<nav>`, one `<main id="main" tabindex="-1">`, one `<h1>` per page,
 * the same 768px breakpoint, and the same rule that only one navigation exists
 * in the document at a time — chosen in JavaScript, never hidden with CSS,
 * because a navigation that is merely invisible is still a navigation to a
 * screen reader.
 */
import { useEffect, useState, type ReactNode } from "react";
import { Link, NavLink, useLocation } from "react-router-dom";

import { API_ORIGIN } from "../../api/client.ts";
import { useVersion } from "../../api/queries.ts";
import { BrandLockup } from "../../components/Brand.tsx";
import { IconButton, LinkButton } from "../../components/Button.tsx";
import { Sheet } from "../../components/Dialog.tsx";
import { PUBLIC_BOUNDARY_LINE } from "../../content/site.ts";
import { RISK_FOOTER } from "../../lib/honesty.ts";

export interface SiteLink {
  readonly to: string;
  readonly label: string;
}

/**
 * The public sections, in the order goal §5 lists them.
 *
 * Every entry is a route that exists. There is no "Docs" here because there are
 * no docs to link to; `/learn` is what this product actually has, and naming it
 * something grander would be a link that disappoints.
 */
export const SITE_LINKS: readonly SiteLink[] = [
  { to: "/product", label: "Product" },
  { to: "/product/markets", label: "Markets" },
  { to: "/product/agents", label: "Agents" },
  { to: "/how-it-works", label: "How it works" },
  { to: "/security", label: "Security" },
  { to: "/learn", label: "Learn" },
];

const WIDE = "(min-width: 768px)";

function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState<boolean>(() => window.matchMedia(query).matches);
  useEffect(() => {
    const list = window.matchMedia(query);
    const onChange = (): void => {
      setMatches(list.matches);
    };
    onChange();
    list.addEventListener("change", onChange);
    return () => {
      list.removeEventListener("change", onChange);
    };
  }, [query]);
  return matches;
}

function linkClass({ isActive }: { isActive: boolean }): string {
  return isActive ? "site-link site-link-active" : "site-link";
}

function SiteHead(): ReactNode {
  const wide = useMediaQuery(WIDE);
  const [menuOpen, setMenuOpen] = useState(false);
  const location = useLocation();

  // A navigation that leaves the menu open behind it is a menu covering the
  // page you asked for.
  useEffect(() => {
    setMenuOpen(false);
  }, [location.pathname]);

  return (
    <header className="site-head">
      <div className="site-width site-head-inner">
        <Link to="/" aria-label="Nodal, home">
          <BrandLockup compact={!wide} />
        </Link>

        {wide ? (
          <nav className="site-head-links" aria-label="Sections">
            {SITE_LINKS.map((item) => (
              <NavLink key={item.to} to={item.to} className={linkClass} end={item.to === "/product"}>
                {item.label}
              </NavLink>
            ))}
          </nav>
        ) : (
          <IconButton
            label="Open the section list"
            expanded={menuOpen}
            onClick={() => {
              setMenuOpen(true);
            }}
          >
            <span aria-hidden="true">☰</span>
          </IconButton>
        )}

        <div className="site-head-actions">
          <LinkButton to="/sign-in" variant="quiet">
            Sign in
          </LinkButton>
          <LinkButton to="/get-started" variant="primary">
            Get started
          </LinkButton>
        </div>
      </div>

      {!wide && (
        <Sheet
          open={menuOpen}
          title="Sections"
          onClose={() => {
            setMenuOpen(false);
          }}
        >
          <nav aria-label="Sections">
            <ul className="site-menu-list">
              {SITE_LINKS.map((item) => (
                <li key={item.to}>
                  <NavLink to={item.to} className={linkClass} end={item.to === "/product"}>
                    {item.label}
                  </NavLink>
                </li>
              ))}
            </ul>
          </nav>
        </Sheet>
      )}
    </header>
  );
}

/**
 * The footer.
 *
 * Terms, Privacy, Risk disclosure and Status are there because the journey
 * requires them. **Contact is not**, and its absence is deliberate: no contact
 * address for this product exists anywhere in this repository, and a `mailto:`
 * to an address nobody reads is worse than no link at all. It appears here the
 * day an address exists.
 *
 * Status links to the API's own health endpoint rather than to a status page,
 * because the health endpoint is real and a status page would be a promise of
 * an operations practice that has not been described anywhere.
 */
function SiteFoot(): ReactNode {
  const version = useVersion();
  return (
    <footer className="site-foot">
      <div className="site-width">
        <div className="site-foot-grid">
          <div>
            <h2>Product</h2>
            <ul>
              {SITE_LINKS.map((item) => (
                <li key={item.to}>
                  <Link to={item.to}>{item.label}</Link>
                </li>
              ))}
            </ul>
          </div>
          <div>
            <h2>Start</h2>
            <ul>
              <li>
                <Link to="/get-started">Get started</Link>
              </li>
              <li>
                <Link to="/sign-in">Sign in</Link>
              </li>
            </ul>
          </div>
          <div>
            <h2>Legal</h2>
            <ul>
              <li>
                <Link to="/terms">Terms</Link>
              </li>
              <li>
                <Link to="/privacy">Privacy</Link>
              </li>
              <li>
                <Link to="/risk">Risk disclosure</Link>
              </li>
            </ul>
          </div>
          <div>
            <h2>Status</h2>
            <ul>
              <li>
                <a href={`${API_ORIGIN}/v1/healthz`} rel="noreferrer">
                  Service health
                </a>
              </li>
            </ul>
            {version.data !== undefined && (
              <p className="mono-small env-tag">
                {version.data.environment} · build {version.data.build_version}
              </p>
            )}
          </div>
        </div>

        <div className="site-foot-legal">
          <p>{PUBLIC_BOUNDARY_LINE}</p>
          <p>{RISK_FOOTER}</p>
          <p>
            Nodal is not an exchange, a broker, a bank or a custodian, and is not regulated,
            licensed or approved by anybody. Nothing on this site is advice.
          </p>
        </div>
      </div>
    </footer>
  );
}

/**
 * Wraps a public page. The page supplies its own `<h1>` and its sections; this
 * supplies the landmarks.
 */
export function SiteFrame(props: { readonly children: ReactNode }): ReactNode {
  return (
    <div className="site">
      <a className="skip-link" href="#main">
        Skip to main content
      </a>
      <SiteHead />
      <main id="main" tabIndex={-1} className="site-main">
        {props.children}
      </main>
      <SiteFoot />
    </div>
  );
}

/**
 * The head of a public page that is not the landing page: the one `<h1>`, a
 * lead, and any actions that belong with the title rather than with a section.
 */
export function SitePageHead(props: {
  readonly title: string;
  readonly lead: string;
  readonly actions?: ReactNode;
  readonly children?: ReactNode;
}): ReactNode {
  return (
    <section className="site-hero">
      <div className="site-width">
        <h1>{props.title}</h1>
        <p className="lead">{props.lead}</p>
        {props.actions !== undefined && <div className="site-cta">{props.actions}</div>}
        {props.children}
      </div>
    </section>
  );
}

/**
 * One ruled section of a public page.
 *
 * The heading level is fixed at `h2` because a public page has exactly one
 * `<h1>` — its title — and a section that wanted to be an `<h1>` is a page.
 */
export function SiteSection(props: {
  readonly title: string;
  readonly lead?: string;
  readonly actions?: ReactNode;
  readonly id?: string;
  readonly children: ReactNode;
}): ReactNode {
  return (
    <section className="site-section" {...(props.id === undefined ? {} : { id: props.id })}>
      <div className="site-width">
        <div className="site-section-head">
          <div>
            <h2>{props.title}</h2>
            {props.lead !== undefined && <p>{props.lead}</p>}
          </div>
          {props.actions !== undefined && <div className="site-cta">{props.actions}</div>}
        </div>
        {props.children}
      </div>
    </section>
  );
}

/** A titled block of copy inside a section grid. */
export function SiteItem(props: {
  readonly title: string;
  readonly children: ReactNode;
}): ReactNode {
  return (
    <div className="site-item">
      <h3>{props.title}</h3>
      {props.children}
    </div>
  );
}

/**
 * The is / is not lists.
 *
 * The glyph in front of each line is not decoration: it is the second channel
 * that keeps the two lists apart when colour is discarded by the operating
 * system in forced-colors mode, and it survives a grayscale screenshot.
 */
export function Claims(props: {
  readonly kind: "is" | "not";
  readonly items: readonly string[];
}): ReactNode {
  const glyph = props.kind === "is" ? "→" : "△";
  return (
    <ul className={props.kind === "is" ? "claims claims-is" : "claims claims-not"}>
      {props.items.map((item) => (
        <li key={item}>
          <span className="claims-glyph" aria-hidden="true">
            {glyph}
          </span>
          <span>{item}</span>
        </li>
      ))}
    </ul>
  );
}

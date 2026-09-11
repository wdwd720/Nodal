/**
 * The last thing between a thrown value and a blank page.
 *
 * React unmounts the whole tree when a render throws and nothing catches it.
 * There was nothing to catch it here, so any exception on any screen — a
 * recovered draft of the wrong shape reaching the BigInt constructor was the
 * one the audit found — replaced the entire application with an empty document:
 * no heading, no message, no control, and no indication that reloading would
 * not produce the same thing again. The one genuinely unrecoverable state this
 * product can reach, and it said less than any refusal in it.
 *
 * What this renders instead is the same `Explanation` every other failure gets,
 * so a fault in the interface reads the way a fault from the backend reads: a
 * sentence, a stable code, and a control. `explain()` already handles a plain
 * Error — it reports `UNREACHABLE` with the message — and the two actions
 * offered are the two that can actually help: try this screen again without
 * losing the session, or clear what this tab is holding, which is what makes a
 * poisoned stash recoverable without instructions nobody can find.
 *
 * # Why it is a class
 *
 * `componentDidCatch` has no hook equivalent. React has never shipped one, and
 * the third-party wrappers are all this file with a dependency in front of it.
 *
 * # What it does NOT do
 *
 * It does not retry by itself, and it does not report anywhere. A boundary that
 * re-renders the thing that just threw produces a loop that looks like a hung
 * page; a boundary that posts to a collector is a network call made from a
 * state where nothing about the application is known to be working. The error
 * goes to the console, where a developer can read it, and to the customer as a
 * sentence they can act on.
 */
import { Component, type ErrorInfo, type ReactNode } from "react";

import { Button } from "./Button.tsx";
import { Explanation } from "./DataState.tsx";
import { clearAllFormState } from "../lib/survives-sign-in.ts";

interface Props {
  readonly children: ReactNode;
  /** What failed, for the heading. Defaults to "This screen". */
  readonly what?: string;
}

interface State {
  readonly error: unknown;
}

export class ErrorBoundary extends Component<Props, State> {
  public override state: State = { error: undefined };

  public static getDerivedStateFromError(error: unknown): State {
    return { error };
  }

  public override componentDidCatch(error: unknown, info: ErrorInfo): void {
    // The console, and nowhere else. A component stack is the only thing that
    // makes this diagnosable and it exists nowhere else in the browser.
    // eslint-disable-next-line no-console
    console.error("render failed", error, info.componentStack);
  }

  public override render(): ReactNode {
    const { error } = this.state;
    if (error === undefined) return this.props.children;

    return (
      <div className="page">
        <h1>{this.props.what ?? "This screen"} could not be drawn</h1>
        <Explanation error={error}>
          <p className="explain-body">
            This is a fault in the interface rather than an answer from the backend. Nothing was
            sent and nothing on your account has changed by it.
          </p>
        </Explanation>
        <div className="form-actions">
          <Button
            variant="primary"
            onClick={() => {
              this.setState({ error: undefined });
            }}
          >
            Try this screen again
          </Button>
          <Button
            variant="secondary"
            onClick={() => {
              // The recoverable cause. A draft this tab is holding that the
              // page cannot read will throw again on every render and on every
              // reload, so the way out is to forget it — which costs a form
              // nobody can submit anyway.
              clearAllFormState();
              window.location.reload();
            }}
          >
            Forget what this tab has saved and reload
          </Button>
        </div>
      </div>
    );
  }
}

/**
 * A modal, and the same modal docked to an edge.
 *
 * Built on the native `<dialog>` element rather than on a div with a
 * hand-written focus trap, because the platform already gets the hard parts
 * right and gets them right in every browser at once: focus is trapped inside
 * while it is open, Escape closes it, the rest of the document is inert to
 * assistive technology, it renders in the top layer so no stacking context can
 * bury it, and focus returns to whatever opened it when it closes.
 *
 * What the platform does not do, and this adds:
 *
 *   - the page behind does not scroll while it is open;
 *   - Escape is routed back through React state, so the component that opened
 *     the dialog learns that it closed;
 *   - a click on the backdrop closes it, which is what a reader expects;
 *   - under 768px it becomes a bottom sheet, because that is where a thumb is.
 *
 * `Sheet` is the same component docked to the leading or trailing edge on a
 * wide viewport. It is what the navigation uses on a narrow one.
 */
import { useEffect, useRef, type ReactNode } from "react";

import { IconButton } from "./Button.tsx";

type Placement = "center" | "start" | "end";

interface Common {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly title: string;
  readonly children: ReactNode;
  /** Actions along the bottom edge. A dialog with no action is a notice. */
  readonly footer?: ReactNode;
}

function Modal(props: Common & { readonly placement: Placement }): ReactNode {
  const ref = useRef<HTMLDialogElement | null>(null);
  const { open, onClose } = props;

  useEffect(() => {
    const dialog = ref.current;
    if (dialog === null) return;
    if (open && !dialog.open) {
      dialog.showModal();
    } else if (!open && dialog.open) {
      dialog.close();
    }
  }, [open]);

  // The page behind a modal does not scroll. Without this a reader who scrolls
  // inside the sheet keeps scrolling the document underneath it and loses the
  // place they were reading.
  useEffect(() => {
    if (!open) return;
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previous;
    };
  }, [open]);

  const className =
    props.placement === "center"
      ? "dialog"
      : props.placement === "end"
        ? "dialog sheet sheet-end"
        : "dialog sheet";

  return (
    <dialog
      ref={ref}
      className={className}
      aria-label={props.title}
      onCancel={(event) => {
        // Escape. Routed through state so the opener knows, rather than letting
        // the element close itself behind React's back.
        event.preventDefault();
        onClose();
      }}
      onClose={onClose}
      onClick={(event) => {
        // The backdrop is the dialog element's own box outside its content, so
        // a click that lands on the element itself is a click on the backdrop.
        if (event.target === ref.current) onClose();
      }}
    >
      <div className="dialog-head">
        <h2>{props.title}</h2>
        <IconButton label="Close" onClick={onClose}>
          <span aria-hidden="true">✕</span>
        </IconButton>
      </div>
      <div className="dialog-body">{props.children}</div>
      {props.footer !== undefined && <div className="dialog-foot">{props.footer}</div>}
    </dialog>
  );
}

/** A centred modal on a wide viewport; a bottom sheet under 768px. */
export function Dialog(props: Common): ReactNode {
  return <Modal {...props} placement="center" />;
}

/** An edge sheet on a wide viewport; a bottom sheet under 768px. */
export function Sheet(props: Common & { readonly side?: "start" | "end" }): ReactNode {
  return <Modal {...props} placement={props.side ?? "start"} />;
}

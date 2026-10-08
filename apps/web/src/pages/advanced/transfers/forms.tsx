/**
 * The three commands of the transfers section: send simulated value to
 * another customer, request sandbox dollars from the faucet, register the
 * handle other customers pay an account by.
 *
 * Every one of them acts on the session's own account only -- the backend
 * refuses anything else -- and carries an idempotency key created when the
 * person confirms, reused for every retry of that same attempt. Changing the
 * request starts a new attempt with a new key: reusing a key for a different
 * request is refused by the backend (INVALID_IDEMPOTENCY_REUSE), and a retry
 * that silently became a second transfer is exactly what the key prevents.
 *
 * Nothing here checks a limit, a balance or a rule. The page sends what was
 * typed and shows the backend's answer: the transfer it recorded, or its
 * refusal with the stable code, the request id and, for a refusal it
 * recorded, the REJECTED transfer that now stands for it.
 */
import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useRegisterHandle,
  useRequestFaucetGrant,
  useSendTransfer,
  type AdvancedFaucetGrant,
  type AdvancedTransfer,
  type TransferMode,
} from "../../../api/advanced/transfers.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import {
  TRANSFER_MODES,
  TRANSFER_STATUS_MEANING,
  checkAccountId,
  checkAmount,
  checkHandle,
  confirmationKey,
  recordedRefusal,
  transferStatusTone,
  type InputCheck,
} from "../../../lib/transfers.ts";
import { advancedHref } from "../area.ts";
import { Codes, ModePill } from "../shared.tsx";

export function transferHref(transferId: string): string {
  return advancedHref("transfers", transferId);
}

/** The key of the attempt on screen, rendered where a person (and a test) can read it. */
function AttemptKey(props: { readonly attemptKey: string | undefined }): ReactNode {
  if (props.attemptKey === undefined) {
    return <p className="field-note">An idempotency key is created when you confirm, and reused if you retry.</p>;
  }
  return (
    <p className="mono-small" data-idempotency-key={props.attemptKey}>
      idempotency key {props.attemptKey} — created at confirmation; a retry of this attempt reuses it
    </p>
  );
}

/** A text field with its label, its note or problem, and the problem announced to assistive technology. */
function TextField(props: {
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly note: string;
  readonly check?: InputCheck;
  readonly inputMode?: "text" | "decimal";
  readonly multiline?: boolean;
}): ReactNode {
  const id = useId();
  const noteId = useId();
  const invalid = props.check !== undefined && props.value !== "" && !props.check.ok;
  const common = {
    id,
    className: "input",
    value: props.value,
    autoComplete: "off",
    spellCheck: false,
    "aria-describedby": noteId,
    ...(invalid ? { "aria-invalid": true } : {}),
  };
  return (
    <div className="form-row">
      <label htmlFor={id}>{props.label}</label>
      {props.multiline === true ? (
        <textarea
          {...common}
          rows={2}
          onChange={(event) => {
            props.onChange(event.target.value);
          }}
        />
      ) : (
        <input
          {...common}
          inputMode={props.inputMode ?? "text"}
          onChange={(event) => {
            props.onChange(event.target.value);
          }}
        />
      )}
      <p id={noteId} className={invalid ? "field-error" : "field-note"}>
        {invalid && props.check !== undefined ? props.check.error : props.note}
      </p>
    </div>
  );
}

/* --------------------------------------------------------------------------
 * Send
 * ------------------------------------------------------------------------ */

type RecipientKind = "handle" | "account";

/**
 * Send simulated value to another customer. Two steps: the request is
 * reviewed, then confirmed; the confirmation creates the idempotency key.
 * The recipient is named by handle where possible -- the backend resolves it
 * and never tells the sender the account behind it -- or by an account id the
 * sender already knows.
 */
export function SendForm(props: { readonly accountId: string; readonly symbol: string | undefined }): ReactNode {
  const unit = props.symbol ?? "sandbox dollars";
  const send = useSendTransfer();
  const [recipientKind, setRecipientKind] = useState<RecipientKind>("handle");
  const [handle, setHandle] = useState("");
  const [recipientAccount, setRecipientAccount] = useState("");
  const [amount, setAmount] = useState("");
  const [memo, setMemo] = useState("");
  const [mode, setMode] = useState<TransferMode>("SANDBOX");
  const [reviewing, setReviewing] = useState(false);
  const [attemptKey, setAttemptKey] = useState<string | undefined>(undefined);
  const modeId = useId();
  const kindName = useId();

  const recipient = recipientKind === "handle" ? checkHandle(handle) : checkAccountId(recipientAccount);
  const value = checkAmount(amount);
  const ready = recipient.ok && value.ok;

  /** Any change is a different request: it needs a new review and a new key. */
  const edit = (apply: () => void): void => {
    apply();
    setReviewing(false);
    setAttemptKey(undefined);
    send.reset();
  };

  const confirm = (): void => {
    if (!ready) return;
    const key = confirmationKey(attemptKey, newIdempotencyKey);
    setAttemptKey(key);
    send.mutate({
      accountId: props.accountId,
      amount: value.value,
      mode,
      idempotencyKey: key,
      ...(recipientKind === "handle" ? { recipientHandle: recipient.value } : { recipientAccountId: recipient.value }),
      ...(memo.trim() === "" ? {} : { memo }),
    });
  };

  const startOver = (): void => {
    setHandle("");
    setRecipientAccount("");
    setAmount("");
    setMemo("");
    setReviewing(false);
    setAttemptKey(undefined);
    send.reset();
  };

  if (send.isSuccess) {
    return (
      <div className="form">
        <SentResult transfer={send.data} />
        <AttemptKey attemptKey={attemptKey} />
        <div className="form-actions">
          <Button variant="primary" onClick={startOver}>
            Send another transfer
          </Button>
        </div>
      </div>
    );
  }

  if (reviewing) {
    return (
      <div className="form" role="group" aria-label="Review the transfer">
        <FieldGrid columns={3}>
          <Field label="From" note="Your own account. A transfer is only ever sent from an account the session owns.">
            <Identifier value={props.accountId} />
          </Field>
          <Field
            label="To"
            note={
              recipientKind === "handle"
                ? "A handle. The backend resolves it and does not show you the account behind it."
                : "An account id you already know."
            }
          >
            {recipientKind === "handle" ? <code className="mono-small">{recipient.value}</code> : <Identifier value={recipient.value} />}
          </Field>
          <Field label="Amount" note="Sent exactly as typed. The backend converts it at the asset's precision and never rounds.">
            <span className="num">{value.value}</span> {unit}
          </Field>
          <Field label="Mode" note={ADVANCED_MODE_DESCRIPTIONS[mode] ?? mode}>
            <ModePill mode={mode} />
          </Field>
          <Field label="Memo" note="Shown to you and the recipient only.">
            {memo.trim() === "" ? <span className="absent">none</span> : memo}
          </Field>
        </FieldGrid>
        <AttemptKey attemptKey={attemptKey} />
        <div className="form-actions">
          <Button variant="primary" onClick={confirm} busy={send.isPending} busyLabel="Sending to the backend…">
            Confirm and send
          </Button>
          <Button
            variant="quiet"
            onClick={() => {
              edit(() => undefined);
            }}
          >
            Change the transfer
          </Button>
        </div>
        {send.isError && <SendRefusal error={send.error} onRetry={confirm} />}
      </div>
    );
  }

  return (
    <form
      className="form"
      aria-label="Send simulated value"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        if (ready) setReviewing(true);
      }}
    >
      <fieldset className="form-row">
        <legend>Recipient named by</legend>
        <label className="radio">
          <input
            type="radio"
            name={kindName}
            checked={recipientKind === "handle"}
            onChange={() => {
              edit(() => {
                setRecipientKind("handle");
              });
            }}
          />{" "}
          handle (preferred: the account behind it stays on the server)
        </label>
        <label className="radio">
          <input
            type="radio"
            name={kindName}
            checked={recipientKind === "account"}
            onChange={() => {
              edit(() => {
                setRecipientKind("account");
              });
            }}
          />{" "}
          account id
        </label>
      </fieldset>
      {recipientKind === "handle" ? (
        <TextField
          label="Recipient handle"
          value={handle}
          check={recipient}
          note="The handle the recipient registered, such as @alice. An unknown handle is answered NOT_FOUND."
          onChange={(next) => {
            edit(() => {
              setHandle(next);
            });
          }}
        />
      ) : (
        <TextField
          label="Recipient account id"
          value={recipientAccount}
          check={recipient}
          note="The recipient's account id, in its canonical 36-character form."
          onChange={(next) => {
            edit(() => {
              setRecipientAccount(next);
            });
          }}
        />
      )}
      <TextField
        label={`Amount in ${unit}`}
        value={amount}
        check={value}
        inputMode="decimal"
        note="A plain decimal in whole units, sent exactly as typed. More decimal places than the asset holds is refused, never rounded."
        onChange={(next) => {
          edit(() => {
            setAmount(next);
          });
        }}
      />
      <TextField
        label="Memo (optional)"
        value={memo}
        multiline
        note="Seen by you and the recipient only. It never reaches a journal, an event or a notification."
        onChange={(next) => {
          edit(() => {
            setMemo(next);
          });
        }}
      />
      <div className="form-row">
        <label htmlFor={modeId}>Mode</label>
        <select
          id={modeId}
          className="input"
          value={mode}
          onChange={(event) => {
            const chosen = TRANSFER_MODES.find((candidate) => candidate === event.target.value);
            if (chosen !== undefined) {
              edit(() => {
                setMode(chosen);
              });
            }
          }}
        >
          {TRANSFER_MODES.map((candidate) => (
            <option key={candidate} value={candidate}>
              {candidate}
            </option>
          ))}
        </select>
        <p className="field-note">{ADVANCED_MODE_DESCRIPTIONS[mode] ?? mode}</p>
      </div>
      <div className="form-actions">
        {ready ? (
          <Button variant="primary" submit>
            Review the transfer
          </Button>
        ) : (
          <Button variant="primary" disabledReason={!recipient.ok ? recipient.error : value.error}>
            Review the transfer
          </Button>
        )}
      </div>
    </form>
  );
}

/** What the backend recorded for a send that moved value. */
function SentResult(props: { readonly transfer: AdvancedTransfer }): ReactNode {
  const { transfer } = props;
  return (
    <div className="callout" role="status" aria-label="The backend's answer">
      <p>
        The backend recorded transfer{" "}
        <Link to={transferHref(transfer.id)}>
          <code className="mono-small">{transfer.id}</code>
        </Link>{" "}
        as <Pill tone={transferStatusTone(transfer.status)}>{transfer.status}</Pill>
      </p>
      <p className="field-note">{TRANSFER_STATUS_MEANING[transfer.status]}</p>
      <p>
        <span data-field="sent-quantity">
          <Qty value={transfer.quantity} decimals={transfer.asset_decimals} symbol={transfer.asset_symbol} />
        </span>{" "}
        <BaseUnits value={transfer.quantity} />
      </p>
    </div>
  );
}

/**
 * A send that did not move value. When the backend recorded the refusal, the
 * REJECTED transfer it wrote is linked: the refusal is a fact with a record,
 * not an error that left nothing behind.
 */
function SendRefusal(props: { readonly error: unknown; readonly onRetry: () => void }): ReactNode {
  const recorded = recordedRefusal(props.error);
  return (
    <Explanation error={props.error} onRetry={props.onRetry} retryLabel="Try again with the same key">
      {recorded === undefined ? (
        <p className="explain-body">Nothing was recorded for this attempt and nothing moved.</p>
      ) : (
        <div className="explain-body" data-recorded-refusal={recorded.transferId}>
          <p>
            The backend recorded this refusal as transfer{" "}
            <Link to={transferHref(recorded.transferId)}>
              <code className="mono-small">{recorded.transferId}</code>
            </Link>
            , REJECTED with <code className="mono-small">{recorded.rejectionCode}</code>. Nothing moved. Sending the
            same attempt again returns this same answer.
          </p>
          <Codes codes={recorded.reasonCodes} />
        </div>
      )}
    </Explanation>
  );
}

/* --------------------------------------------------------------------------
 * The sandbox faucet
 * ------------------------------------------------------------------------ */

/** Request sandbox dollars into the session's own account. */
export function FaucetForm(props: { readonly accountId: string; readonly symbol: string }): ReactNode {
  const grant = useRequestFaucetGrant();
  const [amount, setAmount] = useState("");
  const [attemptKey, setAttemptKey] = useState<string | undefined>(undefined);
  const value = checkAmount(amount);

  const request = (): void => {
    if (!value.ok) return;
    const key = confirmationKey(attemptKey, newIdempotencyKey);
    setAttemptKey(key);
    grant.mutate({ accountId: props.accountId, amount: value.value, idempotencyKey: key });
  };

  return (
    <form
      className="form"
      aria-label="Request sandbox dollars"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        request();
      }}
    >
      <TextField
        label={`Amount in ${props.symbol}`}
        value={amount}
        check={value}
        inputMode="decimal"
        note="Whole sandbox dollars as a plain decimal, sent exactly as typed. The faucet refuses (RATE_LIMITED) a grant beyond what remains."
        onChange={(next) => {
          setAmount(next);
          setAttemptKey(undefined);
          grant.reset();
        }}
      />
      <div className="form-actions">
        {value.ok ? (
          <Button variant="primary" submit busy={grant.isPending} busyLabel="Asking the faucet…">
            Request sandbox dollars
          </Button>
        ) : (
          <Button variant="primary" disabledReason={value.error}>
            Request sandbox dollars
          </Button>
        )}
        {grant.isSuccess && (
          <Button
            variant="quiet"
            onClick={() => {
              setAmount("");
              setAttemptKey(undefined);
              grant.reset();
            }}
          >
            Start a new request
          </Button>
        )}
      </div>
      <AttemptKey attemptKey={attemptKey} />
      {grant.isError && <Explanation error={grant.error} onRetry={request} retryLabel="Try again with the same key" />}
      {grant.isSuccess && <GrantResult grant={grant.data} />}
    </form>
  );
}

function GrantResult(props: { readonly grant: AdvancedFaucetGrant }): ReactNode {
  const { grant } = props;
  return (
    <div className="callout" role="status" aria-label="The faucet's answer">
      <p>
        Granted{" "}
        <span data-field="granted-quantity">
          <Qty value={grant.quantity} decimals={grant.asset_decimals} symbol={grant.asset_symbol} />
        </span>{" "}
        of simulated value into <code className="mono-small">{grant.account_id}</code>.
      </p>
      <FieldGrid columns={3}>
        <Field label="Grant">
          <Identifier value={grant.id} />
        </Field>
        <Field label="Journal" note="The SANDBOX_FUNDING posting that made the grant.">
          <Identifier value={grant.journal_transaction_id} />
        </Field>
        <Field label="Evidence">
          {grant.evidence_ref === undefined ? <span className="absent">none recorded</span> : <Identifier value={grant.evidence_ref} />}
        </Field>
      </FieldGrid>
    </div>
  );
}

/* --------------------------------------------------------------------------
 * The account's handle
 * ------------------------------------------------------------------------ */

/**
 * Register the handle other customers pay this account by. The API offers no
 * read of an account's handle, so the page cannot show one already
 * registered; registering a second answers CONFLICT, which is how an account
 * that already has one finds out.
 */
export function HandleForm(props: { readonly accountId: string }): ReactNode {
  const register = useRegisterHandle();
  const [handle, setHandle] = useState("");
  const [attemptKey, setAttemptKey] = useState<string | undefined>(undefined);
  const value = checkHandle(handle);

  const submit = (): void => {
    if (!value.ok) return;
    const key = confirmationKey(attemptKey, newIdempotencyKey);
    setAttemptKey(key);
    register.mutate({ accountId: props.accountId, handle: value.value, idempotencyKey: key });
  };

  return (
    <form
      className="form"
      aria-label="Register a handle"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        submit();
      }}
    >
      <TextField
        label="Handle"
        value={handle}
        check={value}
        note="3 to 32 characters of a-z, 0-9, '_', '.' or '-', starting with a letter or digit. It is lower-cased, and it is yours for good: a handle is never reassigned."
        onChange={(next) => {
          setHandle(next);
          setAttemptKey(undefined);
          register.reset();
        }}
      />
      <div className="form-actions">
        {value.ok ? (
          <Button variant="primary" submit busy={register.isPending} busyLabel="Registering…">
            Register this handle
          </Button>
        ) : (
          <Button variant="primary" disabledReason={value.error}>
            Register this handle
          </Button>
        )}
      </div>
      <AttemptKey attemptKey={attemptKey} />
      {register.isError && <Explanation error={register.error} onRetry={submit} retryLabel="Try again with the same key" />}
      {register.isSuccess && (
        <p className="callout" role="status" data-registered-handle={register.data.handle}>
          Registered <code className="mono-small">@{register.data.handle}</code> for{" "}
          <code className="mono-small">{register.data.account_id}</code>. Other customers can now pay this account by it.
        </p>
      )}
    </form>
  );
}

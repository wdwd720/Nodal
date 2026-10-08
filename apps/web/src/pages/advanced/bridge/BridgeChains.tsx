/**
 * THE SIMULATED CHAINS: the persistent host's two chains, sbx-solana and
 * sbx-ethereum, shared by the stable asset and the bridge — their tips,
 * finality and parameters, for display.
 *
 * For an operator: producing blocks (in SANDBOX mode the platform is the
 * block producer, so a submitted transaction finalizes only as blocks are
 * produced) and injecting submission faults. Fault injection exists only in
 * LOCAL, TEST and DEV; anywhere else the backend refuses it, and the refusal
 * is shown with its code. Both need sandbox:operate and a recent strong
 * sign-in.
 */
import { useId, useState, type ReactNode } from "react";

import {
  useBridgeChains,
  useInjectFault,
  useProduceBlocks,
  type BlocksRequest,
  type BridgeBlocksResult,
  type BridgeChain,
  type BridgeChains,
  type BridgeFaultResult,
  type FaultRequest,
} from "../../../api/advanced/bridge.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  BRIDGE_CHAIN_IDS,
  BRIDGE_FAULT_KINDS,
  FAULT_KIND_MEANING,
  chainIdOf,
  faultTakesCount,
  type BridgeChainId,
  type BridgeFaultKind,
} from "../../../lib/bridge.ts";
import { parseWholeNumber } from "../../../lib/stable.ts";
import { ScrollTable } from "../shared.tsx";
import { CommandFlow, type Prepared } from "../stable/parts.tsx";
import { BridgeNav } from "./parts.tsx";

export function BridgeChainsPage(): ReactNode {
  const chains = useBridgeChains();
  return (
    <Page
      title="Simulated chains"
      lead="The two simulated chains the stable asset and the bridge run on: their tips, how far finality has reached, and their parameters. A simulation the books are reconciled against."
    >
      <SimulatedNotice />
      <BridgeNav />
      <Panel title="Chains" description="As the persistent chain host reports them at the instant of the read.">
        <AsyncPanel query={chains} loadingLabel="Loading the simulated chains…">
          {(data: BridgeChains) => <ChainsBody data={data} />}
        </AsyncPanel>
      </Panel>
      <ProducePanel />
      <FaultPanel faultsAllowed={chains.data?.fault_injection_allowed} />
    </Page>
  );
}

function ChainsBody(props: { readonly data: BridgeChains }): ReactNode {
  const { data } = props;
  return (
    <>
      <p className="meta-row">
        <span>
          Network <code className="mono-small">{data.network}</code>
        </span>
        <span>
          Fault injection{" "}
          {data.fault_injection_allowed ? (
            <Pill tone="warn">available in this deployment</Pill>
          ) : (
            <Pill tone="neutral">not available here</Pill>
          )}
        </span>
      </p>
      <ScrollTable
        caption="Simulated chains"
        headers={["Chain", "Tip", "Finalized height", "Finality depth", "Blockhash window", "Fee per transaction", "Bridge authority"]}
      >
        {data.chains.map((chain) => (
          <ChainRow key={chain.id} chain={chain} />
        ))}
      </ScrollTable>
    </>
  );
}

function ChainRow(props: { readonly chain: BridgeChain }): ReactNode {
  const { chain } = props;
  return (
    <tr data-chain={chain.id}>
      <th scope="row">
        <code className="mono-small">{chain.id}</code>
      </th>
      <td data-field="tip_height">
        height {String(chain.tip_height)}, slot {String(chain.tip_slot)}
        <span className="cell-note mono-small">{chain.tip_hash}</span>
      </td>
      <td data-field="finalized_height">{String(chain.finalized_height)}</td>
      <td>{String(chain.finality_depth)} blocks</td>
      <td>{String(chain.blockhash_window)} blocks</td>
      <td>
        <BaseUnits value={chain.fee_per_tx} />
        <span className="cell-note">native units of the simulated chain</span>
      </td>
      <td>{chain.bridge_authority === undefined ? <span className="absent">bridge disabled</span> : <Identifier value={chain.bridge_authority} />}</td>
    </tr>
  );
}

function ChainSelect(props: { readonly value: BridgeChainId; readonly onChange: (chain: BridgeChainId) => void }): ReactNode {
  const id = useId();
  return (
    <div className="form-row">
      <label htmlFor={id}>Chain</label>
      <select
        id={id}
        className="input"
        value={props.value}
        onChange={(event) => {
          const chosen = chainIdOf(event.target.value);
          if (chosen !== undefined) props.onChange(chosen);
        }}
      >
        {BRIDGE_CHAIN_IDS.map((chain) => (
          <option key={chain} value={chain}>
            {chain}
          </option>
        ))}
      </select>
    </div>
  );
}

function ChainAfter(props: { readonly chain: BridgeChain }): ReactNode {
  const { chain } = props;
  return (
    <FieldGrid columns={3}>
      <Field label="Chain">
        <code className="mono-small">{chain.id}</code>
      </Field>
      <Field label="Tip">
        height {String(chain.tip_height)}, slot {String(chain.tip_slot)}
      </Field>
      <Field label="Finalized height">{String(chain.finalized_height)}</Field>
    </FieldGrid>
  );
}

function ProducePanel(): ReactNode {
  const command = useProduceBlocks();
  const [chain, setChain] = useState<BridgeChainId>("sbx-solana");
  const [raw, setRaw] = useState("");
  const countId = useId();
  const count = parseWholeNumber(raw, 1, 1000, "The number of blocks");
  const prepared: Prepared<BlocksRequest> = count.ok
    ? { ok: true, request: { chain, count: count.value } }
    : { ok: false, reason: raw === "" ? "Enter how many blocks to produce." : count.error };
  return (
    <Panel
      title="Produce blocks"
      description="Operator: in SANDBOX mode the platform is the block producer, so transactions are included and finalize as blocks are produced. Producing blocks never submits or settles anything itself."
    >
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <ChainSelect value={chain} onChange={setChain} />
        <div className="form-row">
          <label htmlFor={countId}>Blocks</label>
          <input
            id={countId}
            className="input"
            inputMode="numeric"
            autoComplete="off"
            value={raw}
            onChange={(event) => {
              setRaw(event.target.value);
            }}
          />
          <p className="field-note">A whole number from one to one thousand.</p>
        </div>
      </form>
      <CommandFlow
        label="produce blocks"
        prepared={prepared}
        command={command}
        describe={(request: BlocksRequest) => (
          <p>
            Produce {String(request.count)} block{request.count === 1 ? "" : "s"} on <code className="mono-small">{request.chain}</code>.
          </p>
        )}
        renderResult={(result: BridgeBlocksResult) => (
          <div role="status">
            <p>
              {String(result.produced)} block{result.produced === 1 ? "" : "s"} produced.
            </p>
            <ChainAfter chain={result.chain} />
          </div>
        )}
        refusedNote="The backend produced no block."
      />
    </Panel>
  );
}

function FaultPanel(props: { readonly faultsAllowed: boolean | undefined }): ReactNode {
  const command = useInjectFault();
  const [chain, setChain] = useState<BridgeChainId>("sbx-solana");
  const [kind, setKind] = useState<BridgeFaultKind>("TIMEOUT_BUT_LAND");
  const [raw, setRaw] = useState("");
  const kindId = useId();
  const countId = useId();
  const needsCount = faultTakesCount(kind);
  const count = parseWholeNumber(raw, 0, 1000, "The count");

  let prepared: Prepared<FaultRequest>;
  if (needsCount && !count.ok) {
    prepared = { ok: false, reason: raw === "" ? "Enter the count this fault takes." : count.error };
  } else {
    prepared = { ok: true, request: { chain, kind, count: needsCount && count.ok ? count.value : undefined } };
  }

  return (
    <Panel
      title="Inject a submission fault"
      description="Operator, and only in LOCAL, TEST and DEV: anywhere else the backend refuses fault injection with FORBIDDEN (reason FAULT_INJECTION_FORBIDDEN), and that refusal is shown below. A fault is journaled like any other chain event."
    >
      {props.faultsAllowed === false && (
        <p className="note">
          The chain host reports that fault injection is not available in this deployment, so the backend refuses it. The
          form stays, so the refusal and its code can be seen.
        </p>
      )}
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <ChainSelect value={chain} onChange={setChain} />
        <div className="form-row">
          <label htmlFor={kindId}>Fault</label>
          <select
            id={kindId}
            className="input"
            value={kind}
            onChange={(event) => {
              const chosen = BRIDGE_FAULT_KINDS.find((candidate) => candidate === event.target.value);
              if (chosen !== undefined) setKind(chosen);
            }}
          >
            {BRIDGE_FAULT_KINDS.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
              </option>
            ))}
          </select>
          <p className="field-note">{FAULT_KIND_MEANING[kind]}</p>
        </div>
        {needsCount && (
          <div className="form-row">
            <label htmlFor={countId}>Count</label>
            <input
              id={countId}
              className="input"
              inputMode="numeric"
              autoComplete="off"
              value={raw}
              onChange={(event) => {
                setRaw(event.target.value);
              }}
            />
            <p className="field-note">A whole number from zero to one thousand.</p>
          </div>
        )}
      </form>
      <CommandFlow
        label="inject the fault"
        prepared={prepared}
        command={command}
        variant="danger"
        describe={(request: FaultRequest) => (
          <p>
            Arm <code className="mono-small">{request.kind}</code>
            {request.count === undefined ? "" : ` with a count of ${String(request.count)}`} on{" "}
            <code className="mono-small">{request.chain}</code>.
          </p>
        )}
        renderResult={(result: BridgeFaultResult) => (
          <div role="status">
            <p>
              <code className="mono-small">{result.kind}</code> armed
              {result.count === undefined ? "" : ` with a count of ${String(result.count)}`}.
            </p>
            <ChainAfter chain={result.chain} />
          </div>
        )}
        refusedNote="The backend armed no fault."
      />
    </Panel>
  );
}

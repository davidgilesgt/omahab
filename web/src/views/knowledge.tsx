import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { useToast } from "../components/toast";
import { ErrorState, LoadingState, Section } from "../components/ui";
import { CopyButton } from "../components/copyButton";
import { IndexSetupControl } from "../components/indexSetup";

const PRINCIPAL = "default";


function ConsentDialog({
  provider,
  pending,
  error,
  onGrant,
  onDecline,
  onClose,
}: {
  provider: string;
  pending: boolean;
  error: unknown;
  onGrant: () => void;
  onDecline: () => void;
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
    return () => {
      if (dialog?.open) dialog.close();
    };
  }, []);
  return (
    <dialog ref={dialogRef} className="modal" aria-labelledby="consent-title" onCancel={(e) => { e.preventDefault(); onClose(); }}>
      <header>
        <div>
          <p className="eyebrow">Remote summarization</p>
          <h2 id="consent-title">Allow {provider} to summarize private documents?</h2>
        </div>
        <button type="button" className="icon-button" onClick={onClose} aria-label="Close">×</button>
      </header>
      <div className="form-stack">
        <p>This will send private document text to {provider} for summarization. You can revoke this later.</p>
        {error ? <p className="inline-error" role="alert">{error instanceof Error ? error.message : "Could not save consent"}</p> : null}
        <div className="modal-actions">
          <button type="button" className="button secondary" onClick={onDecline} disabled={pending}>Decline</button>
          <button type="button" className="button primary" onClick={onGrant} disabled={pending}>{pending ? "Saving…" : "Grant and continue"}</button>
        </div>
      </div>
    </dialog>
  );
}

function SummarizationConsentBlock() {
  const { client } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [provider, setProvider] = useState("openai");
  const trimmed = provider.trim();
  const normalizedProvider = trimmed.length ? trimmed : "openai";
  const consentQuery = useQuery({
    queryKey: ["knowledge", "consent", PRINCIPAL, normalizedProvider],
    queryFn: () => client.knowledgeGetConsent(normalizedProvider, PRINCIPAL),
    enabled: trimmed.length > 0,
  });
  const [dialogOpen, setDialogOpen] = useState(false);

  const setConsent = useMutation({
    mutationFn: (granted: boolean) => client.knowledgeSetConsent(normalizedProvider, granted, PRINCIPAL),
    onSuccess: (_data, granted) => {
      void queryClient.invalidateQueries({ queryKey: ["knowledge", "consent", PRINCIPAL, normalizedProvider] });
      if (granted) {
        toast.success(`Consent granted for ${normalizedProvider}`);
      } else {
        toast.success(`Consent declined for ${normalizedProvider}`);
      }
      setDialogOpen(false);
    },
    onError: (error: unknown) => {
      const message = error instanceof Error ? error.message : "Could not save consent";
      toast.error(message);
    },
  });

  function requestSummarize() {
    if (!trimmed) {
      toast.error("Enter a provider name");
      return;
    }
    if (consentQuery.data?.granted) {
      toast.success(`Summarization with ${normalizedProvider} — consent already granted`);
      return;
    }
    setDialogOpen(true);
  }

  const granted = consentQuery.data?.granted ?? false;
  const isChecking = consentQuery.isLoading;

  return (
    <div className="form-stack">
      <div className="form-inline" style={{ alignItems: "end" }}>
        <label>
          Provider
          <input
            value={provider}
            onChange={(event) => setProvider(event.currentTarget.value)}
            placeholder="openai"
            spellCheck={false}
            autoComplete="off"
          />
        </label>
        <div className="row-actions" style={{ justifyContent: "flex-start" }}>
          <button type="button" className="button primary" onClick={requestSummarize} disabled={!trimmed || setConsent.isPending}>
            Summarize with {normalizedProvider}
          </button>
        </div>
        <span className="muted" style={{ fontSize: "0.875rem", minWidth: "10rem" }}>
          {isChecking ? "Checking consent…" : consentQuery.isError ? "Consent check failed" : granted ? "Consent granted" : "Consent not granted"}
        </span>
      </div>
      {consentQuery.isError ? (
        <p className="inline-error" role="alert">
          {consentQuery.error instanceof Error ? consentQuery.error.message : "Could not check consent"}
        </p>
      ) : null}
      <small>
        Remote summarization sends private document text to the named provider. An explicit grant or decline is required and is
        persisted per provider.
      </small>
      {dialogOpen ? (
        <ConsentDialog
          provider={normalizedProvider}
          pending={setConsent.isPending}
          error={setConsent.error}
          onGrant={() => setConsent.mutate(true)}
          onDecline={() => setConsent.mutate(false)}
          onClose={() => setDialogOpen(false)}
        />
      ) : null}
    </div>
  );
}

function AssistantInfoBlock() {
  const { client } = useAuth();
  const instanceQuery = useQuery({ queryKey: ["instance"], queryFn: client.instance });
  if (instanceQuery.isLoading) return <LoadingState label="Loading assistant info" />;
  if (instanceQuery.isError) return <ErrorState error={instanceQuery.error} retry={() => void instanceQuery.refetch()} />;
  const inst = instanceQuery.data;
  if (!inst) return <p className="muted">No instance info.</p>;
  const domain = inst.domain;
  const assistant = inst.assistant_name || "Hermes";
  const aiUrl = domain ? `https://ai.${domain}` : "https://ai.example.com";
  return (
    <div className="form-stack">
      <dl className="definition-list">
        <div><dt>Assistant</dt><dd>{assistant} <span className="muted">({inst.assistant_slug || "hermes"})</span></dd></div>
        <div><dt>Domain</dt><dd className="mono">{domain} <CopyButton text={domain} label="Copy" /></dd></div>
        <div><dt>Assistant URL</dt><dd><a href={aiUrl} target="_blank" rel="noreferrer">{aiUrl}</a> <CopyButton text={aiUrl} label="Copy" /></dd></div>
      </dl>
      <small>The assistant is reached via the AI app tile. Its MCP tools are available at /mcp with a dedicated token.</small>
    </div>
  );
}

function HermesTokenBlock() {
  const { client } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();
  const tokenQuery = useQuery({ queryKey: ["hermes", "mcp-token"], queryFn: client.hermesMCPToken });
  const rotate = useMutation({
    mutationFn: () => client.rotateHermesMCPToken(),
    onSuccess: (data) => {
      void queryClient.invalidateQueries({ queryKey: ["hermes", "mcp-token"] });
      toast.success("Hermes MCP token rotated");
      if (data?.token) {
        navigator.clipboard.writeText(data.token).catch(() => {});
      }
    },
    onError: (err: unknown) => toast.error(err instanceof Error ? err.message : "Rotate failed"),
  });

  if (tokenQuery.isLoading) return <LoadingState label="Loading MCP token" />;
  if (tokenQuery.isError) return <ErrorState error={tokenQuery.error} retry={() => void tokenQuery.refetch()} />;

  const token = tokenQuery.data?.token ?? "";
  const masked = token ? `${token.slice(0, 8)}…${token.slice(-4)}` : "Not set";

  return (
    <div className="form-stack">
      <p>
        <strong>Current token:</strong> <span className="mono">{masked}</span>{" "}
        {token ? <CopyButton text={token} label="Copy token" /> : null}
      </p>
      <div className="row-actions">
        <button className="button secondary" type="button" disabled={rotate.isPending} onClick={() => rotate.mutate()}>
          {rotate.isPending ? "Rotating…" : "Rotate token"}
        </button>
      </div>
      {rotate.data?.token ? (
        <div className="banner-card" style={{ background: "var(--warning-bg)", border: "1px solid var(--warning)", padding: 12, borderRadius: 8 }}>
          <strong>New token (copy now — shown once):</strong>
          <p className="mono" style={{ wordBreak: "break-all" }}>{rotate.data.token}</p>
          <CopyButton text={rotate.data.token} label="Copy new token" />
        </div>
      ) : null}
      <small>Rotating invalidates the previous Hermes MCP token. Update Hermes env with the new value.</small>
      {rotate.isError ? (
        <p className="inline-error" role="alert">{rotate.error instanceof Error ? rotate.error.message : "Rotate failed"}</p>
      ) : null}
    </div>
  );
}

export function AssistantKnowledgePanel() {
  return (
    <div style={{ display: "grid", gap: "1.5rem" }}>
      <Section
        title="Assistant"
        description="Name, domain, and URL for the home assistant."
      >
        <AssistantInfoBlock />
      </Section>
      <Section
        title="Assistant knowledge"
        description="Choose how private sources are indexed for semantic search. Model details come from the server’s pinned models."
      >
        <IndexSetupControl />
      </Section>
      <Section
        title="Hermes MCP token"
        description="Dedicated token for the /mcp endpoint (Bearer OMAHAB_MCP_TOKEN). Rotate when needed."
      >
        <HermesTokenBlock />
      </Section>
      <Section
        title="Remote summarization"
        description="Grant or decline per-provider consent before any document text is sent for remote summarization."
      >
        <SummarizationConsentBlock />
      </Section>
    </div>
  );
}

export default AssistantKnowledgePanel;

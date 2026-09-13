import { useEffect, useRef, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../auth";
import { useToast } from "../components/toast";
import { ErrorState, LoadingState, PageHeader, Section } from "../components/ui";
import { CopyButton } from "../components/copyButton";
import { IndexSetupControl } from "../components/indexSetup";
import type { ModelSetupStatus } from "../api/types";

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

const SUMMARIZATION_ALIAS = "omahab/summarization";

function ProviderConsentRow({ provider, managementUrl }: { provider: string; managementUrl?: string }) {
  const { client } = useAuth();
  const toast = useToast();
  const queryClient = useQueryClient();
  const consentQuery = useQuery({
    queryKey: ["knowledge", "consent", PRINCIPAL, provider],
    queryFn: () => client.knowledgeGetConsent(provider, PRINCIPAL),
    enabled: provider !== "unknown",
  });
  const [dialogOpen, setDialogOpen] = useState(false);
  const setConsent = useMutation({
    mutationFn: (granted: boolean) => client.knowledgeSetConsent(provider, granted, PRINCIPAL),
    onSuccess: (_data, granted) => {
      void queryClient.invalidateQueries({ queryKey: ["knowledge", "consent", PRINCIPAL, provider] });
      toast.success(granted ? `Consent granted for ${provider}` : `Consent declined for ${provider}`);
      setDialogOpen(false);
    },
    onError: (error: unknown) => {
      toast.error(error instanceof Error ? error.message : "Could not save consent");
    },
  });

  async function openDialog() {
    // Refresh native routes before deciding so a remap wins over stale text.
    await queryClient.invalidateQueries({ queryKey: ["setup", "models"] });
    await queryClient.refetchQueries({ queryKey: ["setup", "models"] });
    setDialogOpen(true);
  }

  function handleGrant() {
    const current = queryClient.getQueryData<ModelSetupStatus>(["setup", "models"]);
    const currentProviders = current?.aliases.find((a) => a.name === SUMMARIZATION_ALIAS)?.providers ?? [];
    if (!currentProviders.includes(provider)) {
      // The route changed while deciding: never save consent for the stale display.
      setDialogOpen(false);
      toast.error("Summarization route changed — showing the current provider instead");
      void queryClient.invalidateQueries({ queryKey: ["setup", "models"] });
      return;
    }
    setConsent.mutate(true);
  }

  if (provider === "unknown") {
    return (
      <div className="form-stack">
        <div><code className="mono">unknown provider</code> <span className="muted">Consent unavailable</span></div>
        <p className="muted" style={{ fontSize: "0.875rem" }}>
          The summarization route has unknown provider metadata. Configure its metadata in LiteLLM before granting consent.
        </p>
        {managementUrl ? <a href={managementUrl} target="_blank" rel="noreferrer">Manage in LiteLLM</a> : null}
      </div>
    );
  }

  const granted = consentQuery.data?.granted ?? false;
  return (
    <div className="form-stack">
      <div style={{ display: "flex", gap: "0.5rem", alignItems: "baseline", flexWrap: "wrap" }}>
        <code className="mono">{provider}</code>
        <span className="muted" style={{ fontSize: "0.875rem" }}>
          {consentQuery.isLoading ? "Checking consent…" : consentQuery.isError ? "Consent check failed" : granted ? "Consent granted" : "Consent not granted"}
        </span>
      </div>
      {consentQuery.isError ? (
        <p className="inline-error" role="alert">
          {consentQuery.error instanceof Error ? consentQuery.error.message : "Could not check consent"}{" "}
          <button className="button ghost" type="button" onClick={() => void consentQuery.refetch()}>Retry</button>
        </p>
      ) : null}
      <small>Private document text may be sent to {provider} for remote summarization.</small>
      <div className="row-actions" style={{ justifyContent: "flex-start" }}>
        <button type="button" className="button primary" onClick={() => void openDialog()} disabled={setConsent.isPending}>
          Allow document sharing
        </button>
        <button type="button" className="button secondary" onClick={() => void openDialog()} disabled={setConsent.isPending}>
          Revoke consent
        </button>
      </div>
      {dialogOpen ? (
        <ConsentDialog
          provider={provider}
          pending={setConsent.isPending}
          error={setConsent.error}
          onGrant={handleGrant}
          onDecline={() => setConsent.mutate(false)}
          onClose={() => setDialogOpen(false)}
        />
      ) : null}
    </div>
  );
}

function SummarizationConsentBlock() {
  const { client } = useAuth();
  const queryClient = useQueryClient();
  const { pathname } = useLocation();
  const adminPrefix = pathname.startsWith("/admin") ? "/admin" : "";
  const setupQuery = useQuery({ queryKey: ["setup", "models"], queryFn: client.modelSetup, staleTime: 30_000, retry: false });
  useEffect(() => {
    const onFocus = () => { void queryClient.invalidateQueries({ queryKey: ["setup", "models"] }); };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [queryClient]);
  const status = setupQuery.data;
  const alias = status?.aliases.find((a) => a.name === SUMMARIZATION_ALIAS);
  const providers = alias?.providers ?? [];
  const managementUrl = status?.management_url;
  return (
    <div className="form-stack">
      <p><code className="mono">{SUMMARIZATION_ALIAS}</code> <span className="muted">{alias?.configured ? "configured" : "not configured"}</span></p>
      {managementUrl ? <a href={managementUrl} target="_blank" rel="noreferrer">Manage in LiteLLM</a> : null}
      <small>Private document text may be sent to the displayed provider for remote summarization. Consent is stored per provider and is never granted automatically.</small>
      {setupQuery.isLoading ? (
        <LoadingState label="Loading summarization routes" />
      ) : setupQuery.isError ? (
        <ErrorState error={setupQuery.error} retry={() => void setupQuery.refetch()} />
      ) : !alias?.configured || !providers.length ? (
        <p className="muted">
          No summarization route is configured. <Link to={`${adminPrefix}/setup`}>Create default aliases in Setup → AI providers</Link>, or manage models in LiteLLM.
        </p>
      ) : (
        <div className="form-stack">
          {providers.map((provider) => (
            <ProviderConsentRow key={provider} provider={provider} managementUrl={managementUrl} />
          ))}
        </div>
      )}
      <p className="muted" style={{ fontSize: "0.875rem" }}>
        <Link to={`${adminPrefix}/setup`}>Back to Setup → AI providers</Link>
      </p>
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
        <div className="token-banner">
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
    <div className="page">
      <PageHeader title="AI" description="Local document search and remote summarization consent." />
      <div style={{ display: "grid", gap: "1.5rem" }}>
        <Section title="Assistant">
          <AssistantInfoBlock />
        </Section>
        <Section title="Assistant knowledge">
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
    </div>
  );
}

export default AssistantKnowledgePanel;

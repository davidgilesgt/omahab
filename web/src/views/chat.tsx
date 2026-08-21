import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { defaultHermesUrl, HermesTransport, safeHermesUrl, type HermesApprovalRequest, type HermesMessage } from "../api/hermes";
import { PageHeader, StatusPill, formatDate } from "../components/ui";

const HERMES_URL_KEY = "omahab.hermes.url";
const HERMES_PROFILE_KEY = "omahab.hermes.profile";

type ConnectionState = "connecting" | "connected" | "disconnected" | "error";

export function ChatPage() {
  const [url, setUrl] = useState(() => safeHermesUrl(localStorage.getItem(HERMES_URL_KEY) ?? "") ?? defaultHermesUrl());
  const [profile, setProfile] = useState(() => localStorage.getItem(HERMES_PROFILE_KEY) ?? "default");
  const [connection, setConnection] = useState<ConnectionState>("connecting");
  const [connectionError, setConnectionError] = useState<string | null>(null);
  const [messages, setMessages] = useState<HermesMessage[]>([]);
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [approval, setApproval] = useState<HermesApprovalRequest | null>(null);
  const [approvalPending, setApprovalPending] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const transportRef = useRef<HermesTransport | null>(null);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const composerRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    const transport = new HermesTransport({ url, profile });
    transportRef.current = transport;
    setConnection("connecting");
    setConnectionError(null);
    const unsubscribe = transport.subscribe((message) => {
      setMessages((current) => current.some((item) => item.id === message.id) ? current.map((item) => item.id === message.id ? message : item) : [...current, message]);
    });
    const unsubscribeApprovals = transport.subscribeApprovals(setApproval);
    transport.connect().then(() => setConnection("connected")).catch((error: unknown) => {
      setConnection("error");
      setConnectionError(error instanceof Error ? error.message : "Hermes could not be reached.");
    });
    return () => {
      unsubscribeApprovals();
      unsubscribe();
      transport.close();
      if (transportRef.current === transport) transportRef.current = null;
    };
  }, [profile, url]);

  useEffect(() => {
    transcriptRef.current?.scrollTo({ top: transcriptRef.current.scrollHeight, behavior: "smooth" });
  }, [messages]);

  async function sendMessage() {
    const content = draft.trim();
    if (!content || !transportRef.current || connection !== "connected") return;
    const local: HermesMessage = { id: crypto.randomUUID(), role: "user", content, created_at: new Date().toISOString() };
    setMessages((current) => [...current, local]);
    setDraft("");
    setSending(true);
    setConnectionError(null);
    try {
      await transportRef.current.send(content);
    } catch (error) {
      setConnectionError(error instanceof Error ? error.message : "The message could not be sent.");
    } finally {
      setSending(false);
      composerRef.current?.focus();
    }
  }
  async function respondToApproval(choice: HermesApprovalRequest["choices"][number]) {
    if (!approval || !transportRef.current) return;
    setApprovalPending(true);
    setConnectionError(null);
    try {
      await transportRef.current.respondToApproval(approval.requestId, choice);
      setApproval(null);
    } catch (error) {
      setConnectionError(error instanceof Error ? error.message : "The approval response could not be sent.");
    } finally {
      setApprovalPending(false);
    }
  }


  function submitSettings(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    const nextUrl = String(data.get("url") ?? "").trim();
    const nextProfile = String(data.get("profile") ?? "default").trim();
    if (!nextUrl || !nextProfile) return;
    try {
      const parsed = new URL(nextUrl);
      if (parsed.protocol !== "ws:" && parsed.protocol !== "wss:") throw new Error("Use a ws:// or wss:// endpoint.");
      const sensitiveKeys = ["token", "ticket", "key", "api_key", "authorization"];
      if (parsed.username || parsed.password || sensitiveKeys.some((key) => parsed.searchParams.has(key))) {
        throw new Error("Do not put credentials in the WebSocket URL. Configure authentication at the gateway.");
      }
    } catch (error) {
      setConnectionError(error instanceof Error ? error.message : "Enter a valid WebSocket URL.");
      return;
    }
    localStorage.setItem(HERMES_URL_KEY, nextUrl);
    localStorage.setItem(HERMES_PROFILE_KEY, nextProfile);
    setUrl(nextUrl);
    setProfile(nextProfile);
    setSettingsOpen(false);
  }

  function handleComposerKey(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      void sendMessage();
    }
  }

  return (
    <div className="page chat-page">
      <PageHeader eyebrow="Hermes" title="AI" description="A direct, authenticated JSON-RPC session with your server-side assistant." actions={<button className="button secondary" type="button" onClick={() => setSettingsOpen((open) => !open)} aria-expanded={settingsOpen}>Connection</button>} />
      {settingsOpen && (
        <section className="connection-panel" aria-labelledby="connection-heading">
          <div><h2 id="connection-heading">Hermes connection</h2><p>Only non-secret endpoint metadata is saved in this browser.</p></div>
          <form className="form-inline" onSubmit={submitSettings}>
            <label>WebSocket URL<input name="url" type="url" defaultValue={url} required className="mono" /></label>
            <label>Profile<input name="profile" defaultValue={profile} required /></label>
            <button className="button primary" type="submit">Reconnect</button>
          </form>
        </section>
      )}
      <section className="chat-shell" aria-label="AI conversation">
        <header className="chat-status"><div><span className="assistant-avatar" aria-hidden="true">AI</span><div><strong>AI</strong><small>Profile <span className="mono">{profile}</span></small></div></div><StatusPill value={connection} /></header>
        <div className="transcript" ref={transcriptRef} role="log" aria-live="polite" aria-relevant="additions">
          {messages.length === 0 ? (
            <div className="chat-empty"><span className="assistant-avatar large" aria-hidden="true">AI</span><h2>What can I help with?</h2><p>Messages and tool activity come directly from your configured Hermes profile. Nothing is simulated in this interface.</p></div>
          ) : messages.map((message) => (
            <article key={message.id} className={`message message-${message.role}`}>
              <header><strong>{message.role === "user" ? "You" : message.role === "assistant" ? "AI" : "Tool"}</strong><time dateTime={message.created_at}>{formatDate(message.created_at)}</time></header>
              <div className="message-content">{message.content}</div>
            </article>
          ))}
        </div>
        {approval && (
          <section className="tool-approval" aria-labelledby="approval-heading">
            <div><p className="eyebrow">Tool approval</p><strong id="approval-heading">{approval.description}</strong><small>The command stays on the server. Choose the narrowest permission that works.</small></div>
            <div className="row-actions">
              {approval.choices.includes("once") && <button className="button primary" type="button" disabled={approvalPending} onClick={() => void respondToApproval("once")}>Run once</button>}
              {approval.choices.includes("session") && <button className="button secondary" type="button" disabled={approvalPending} onClick={() => void respondToApproval("session")}>Allow for session</button>}
              {approval.choices.includes("deny") && <button className="button ghost" type="button" disabled={approvalPending} onClick={() => void respondToApproval("deny")}>Reject</button>}
            </div>
          </section>
        )}
        <footer className="composer-area">
          {connectionError && <p className="inline-error" role="alert">{connectionError}</p>}
          <div className="composer"><label className="sr-only" htmlFor="chat-message">Message AI</label><textarea id="chat-message" ref={composerRef} value={draft} onChange={(event) => setDraft(event.currentTarget.value)} onKeyDown={handleComposerKey} rows={2} disabled={connection !== "connected"} aria-describedby="composer-help" /><button className="button primary" type="button" onClick={() => void sendMessage()} disabled={!draft.trim() || sending || connection !== "connected"}>{sending ? "Sending…" : "Send"}</button></div>
          <small id="composer-help">Enter to send · Shift+Enter for a new line · tool execution remains server-side</small>
        </footer>
      </section>
    </div>
  );
}

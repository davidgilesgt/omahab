export interface HermesMessage {
  id: string;
  role: "user" | "assistant" | "tool";
  content: string;
  created_at: string;
  pending?: boolean;
}
export interface HermesApprovalRequest {
  requestId: string;
  description: string;
  choices: Array<"once" | "session" | "always" | "deny">;
}


interface GatewayEvent {
  type: string;
  session_id?: string;
  payload?: Record<string, unknown>;
}

interface RpcResponse {
  jsonrpc: "2.0";
  id?: string | number | null;
  method?: string;
  params?: GatewayEvent;
  result?: unknown;
  error?: { code?: number; message?: string };
}

interface SessionCreateResult {
  session_id: string;
  stored_session_id?: string;
}

export interface HermesTransportConfig {
  url: string;
  profile: string;
}

export class HermesTransport {
  private socket: WebSocket | null = null;
  private nextId = 1;
  private sessionId: string | null = null;
  private activeAssistant: HermesMessage | null = null;
  private pending = new Map<number, { resolve: (value: unknown) => void; reject: (error: Error) => void }>();
  private listeners = new Set<(message: HermesMessage) => void>();
  private approvalListeners = new Set<(request: HermesApprovalRequest) => void>();

  constructor(private readonly config: HermesTransportConfig) {}

  connect(): Promise<void> {
    if (this.socket?.readyState === WebSocket.OPEN && this.sessionId) return Promise.resolve();
    const { promise, resolve, reject } = Promise.withResolvers<void>();
    const socket = new WebSocket(this.config.url);
    this.socket = socket;
    socket.addEventListener("open", () => {
      this.call("session.create", { cols: 96, source: "web", profile: this.config.profile })
        .then((result) => {
          const created = result as Partial<SessionCreateResult>;
          if (!created.session_id) throw new Error("Hermes did not return a session identifier.");
          this.sessionId = created.session_id;
          resolve();
        })
        .catch(reject);
    }, { once: true });
    socket.addEventListener("error", () => reject(new Error("The Hermes connection could not be established.")), { once: true });
    socket.addEventListener("message", (event) => this.receive(String(event.data)));
    socket.addEventListener("close", () => {
      this.sessionId = null;
      for (const request of this.pending.values()) request.reject(new Error("The Hermes connection closed."));
      this.pending.clear();
    });
    return promise;
  }

  private emit(message: HermesMessage) {
    for (const listener of this.listeners) listener(message);
  }

  private receive(raw: string) {
    let frame: RpcResponse;
    try {
      frame = JSON.parse(raw) as RpcResponse;
    } catch {
      return;
    }
    if (frame.id !== undefined && frame.id !== null) {
      const numericId = Number(frame.id);
      const request = this.pending.get(numericId);
      if (!request) return;
      this.pending.delete(numericId);
      if (frame.error) request.reject(new Error(frame.error.message || "Hermes RPC failed."));
      else request.resolve(frame.result);
      return;
    }
    if (frame.method !== "event" || !frame.params?.type) return;
    const event = frame.params;
    if (event.session_id && this.sessionId && event.session_id !== this.sessionId) return;
    const payload = event.payload ?? {};
    const text = typeof payload.text === "string" ? payload.text : typeof payload.rendered === "string" ? payload.rendered : "";

    if (event.type === "message.start") {
      this.activeAssistant = { id: crypto.randomUUID(), role: "assistant", content: "", created_at: new Date().toISOString(), pending: true };
      this.emit(this.activeAssistant);
    } else if (event.type === "message.delta") {
      if (!this.activeAssistant) this.activeAssistant = { id: crypto.randomUUID(), role: "assistant", content: "", created_at: new Date().toISOString(), pending: true };
      this.activeAssistant = { ...this.activeAssistant, content: `${this.activeAssistant.content}${text}` };
      this.emit(this.activeAssistant);
    } else if (event.type === "message.complete") {
      if (!this.activeAssistant) this.activeAssistant = { id: crypto.randomUUID(), role: "assistant", content: "", created_at: new Date().toISOString() };
      this.activeAssistant = { ...this.activeAssistant, content: text || this.activeAssistant.content, pending: false };
      this.emit(this.activeAssistant);
      this.activeAssistant = null;
    } else if (event.type === "approval.request") {
      const requestId = typeof payload.request_id === "string" ? payload.request_id : "";
      if (!requestId) return;
      const supportedChoices = ["once", "session", "always", "deny"] as const;
      const offered = Array.isArray(payload.choices) ? payload.choices : ["once", "deny"];
      const choices = supportedChoices.filter((choice) => offered.includes(choice));
      const request: HermesApprovalRequest = {
        requestId,
        description: typeof payload.description === "string" ? payload.description : "Hermes needs approval to continue.",
        choices: choices.length ? choices : ["once", "deny"],
      };
      for (const listener of this.approvalListeners) listener(request);
    } else if (event.type === "tool.start" || event.type === "tool.complete") {
      const toolName = typeof payload.name === "string" ? payload.name : "Tool";
      this.emit({ id: crypto.randomUUID(), role: "tool", content: event.type === "tool.start" ? `${toolName} started` : `${toolName} completed`, created_at: new Date().toISOString() });
    } else if (event.type === "error") {
      const message = typeof payload.message === "string" ? payload.message : "Hermes reported an error.";
      this.emit({ id: crypto.randomUUID(), role: "tool", content: message, created_at: new Date().toISOString() });
    }
  }

  call(method: string, params: Record<string, unknown>): Promise<unknown> {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) return Promise.reject(new Error("Hermes is not connected."));
    const id = this.nextId++;
    this.socket.send(JSON.stringify({ jsonrpc: "2.0", id, method, params }));
    const { promise, resolve, reject } = Promise.withResolvers<unknown>();
    this.pending.set(id, { resolve, reject });
    return promise;
  }

  async send(content: string): Promise<void> {
    if (!this.sessionId) throw new Error("Hermes has no active session.");
    await this.call("prompt.submit", { session_id: this.sessionId, text: content });
  }
  async respondToApproval(requestId: string, choice: HermesApprovalRequest["choices"][number]): Promise<void> {
    if (!this.sessionId) throw new Error("Hermes has no active session.");
    await this.call("approval.respond", { request_id: requestId, session_id: this.sessionId, choice });
  }


  subscribe(listener: (message: HermesMessage) => void) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }
  subscribeApprovals(listener: (request: HermesApprovalRequest) => void) {
    this.approvalListeners.add(listener);
    return () => this.approvalListeners.delete(listener);
  }


  close() {
    this.socket?.close(1000, "Client closed");
    this.socket = null;
    this.sessionId = null;
  }
}
export function safeHermesUrl(value: string): string | null {
  try {
    const parsed = new URL(value);
    const sensitiveKeys = ["token", "ticket", "key", "api_key", "authorization"];
    if (parsed.protocol !== "ws:" && parsed.protocol !== "wss:") return null;
    if (parsed.username || parsed.password || sensitiveKeys.some((key) => parsed.searchParams.has(key))) return null;
    return parsed.toString();
  } catch {
    return null;
  }
}


export function defaultHermesUrl() {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  const fallback = `${protocol}//${window.location.host}/api/v1/hermes/ws`;
  const configured = import.meta.env.VITE_HERMES_WS_URL as string | undefined;
  return configured ? safeHermesUrl(configured) ?? fallback : fallback;
}

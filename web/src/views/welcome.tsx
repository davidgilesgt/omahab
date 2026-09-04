import { useParams } from "react-router-dom";
import { QRCode } from "../components/qr";
import { PageHeader, Section } from "../components/ui";

function getDomain(): string {
  if (typeof window === "undefined") return "";
  const host = window.location.hostname;
  if (!host) return "";
  if (host.startsWith("home.")) return host.slice(5);
  const idx = host.indexOf(".home.");
  if (idx !== -1) return host.slice(idx + 6);
  const parts = host.split(".");
  if (parts.length >= 2) return parts.slice(1).join(".");
  return host;
}

export function WelcomePage() {
  const { token } = useParams<{ token: string }>();
  const domain = getDomain();
  const idHost = domain ? `id.${domain}` : "id.example.com";
  const photosHost = domain ? `photos.${domain}` : "photos.example.com";
  // enrollment URL: Pocket ID expects token param; shape may be /enroll?token= or /enrollment?token=
  // We keep generic link to id host with token as path; user can copy.
  const enrollmentUrl = token ? `https://${idHost}/enroll?token=${encodeURIComponent(token)}` : `https://${idHost}`;
  const qrData = token ? enrollmentUrl : "";

  return (
    <div className="page">
      <PageHeader
        eyebrow="Welcome"
        title="You’re invited to Omahab"
        description="Four quick steps to get you inside the home — no technical knowledge needed."
      />

      <div className="welcome-grid" style={{ display: "grid", gap: 16 }}>
        <Section title="1 · Install Tailscale" description="Tailscale connects your device to the home tailnet (the private network).">
          <ul className="list">
            <li>
              <a href="https://apps.apple.com/app/tailscale/id1470499037" target="_blank" rel="noreferrer">
                iPhone / iPad — App Store
              </a>
            </li>
            <li>
              <a href="https://play.google.com/store/apps/details?id=com.tailscale.ipn" target="_blank" rel="noreferrer">
                Android — Play Store
              </a>
            </li>
            <li>
              <a href="https://tailscale.com/download" target="_blank" rel="noreferrer">
                macOS / Windows / Linux — tailscale.com/download
              </a>
            </li>
          </ul>
          <p className="muted" style={{ fontSize: "0.875rem" }}>
            Install the app, sign in, and accept the invitation if prompted. Your admin may need to approve your device.
          </p>
        </Section>

        <Section title="2 · Accept your invite" description="Open your personal enrollment link to register with the home.">
          {token ? (
            <>
              <p>
                <strong>Enrollment token:</strong> <span className="mono" style={{ wordBreak: "break-all" }}>{token}</span>
              </p>
              <p>
                <a className="button primary" href={enrollmentUrl} target="_blank" rel="noreferrer">
                  Accept invite — create passkey
                </a>
              </p>
              <p className="muted" style={{ fontSize: "0.875rem" }}>
                This link is one-time and expires quickly. If it says “expired”, ask your admin for a fresh invite.
              </p>
              {qrData ? (
                <div style={{ marginTop: 12 }}>
                  <p style={{ fontSize: "0.875rem" }}>
                    <strong>QR for your phone:</strong>
                  </p>
                  <QRCode value={qrData} label="QR code for enrollment" />
                  <p className="mono" style={{ fontSize: "0.75rem", wordBreak: "break-all", marginTop: 6 }}>
                    {qrData}
                  </p>
                </div>
              ) : null}
            </>
          ) : (
            <p className="muted">No token in the URL. Ask your admin for a fresh welcome link like home.{domain}/welcome/&lt;token&gt;.</p>
          )}
        </Section>

        <Section title="3 · Create a passkey" description="Pocket ID will ask you to create a passkey — your passwordless sign-in.">
          <ul className="list">
            <li>Approve the browser prompt to create a passkey (Face ID / Touch ID / Windows Hello).</li>
            <li>If you skip it, you can still add a passkey later from Pocket ID.</li>
          </ul>
        </Section>

        <Section title="4 · Open Photos" description="You’re in — try the first app.">
          <p>
            <a className="button secondary" href={`https://${photosHost}`} target="_blank" rel="noreferrer">
              Open Photos
            </a>{" "}
            <span className="muted" style={{ fontSize: "0.875rem" }}>
              (or any tile from the home page at <span className="mono">home.{domain || "example.com"}</span>)
            </span>
          </p>
          <div style={{ display: "flex", gap: 8, marginTop: 12 }}>
            <button className="button ghost" type="button" onClick={() => window.print()}>
              Print this page
            </button>
          </div>
        </Section>
      </div>

      <p className="muted" style={{ marginTop: 16, fontSize: "0.875rem" }}>
        Need help? Ask the person who invited you — they can see your enrollment status from the People page.
      </p>

      <style>{`
        @media print { .button, .page-actions { display: none !important; } .page { padding: 0; } }
      `}</style>
    </div>
  );
}

export default WelcomePage;

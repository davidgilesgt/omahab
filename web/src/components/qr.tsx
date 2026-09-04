import { useEffect, useRef } from "react";
import QRCodeLib from "qrcode";

export function QRCode({ value, size = 180, label }: { value: string; size?: number; label: string }) {
  const canvasRef = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || !value) return;
    QRCodeLib.toCanvas(canvas, value, {
      width: size,
      margin: 1,
      color: { dark: "#262820", light: "#ffffff" },
    }).catch(() => {
      // ignore render errors (e.g., value too long)
    });
  }, [value, size]);

  return (
    <div className="qr-frame" role="img" aria-label={label}>
      <canvas ref={canvasRef} width={size} height={size} style={{ display: "block", width: size, height: size }} />
    </div>
  );
}

import { useEffect, useRef, useState } from "react";
import { Palette } from "lucide-react";

export const THEMES = [
  "system",
  "light",
  "dark",
  "tokyo-night",
  "catppuccin",
  "everforest",
  "gruvbox",
  "kanagawa",
  "nord",
  "rose-pine",
  "matte-black",
  "osaka-jade",
  "ristretto",
] as const;

export function ThemePicker({ value, onChange }: { value: string; onChange: (t: string) => void }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function handleClickOutside(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    function handleEscape(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", handleClickOutside);
    document.addEventListener("keydown", handleEscape);
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [open]);

  return (
    <div ref={ref} style={{ position: "relative" }}>
      <button
        type="button"
        className="icon-button"
        aria-label="Color theme"
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Palette size={18} strokeWidth={1.75} aria-hidden />
      </button>
      {open && (
        <div role="listbox" className="theme-grid" aria-label="Color theme">
          {THEMES.map((t) => (
            <button
              key={t}
              role="option"
              aria-selected={value === t}
              data-theme={t === "system" ? undefined : t}
              onClick={() => {
                onChange(t);
                setOpen(false);
              }}
              className="theme-option"
              type="button"
            >
              <span className="swatch" aria-hidden>
                <i style={{ background: "var(--paper)" }} />
                <i style={{ background: "var(--surface-raised)" }} />
                <i style={{ background: "var(--accent)" }} />
              </span>
              <span className="theme-label">{t}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

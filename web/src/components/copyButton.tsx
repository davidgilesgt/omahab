import { useCallback, useEffect, useRef, useState } from "react";
import { useToast } from "./toast";

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Fall through to the execCommand fallback.
  }
  let area: HTMLTextAreaElement | null = null;
  try {
    area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    if (area !== null && area.parentNode !== null) {
      area.parentNode.removeChild(area);
    }
  }
}

interface CopyButtonProps {
  text: string;
  label?: string;
  className?: string;
}

export function CopyButton({ text, label = "Copy", className }: CopyButtonProps) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | null>(null);
  const toast = useToast();

  const copy = useCallback(async () => {
    const ok = await copyText(text);
    if (ok) {
      setCopied(true);
      if (timer.current !== null) window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 2000);
    } else {
      toast.error("Copy failed. Select and copy the text manually.");
    }
  }, [text, toast]);

  useEffect(
    () => () => {
      if (timer.current !== null) window.clearTimeout(timer.current);
    },
    [],
  );

  return (
    <button
      type="button"
      className={className ? `copy-button ${className}` : "copy-button"}
      onClick={copy}
      aria-live="polite"
      aria-label={copied ? "Copied" : label}
    >
      <span className="mono" aria-hidden="true">
        {copied ? "Copied" : label}
      </span>
    </button>
  );
}

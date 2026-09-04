import { Check } from "lucide-react";

export type StepDef = { id: string; label: string };

export function Stepper({ steps, current }: { steps: StepDef[]; current: string }) {
  let idx = steps.findIndex((s) => s.id === current);
  // mode step is before first tracked step: show Claim as done, rest todo
  if (idx === -1) {
    // if current is "mode" treat as after code done, before ssh
    idx = 1;
    // For mode we render with a special rule: index 0 done, rest todo, no current
    // Use a flag to indicate no current
    return (
      <ol className="stepper">
        {steps.map((s, i) => {
          let state: "done" | "current" | "todo";
          if (i === 0) state = "done";
          else state = "todo";
          return (
            <li key={s.id} data-state={state}>
              <span className="stepper-dot" aria-hidden>
                {state === "done" ? <Check size={12} strokeWidth={2.5} /> : i + 1}
              </span>
              <span>{s.label}</span>
            </li>
          );
        })}
      </ol>
    );
  }

  return (
    <ol className="stepper">
      {steps.map((s, i) => {
        let state: "done" | "current" | "todo";
        if (i < idx) state = "done";
        else if (i === idx) state = "current";
        else state = "todo";
        return (
          <li key={s.id} data-state={state}>
            <span className="stepper-dot" aria-hidden>
              {state === "done" ? <Check size={12} strokeWidth={2.5} /> : i + 1}
            </span>
            <span>{s.label}</span>
          </li>
        );
      })}
    </ol>
  );
}

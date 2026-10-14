import { type ReactNode, useEffect, useRef, useState } from "react";

/** A control-bar button opening a panel above it; closes on outside click or Escape. */
export function MenuButton({ label, name, active, children }: { label: ReactNode; name: string; active?: boolean; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: Event) => !ref.current?.contains(e.target as Node) && setOpen(false);
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);
  return (
    <div className="moqit-device-menu-anchor" ref={ref}>
      <button aria-label={name} aria-expanded={open} className={open || active ? "on" : undefined} onClick={() => setOpen((o) => !o)}>
        {label}
      </button>
      {open && (
        <div className="moqit-device-menu" role="dialog" aria-label={`${name} settings`}>
          {children}
        </div>
      )}
    </div>
  );
}

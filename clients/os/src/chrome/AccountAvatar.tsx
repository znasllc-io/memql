import { useSessionIfPresent } from "./access";

/** The same account identity in the dock and conversations. */
export function AccountAvatar({ className = "" }: { className?: string }) {
  const session = useSessionIfPresent();
  const initial = (session?.access?.primaryEmail || "?").slice(0, 1).toUpperCase();
  return <span className={`os-avatar ${className}`} aria-hidden>{initial}</span>;
}

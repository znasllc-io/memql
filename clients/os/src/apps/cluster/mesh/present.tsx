import type { ReactNode } from "react";
import { Bot, CircleDashed, Fingerprint, Globe, Plug, Server, SquareTerminal, Workflow } from "lucide-react";

// The two presentation helpers the Mesh list and a node's page share. Their
// own module so neither page imports the other.

/** A count as a person reads it: grouped digits. */
export function formatCount(value: number): string {
  return Math.round(value).toLocaleString();
}

/** What kind of node, before a word is read. */
export function nodeIcon(nodeType: string): ReactNode {
  const props = { size: 18, "aria-hidden": true } as const;
  switch (nodeType) {
    case "bff":
      return <Server {...props} />;
    case "agent":
      return <Bot {...props} />;
    case "planner":
      return <Workflow {...props} />;
    case "workbench":
      return <SquareTerminal {...props} />;
    case "edge":
      return <Globe {...props} />;
    case "mcp":
      return <Plug {...props} />;
    case "identity":
      return <Fingerprint {...props} />;
    default:
      return <CircleDashed {...props} />;
  }
}

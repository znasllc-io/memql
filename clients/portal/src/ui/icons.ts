// The icon vocabulary, re-exported from one place so pages never import
// lucide-react directly -- one file says which icons exist in this product,
// and a rename or swap touches one line. Icons appear in nav, in buttons
// where they clarify, and in status -- never as decoration.

export {
  Archive,
  Blocks,
  Bot,
  Boxes,
  Building2,
  // TWO USES, both single chevrons:
  //
  //   * The Select's own arrow. A native <select> paints one per platform and
  //     sizes itself around it, so ui/Field.tsx removes the appearance
  //     entirely and draws this back -- as an element, so it inherits
  //     currentColor and is correct in both themes.
  //   * The rail's sub-section disclosure (memql#4527), Down when open and
  //     Right when closed.
  //
  // ChevronsLeft/Right (the DOUBLE chevrons, below) are the rail's own fold
  // control and must not be swapped for these -- a person reads the chevron
  // count as "how much folds".
  ChevronDown,
  ChevronRight,
  ChevronsLeft,
  ChevronsRight,
  Download,
  ExternalLink,
  // The per-page guide's entry point (memql#4652). An eye rather than a
  // question mark or an "i": what the control opens is a LOOK at the page --
  // a video, a description, the internals behind it -- and a question mark
  // reads as "help with a problem" on a screen where nothing has gone wrong.
  Eye,
  Gauge,
  Globe,
  GraduationCap,
  Inbox,
  KeyRound,
  LayoutGrid,
  LogOut,
  Monitor,
  Moon,
  Orbit,
  Plug,
  Plus,
  RefreshCw,
  Rocket,
  ScrollText,
  Search,
  Shield,
  ShieldCheck,
  // The Shopify connector's operator surface (memql#4398). A shopfront
  // rather than a shopping cart: what the page manages is the MERCHANT'S
  // store, not a basket.
  Store,
  Sun,
  Upload,
  User,
  Users,
  // The Fleet's two surfaces (memql#4355 / #4356). Monitor above already
  // carries "a machine"; Wrench is the workbench -- the cluster's own
  // sandboxed working directory, which is a tool this cluster owns rather
  // than a computer somebody sits at.
  Wrench,
  X,
} from "lucide-react";

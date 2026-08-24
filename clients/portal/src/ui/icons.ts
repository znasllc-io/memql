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
  ChevronsLeft,
  ChevronsRight,
  Download,
  ExternalLink,
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

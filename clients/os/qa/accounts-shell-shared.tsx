import { applyReducedMotionFixture } from "./reducedMotion";
import { createRoot } from "react-dom/client";
import { useEffect, useRef, useState } from "react";
import { Button, NextPage, RecordList, RecordRow } from "../src/kit";
import { EntryPending, useEntryTransition } from "../src/kit/EntryPending";
import { RankedList } from "../src/kit/statistics";
import { applyPackStyles } from "../src/themes/registry";
import "../src/styles/index.css";

// Fixture only: no service, credentials, writes or account-switch simulation.
const params = new URLSearchParams(location.search);
document.documentElement.dataset.theme = params.get("mode") === "light" ? "light" : "dark";
document.documentElement.dataset.osTheme = "graphite";
applyPackStyles([]);
if (params.get("motion") === "reduced") applyReducedMotionFixture();

function Entry() {
  const [pending, setPending] = useState(true);
  const transition = useEntryTransition(pending);
  useEffect(() => {
    if (!pending || params.get("hold") === "true") return;
    const timer = setTimeout(() => setPending(false), 1800);
    return () => clearTimeout(timer);
  }, [pending]);
  return transition.show ? <EntryPending label="Opening your desktop" leaving={transition.leaving} />
    : <main style={{ padding: 24 }}><Button onClick={() => setPending(true)}>Open again</Button><p>Destination ready</p></main>;
}
function Paging() {
  const [page, setPage] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const failed = useRef(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const loadMore = () => new Promise<void>(resolve => {
    setLoading(true); setError("");
    timer.current = setTimeout(() => {
      if (params.get("fail") === "true" && !failed.current) { failed.current = true; setError("The connection was interrupted."); }
      else setPage(value => value + 1);
      setLoading(false); resolve();
    }, 1800);
  });
  return <main style={{ maxWidth: 680, margin: "24px auto", padding: 20 }}>
    <h2>Recorded activity</h2>
    <div style={{ height: 330, overflow: "auto" }}>
      <RecordList label="Recorded activity">{Array.from({ length: (page + 1) * 10 }, (_, index) =>
        <RecordRow key={index} name={`Activity ${index + 1}`} secondary="Recorded earlier" />)}</RecordList>
      <NextPage feed={{ hasMore: page < 2, loadingMore: loading, loadMoreError: error, loadMore, progress: page }} />
    </div>
  </main>;
}
function Ranked() {
  const [all, setAll] = useState(false);
  return <main style={{ maxWidth: 680, margin: "24px auto", padding: 20 }}>
    {all ? <Button onClick={() => setAll(false)}>Back</Button> : null}
    <RankedList title={all ? "All pages" : "Pages"} rows={Array.from({ length: params.get("empty") === "true" ? 0 : 12 }, (_, i) => ({ key: String(i), label: i ? `/page-${i}` : "/", value: 120 - i * 10 }))}
      limit={all ? 12 : 8} showAllLabel="All pages" onShowAll={() => setAll(true)} />
  </main>;
}
createRoot(document.getElementById("root")!).render(params.get("view") === "entry" ? <Entry /> : params.get("view") === "ranked" ? <Ranked /> : <Paging />);

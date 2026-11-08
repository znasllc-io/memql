import { useEffect, useState, type ComponentType } from "react";
import { useWindowSearchLoading } from "../kit/windowSearch";
import { ContentSkeleton } from "../kit/ContentSkeleton";
import type { OsAppProps } from "./registry";

/** Fetch an installed app only when a window opens it. The registry remains
 * synchronous metadata, while the successfully loaded component stays in this
 * browser session. A failed import is released so Reload app can try again;
 * React.lazy alone retains a rejected promise across that remount. */
export function loadApp(name: string, load: () => Promise<ComponentType<OsAppProps>>): ComponentType<OsAppProps> {
  let loaded: ComponentType<OsAppProps> | null = null;
  let pending: Promise<ComponentType<OsAppProps>> | null = null;

  function read() {
    if (loaded) return Promise.resolve(loaded);
    if (!pending) pending = Promise.resolve().then(load).then(component => {
      loaded = component;
      return component;
    }).finally(() => { pending = null; });
    return pending;
  }

  function App(props: OsAppProps) {
    const [component, setComponent] = useState<ComponentType<OsAppProps> | null>(() => loaded);
    const [error, setError] = useState<unknown>(null);
    useEffect(() => {
      if (component) return;
      let active = true;
      void read().then(next => { if (active) setComponent(() => next); }, reason => {
        if (active) setError(reason instanceof Error ? reason : new Error(String(reason)));
      });
      return () => { active = false; };
    }, [component]);
    useWindowSearchLoading(component === null && error === null);
    if (error) throw error;
    const Body = component;
    return Body ? <Body {...props} /> : <ContentSkeleton label={`Opening ${name}`} />;
  }
  App.displayName = `Load${name.replaceAll(" ", "")}`;
  return App;
}

import { Component, Fragment, type ErrorInfo, type ReactNode } from "react";

import { Button, Notice } from "../kit";
import { report } from "../logs/capture";

// A render error in one app no longer blanks the desk (epic memql#4895).
//
// React unmounts the WHOLE tree above an uncaught render error -- every
// window, the dock, the wallpaper -- which is the one thing a desktop shell
// must never do in response to a fault in one of its apps. This boundary
// sits around each window's app body, so the fault is contained to that
// window: it shows the error's own sentence and a way to try again, and
// nothing else on the desk changes.
//
// It also REPORTS the line, and that is why it lives here rather than
// being a generic boundary: it knows the app id and the section it wraps
// exactly, from props, where the capture's focused-window context can only
// guess. "The right app id" is a property of this component rather than a
// hope about which window had focus when the render blew up.

interface Props {
  app: string;
  section: string;
  children: ReactNode;
}

interface State {
  error: Error | null;
  /** Bumped on reload so the children remount rather than re-render. */
  generation: number;
}

const COMPONENT_STACK_CHARS = 4_096;

function asError(thrown: unknown): Error {
  return thrown instanceof Error ? thrown : new Error(String(thrown));
}

export class WindowErrorBoundary extends Component<Props, State> {
  override state: State = { error: null, generation: 0 };

  static getDerivedStateFromError(thrown: unknown): Partial<State> {
    return { error: asError(thrown) };
  }

  override componentDidCatch(thrown: unknown, info: ErrorInfo): void {
    const error = asError(thrown);
    report({
      level: "error",
      app: this.props.app,
      section: this.props.section,
      component: `os.${this.props.app}`,
      message: error.message === "" ? "Render error" : error.message,
      error,
      attributes: {
        source: "boundary",
        componentStack: (info.componentStack ?? "").slice(0, COMPONENT_STACK_CHARS),
      },
    });
  }

  override componentDidUpdate(prev: Props): void {
    // Navigating to another section is a way out too: the nav stays outside
    // this boundary, and a person who moved on should not find the last
    // section's fault standing in front of the next one.
    if (this.state.error !== null && prev.section !== this.props.section) {
      this.reload();
    }
  }

  private reload = (): void => {
    this.setState((held) => ({ error: null, generation: held.generation + 1 }));
  };

  override render(): ReactNode {
    if (this.state.error !== null) {
      return (
        <div className="os-app-stack os-window-fault">
          <Notice
            tone="error"
            sentence="This app hit an error."
            next="The rest of the desk is unaffected. Reloading remounts the app in this window; the line has been recorded in Logs."
            detail={this.state.error.message}
          >
            <Button onClick={this.reload}>Reload app</Button>
          </Notice>
        </div>
      );
    }
    return <Fragment key={this.state.generation}>{this.props.children}</Fragment>;
  }
}

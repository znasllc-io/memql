import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AddButton, AddLink } from "../../src/kit/AddButton";

describe("shared creation affordances", () => {
  it("preserves native disabled behavior and never submits its containing form", () => {
    const add = vi.fn();
    const submit = vi.fn((event: React.FormEvent) => event.preventDefault());
    const { rerender } = render(<form onSubmit={submit}><AddButton label="Add a sender" onClick={add} /></form>);
    const button = screen.getByRole("button", { name: "Add a sender" });
    fireEvent.click(button);
    expect(add).toHaveBeenCalledOnce();
    expect(submit).not.toHaveBeenCalled();
    rerender(<AddButton label="Add a sender" onClick={add} disabled />);
    fireEvent.click(screen.getByRole("button", { name: "Add a sender" }));
    expect(add).toHaveBeenCalledOnce();
  });

  it("keeps a compact insertion's roving keyboard props and label", () => {
    const focus = vi.fn();
    const key = vi.fn();
    render(<AddButton label="Insert step 1" data-node="insert-0" tabIndex={0} onFocus={focus} onKeyDown={key} style={{ minWidth: 28, minHeight: 28 }} />);
    const button = screen.getByRole("button", { name: "Insert step 1" });
    button.focus();
    fireEvent.keyDown(button, { key: "ArrowDown" });
    expect(document.activeElement).toBe(button);
    expect(focus).toHaveBeenCalledOnce();
    expect(key).toHaveBeenCalledOnce();
    expect(button.title).toBe("Insert step 1");
  });

  it("keeps link navigation, caller sizing and contextual class", () => {
    render(<AddLink label="Create a repository" href="https://example.com/new" target="_blank" rel="noopener noreferrer" className="repository-create" style={{ minWidth: 44 }} />);
    const link = screen.getByRole("link", { name: "Create a repository" });
    expect(link.getAttribute("href")).toBe("https://example.com/new");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    expect(link.style.minWidth).toBe("44px");
    expect(link.classList.contains("repository-create")).toBe(true);
    link.focus();
    expect(document.activeElement).toBe(link);
  });
});

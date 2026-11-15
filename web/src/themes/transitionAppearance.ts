import { flushSync } from "react-dom";

let appearanceTransition: ViewTransition | undefined;

export function transitionAppearance(update: () => void) {
  appearanceTransition?.skipTransition();
  if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
    update();
    return;
  }
  // Dialogs and menus are portaled outside the workspace. Snapshotting them
  // while they close duplicates the overlay and blends different layouts.
  const overlayOpen = Array.from(document.querySelectorAll('[role="dialog"], [role="listbox"], [role="menu"]')).some(
    (element) => element.getClientRects().length > 0 && getComputedStyle(element).visibility !== "hidden"
  );
  if (typeof document.startViewTransition !== "function" || overlayOpen) {
    flushSync(update);
    document.querySelector(".workspace-main")?.animate?.([{ opacity: 0.8 }, { opacity: 1 }], { duration: 160, easing: "ease-out" });
    return;
  }
  appearanceTransition = document.startViewTransition(() => flushSync(update));
  // Rapid changes may skip a snapshot. The state update still runs.
  void appearanceTransition.ready.catch(() => {});
  void appearanceTransition.finished.catch(() => {});
}

// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { expect, it, vi } from "vitest";
import { RefreshContext, useResource } from "./common";

it("invalidates cached state immediately at reset and gates it through failed refreshes", async () => {
  const fetcher = vi.fn()
    .mockResolvedValueOnce(new Response('{"name":"Old sandbox"}'))
    .mockImplementationOnce(() => new Promise<Response>((resolve) => { finishResetRefresh = resolve; }))
    .mockResolvedValueOnce(new Response('{"name":"Fresh sandbox"}'));
  let finishResetRefresh!: (response: Response) => void;
  vi.stubGlobal("fetch", fetcher);
  vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
  const container = document.body.appendChild(document.createElement("div"));
  const root = createRoot(container);

  function State({ epoch }: { epoch: number }) {
    const state = useResource<{ name: string }>("/_sandbox/state", epoch);
    return <span>{state?.data?.name ?? state?.error ?? "Loading sandbox"}</span>;
  }

  async function render(epoch: number, revision = 0) {
    await act(async () => root.render(
      <RefreshContext.Provider value={{ auto: false, revision, refresh: () => {} }}>
        <State epoch={epoch} />
      </RefreshContext.Provider>,
    ));
  }

  try {
    await render(0);
    expect(container.textContent).toBe("Old sandbox");
    await render(1);
    expect(container.textContent).toBe("Loading sandbox");

    await act(async () => finishResetRefresh(new Response('{"error":"State unavailable"}', { status: 503 })));
    expect(container.textContent).toBe("State unavailable");

    await render(1, 1);
    expect(container.textContent).toBe("Fresh sandbox");
    expect(fetcher).toHaveBeenCalledTimes(3);
  } finally {
    act(() => root.unmount());
    container.remove();
    vi.unstubAllGlobals();
  }
});

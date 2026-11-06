import "./helpers/dom-environment";
import assert from "node:assert/strict";
import test from "node:test";
import { createElement, type ComponentProps } from "react";
import { createMemoryRouter, MemoryRouter, RouterProvider } from "react-router";
import { SearchPanel } from "../src/components/SearchPanel";
import { act, fireEvent, render } from "./helpers/dom";

// React's form event handling needs the browser's HTMLFormElement overload.
Object.defineProperty(globalThis, "FormData", { configurable: true, value: window.FormData });

type SearchProps = ComponentProps<typeof SearchPanel>;

function renderSearch(props: SearchProps = {}, initialEntries = ["/list"]) {
  const router = createMemoryRouter([
    { path: "*", element: createElement(SearchPanel, props) },
  ], { initialEntries });
  const view = render(createElement(RouterProvider, { router }));
  const input = view.getByRole("textbox", { name: "搜索关键词" }) as HTMLInputElement;
  const form = view.getByRole("search");
  const submit = view.getByRole("button", { name: "搜索", exact: true });
  return { ...view, router, input, form, submit };
}

for (const variant of ["default", "uiverse"] as const) {
  test(`${variant} search keeps typing local and only submits a trimmed keyword on activation`, async () => {
    const searches: string[] = [];
    const { input, submit, router } = renderSearch({
      variant, value: "旧关键词", onSearch: (q) => searches.push(q),
    });

    fireEvent.change(input, { target: { value: "  新关键词  " } });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 600)); });
    assert.deepEqual(searches, []);
    assert.equal(router.state.location.search, "");

    input.focus();
    fireEvent.click(submit);
    assert.deepEqual(searches, ["新关键词"]);
    assert.equal(input.value, "新关键词");
    assert.notEqual(document.activeElement, input);
  });
}

test("submitting updates the URL once, resets pagination and tags, and supports history restoration", async () => {
  const { input, submit, router } = renderSearch({ navigationPath: "/" }, [
    "/?q=原关键词&tag=旅行&sort=latest&feed=latest&page=3",
  ]);
  fireEvent.change(input, { target: { value: "  目标关键词  " } });
  assert.equal(new URLSearchParams(router.state.location.search).get("q"), "原关键词");
  fireEvent.click(submit);
  const params = new URLSearchParams(router.state.location.search);
  assert.equal(router.state.location.pathname, "/");
  assert.equal(params.get("q"), "目标关键词");
  assert.equal(params.has("tag"), false);
  assert.equal(params.has("page"), false);
  assert.equal(params.get("sort"), "latest");
  assert.equal(params.get("feed"), "latest");

  await act(async () => { await router.navigate(-1); });
  assert.equal(input.value, "原关键词");
  await act(async () => { await router.navigate(1); });
  assert.equal(input.value, "目标关键词");
});

test("clear only edits the draft, restores input focus, and waits for submission to remove the query", () => {
  const { input, submit, router, getByRole } = renderSearch({ variant: "uiverse" }, [
    "/list?q=关键词&tag=旅行&sort=latest",
  ]);
  const clear = getByRole("button", { name: "清空搜索" }) as HTMLButtonElement;
  fireEvent.click(clear);
  assert.equal(input.value, "");
  assert.equal(document.activeElement, input);
  assert.equal(clear.disabled, true);
  assert.equal(new URLSearchParams(router.state.location.search).get("q"), "关键词");

  fireEvent.click(submit);
  const params = new URLSearchParams(router.state.location.search);
  assert.equal(params.has("q"), false);
  assert.equal(params.get("tag"), "旅行");
  assert.equal(params.get("sort"), "latest");
});

test("IME confirmation Enter never submits, including Safari's legacy key code", () => {
  const searches: string[] = [];
  const { input, form, submit } = renderSearch({ onSearch: (q) => searches.push(q) });
  input.focus();
  fireEvent.compositionStart(input);
  fireEvent.change(input, { target: { value: "中文" } });
  assert.equal(fireEvent.keyDown(input, { key: "Enter", isComposing: true }), false);
  fireEvent.submit(form);
  assert.deepEqual(searches, []);
  assert.equal(document.activeElement, input);

  fireEvent.compositionEnd(input);
  assert.equal(fireEvent.keyDown(input, { key: "Enter", keyCode: 229 }), false);
  assert.equal(fireEvent.keyDown(input, { key: "Enter", isComposing: true }), false);
  assert.deepEqual(searches, []);
  assert.equal(document.activeElement, input);
  assert.equal(fireEvent.keyDown(input, { key: "Enter" }), true);
  fireEvent.click(submit);
  assert.deepEqual(searches, ["中文"]);
  assert.notEqual(document.activeElement, input);
});

test("loading blocks duplicate submissions while allowing a different keyword", () => {
  const searches: string[] = [];
  const { input, form, submit, getByRole } = renderSearch({
    value: "当前关键词", loading: true, onSearch: (q) => searches.push(q),
  });
  assert.equal(form.dataset.loading, "true");
  assert.equal(form.dataset.searching, undefined);
  assert.match(getByRole("status").textContent ?? "", /正在加载搜索结果/);
  assert.equal(submit.getAttribute("aria-disabled"), "true");
  fireEvent.click(submit);
  assert.deepEqual(searches, []);

  fireEvent.change(input, { target: { value: "下一个关键词" } });
  assert.equal(submit.hasAttribute("aria-disabled"), false);
  fireEvent.click(submit);
  assert.deepEqual(searches, ["下一个关键词"]);
});

test("the progress line only follows submissions and resets after completion or a cache hit", () => {
  const onSearch = () => {};
  const props = { variant: "uiverse" as const, value: "当前关键词", onSearch, loading: false };
  const wrapper = (next: SearchProps) => createElement(MemoryRouter, null, createElement(SearchPanel, next));
  const view = render(wrapper(props));
  const input = view.getByRole("textbox") as HTMLInputElement;
  const form = view.getByRole("search");

  input.focus();
  fireEvent.change(input, { target: { value: "下一次搜索" } });
  assert.equal(form.dataset.searching, undefined);
  fireEvent.submit(form);
  assert.notEqual(document.activeElement, input);
  view.rerender(wrapper({ ...props, value: "下一次搜索", loading: true }));
  assert.equal(form.dataset.searching, "true");
  view.rerender(wrapper({ ...props, value: "下一次搜索", loading: false }));
  assert.equal(form.dataset.searching, undefined);
  view.rerender(wrapper({ ...props, value: "下一次搜索", loading: true }));
  assert.equal(form.dataset.searching, undefined);

  view.rerender(wrapper({ ...props, value: "下一次搜索", loading: false }));
  fireEvent.change(input, { target: { value: "缓存搜索" } });
  fireEvent.submit(form);
  view.rerender(wrapper({ ...props, value: "缓存搜索", loading: false }));
  assert.equal(form.dataset.searching, undefined);
  view.rerender(wrapper({ ...props, value: "缓存搜索", loading: true }));
  assert.equal(form.dataset.searching, undefined);
});

test("loading updates preserve edits and external committed values restore the input", () => {
  const onSearch = () => {};
  const props = { variant: "uiverse" as const, value: "当前关键词", onSearch };
  const wrapper = (next: SearchProps) => createElement(MemoryRouter, null, createElement(SearchPanel, next));
  const view = render(wrapper(props));
  const input = view.getByRole("textbox") as HTMLInputElement;
  const form = view.getByRole("search");
  fireEvent.change(input, { target: { value: "尚未提交" } });
  view.rerender(wrapper({ ...props, loading: true }));
  assert.equal(input.value, "尚未提交");
  assert.equal(form.dataset.loading, "true");
  view.rerender(wrapper({ ...props, loading: false }));
  assert.equal(input.value, "尚未提交");
  assert.equal(form.dataset.loading, undefined);
  view.rerender(wrapper({ ...props, value: "来自历史记录" }));
  assert.equal(input.value, "来自历史记录");
});

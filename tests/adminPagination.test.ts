import assert from "node:assert/strict";
import test from "node:test";
import { createElement, useState } from "react";
import { fireEvent, render, within } from "./helpers/dom";
import { AdminPagination } from "../src/admin/AdminPagination";

test("pagination displays the current page and total for each item label", () => {
  const { container, rerender } = render(createElement(AdminPagination, {
    page: 2, totalPages: 3, total: 42, itemLabel: "视频", onPage() {},
  }));
  for (const itemLabel of ["视频", "标签"]) {
    rerender(createElement(AdminPagination, {
      page: 2, totalPages: 3, total: 42, itemLabel, onPage() {},
    }));
    const navigation = within(container).getByRole("navigation", { name: `${itemLabel}列表分页` });
    assert.equal(within(navigation).getByText(`第 2 / 3 页 · 共 42 个${itemLabel}`).getAttribute("aria-live"), "polite");
    assert.deepEqual(within(navigation).getAllByRole("button").map(button => button.textContent), ["上一页", "下一页"]);
  }
});

test("pagination navigates from the current page in both directions", () => {
  function Harness() {
    const [page, setPage] = useState(2);
    return createElement(AdminPagination, {
      page, totalPages: 3, total: 42, itemLabel: "视频", onPage: setPage,
    });
  }
  const { getByRole, getByText } = render(createElement(Harness));
  fireEvent.click(getByRole("button", { name: "上一页" }));
  getByText("第 1 / 3 页 · 共 42 个视频");
  fireEvent.click(getByRole("button", { name: "下一页" }));
  getByText("第 2 / 3 页 · 共 42 个视频");
  fireEvent.click(getByRole("button", { name: "下一页" }));
  getByText("第 3 / 3 页 · 共 42 个视频");
});

test("pagination blocks boundary and pending requests", () => {
  const pages: unknown[] = [];
  const props = { total: 42, itemLabel: "视频", onPage: (page: unknown) => { pages.push(page); } };
  const { getByRole, rerender } = render(createElement(AdminPagination, {
    ...props, page: 1, totalPages: 3,
  }));
  for (const { page, totalPages, pending, previousDisabled, nextDisabled } of [
    { page: 1, totalPages: 3, pending: false, previousDisabled: true, nextDisabled: false },
    { page: 3, totalPages: 3, pending: false, previousDisabled: false, nextDisabled: true },
    { page: 1, totalPages: 1, pending: false, previousDisabled: true, nextDisabled: true },
    { page: 2, totalPages: 3, pending: true, previousDisabled: true, nextDisabled: true },
  ]) {
    rerender(createElement(AdminPagination, { ...props, page, totalPages, pending }));
    const previous = getByRole("button", { name: "上一页" }) as HTMLButtonElement;
    const next = getByRole("button", { name: "下一页" }) as HTMLButtonElement;
    assert.equal(previous.disabled, previousDisabled);
    assert.equal(next.disabled, nextDisabled);
    if (previousDisabled) fireEvent.click(previous);
    if (nextDisabled) fireEvent.click(next);
  }
  assert.deepEqual(pages, []);
});

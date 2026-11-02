import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { expect, test } from "@playwright/test";

const row = (page: import("@playwright/test").Page, name: string) =>
  page.locator("#files-list .row").filter({ has: page.locator(".name", { hasText: new RegExp(`^${name}$`) }) });

test("the file panel uploads, moves, renames, downloads and deletes guest files", async ({ page }) => {
  await page.goto("./?relay=");
  const screen = page.locator(".xterm-rows");
  await expect(screen).toContainText("root@bootlet:~#", { timeout: 180_000 });

  await page.locator("#files-toggle").click();
  await expect(page.locator("#files-crumbs")).toContainText("root");
  await expect(page.locator("#files-msg")).toHaveText("This folder is empty."); // a fresh disk

  // Drop a 2 MB file from the "computer" onto the panel.
  const size = 2 << 20;
  const content = Buffer.alloc(size);
  for (let i = 0; i < size; i++) content[i] = (i * 7 + (i >> 11)) & 0xff;
  const sha1 = createHash("sha1").update(content).digest("hex");
  await page.evaluate((n) => {
    const data = new Uint8Array(n);
    for (let i = 0; i < n; i++) data[i] = (i * 7 + (i >> 11)) & 0xff;
    const dt = new DataTransfer();
    dt.items.add(new File([data], "upload.bin"));
    const list = document.getElementById("files-list")!;
    list.dispatchEvent(new DragEvent("dragover", { dataTransfer: dt, bubbles: true, cancelable: true }));
    list.dispatchEvent(new DragEvent("drop", { dataTransfer: dt, bubbles: true, cancelable: true }));
  }, size);
  await expect(row(page, "upload.bin")).toBeVisible({ timeout: 60_000 });
  await expect(row(page, "upload.bin")).toContainText("2.0 MB");
  await page.locator(".xterm").click();
  await page.keyboard.type("sha1sum /root/upload.bin\n");
  await expect(screen).toContainText(sha1);

  // New folder, named in place.
  await page.locator("#files-mkdir").click();
  const field = page.locator("#files-list input.rename");
  await expect(field).toBeFocused();
  await field.fill("box");
  await field.press("Enter");
  await expect(row(page, "box")).toBeVisible();

  // Drag the file onto the folder: it moves.
  await row(page, "upload.bin").dragTo(row(page, "box"));
  await expect(row(page, "upload.bin")).toHaveCount(0);
  await row(page, "box").dblclick();
  await expect(page.locator("#files-crumbs")).toContainText("box");
  await expect(row(page, "upload.bin")).toBeVisible();

  // Rename with F2.
  await row(page, "upload.bin").click();
  await page.keyboard.press("F2");
  await field.fill("renamed.bin");
  await field.press("Enter");
  await expect(row(page, "renamed.bin")).toBeVisible();

  // Double-click downloads a file.
  const [download] = await Promise.all([page.waitForEvent("download"), row(page, "renamed.bin").dblclick()]);
  expect(download.suggestedFilename()).toBe("renamed.bin");
  const got = await readFile((await download.path())!);
  expect(createHash("sha1").update(got).digest("hex")).toBe(sha1);

  // Delete (after confirming), then go back up with Backspace.
  await row(page, "renamed.bin").click();
  page.once("dialog", (d) => d.accept());
  await page.keyboard.press("Delete");
  await expect(row(page, "renamed.bin")).toHaveCount(0);
  await expect(page.locator("#files-msg")).toHaveText("This folder is empty.");
  await page.keyboard.press("Backspace");
  await expect(row(page, "box")).toBeVisible();

  // Dragging a guest item onto the terminal types its path.
  await page.locator(".xterm").click();
  await page.keyboard.type("ls -ld ");
  await row(page, "box").dragTo(page.locator("#terminal"));
  await page.keyboard.type("; echo li''sted\n");
  await expect(screen).toContainText("listed");
  await expect(screen).toContainText(/drwx.*\/root\/box/);
});

test("the System tab shows live guest and emulator stats", async ({ page }) => {
  await page.goto("./?relay=");
  const screen = page.locator(".xterm-rows");
  await expect(screen).toContainText("root@bootlet:~#", { timeout: 180_000 });
  await expect(page.locator("#status-text")).not.toContainText("MIPS"); // moved to the System tab

  await page.locator("#files-toggle").click();
  await page.getByRole("tab", { name: "System" }).click();
  const view = page.locator("#sys-view");
  await expect(view.locator(".sys-meta")).toContainText(/Up .* processes .* load/);
  const cpu = view.locator('[data-card="CPU"]').locator(".card-value");
  await expect(cpu).toHaveText(/^\d+%$/);
  await expect(view.locator('[data-card="Memory"]').locator(".card-value")).toHaveText(/MB of \d+ MB/);
  await expect(view.locator(".fs-row", { hasText: "Persistent disk" })).toContainText(/of (\d+ MB|1\.0 GB)/); // 1 GiB less the journal
  await expect(view.locator('[data-card="Emulator"]').locator(".card-value")).toHaveText(/MIPS$/);

  // A busy loop in the guest shows up as CPU use and in the process list.
  await page.locator(".xterm").click();
  await page.keyboard.type("timeout 20 sh -c 'while :; do :; done' &\n");
  await expect(view.locator(".procs tbody tr").first()).toContainText("sh", { timeout: 15_000 });
  await expect(cpu).toHaveText(/^(9\d|100)%$/, { timeout: 15_000 });

  // The choice of tab is remembered.
  await page.reload();
  await expect(page.getByRole("tab", { name: "System" })).toHaveAttribute("aria-selected", "true");
});

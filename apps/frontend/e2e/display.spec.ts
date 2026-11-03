import { expect, test, type Page } from "@playwright/test";

type Stats = { mips: number; activeMips: number; fps: number; engine: { engine: string; compiled: number } };
const stats = (page: Page) => page.evaluate(() => (globalThis as unknown as { __bootlet: { stats?: Stats } }).__bootlet.stats);

async function bootToPrompt(page: Page) {
  const screen = page.locator(".xterm-rows");
  await expect(screen).toContainText("root@bootlet:~#", { timeout: 180_000 });
  return screen;
}

test("the display shows the splash, then Linux, and takes keyboard input", async ({ page }, info) => {
  await page.goto("./?relay=");
  const display = page.locator("#display");
  await expect(display).toBeVisible();

  // The emulator draws its splash before Linux: frames arrive while booting.
  await expect.poll(async () => (await stats(page))?.fps ?? 0, { timeout: 120_000 }).toBeGreaterThan(0);
  await display.screenshot({ path: info.outputPath("splash.png") });

  const screen = await bootToPrompt(page);
  // Linux (fbcon) has taken over: the screen differs from the splash.
  const splash = await display.screenshot();
  await page.waitForTimeout(500);

  // Keys typed on the focused display reach the shell on tty1.
  await display.click();
  await expect(display).toBeFocused();
  await page.keyboard.type("touch /tmp/fromdisplay\n");
  await page.waitForTimeout(1500);
  const typed = await display.screenshot({ path: info.outputPath("console.png") });
  expect(typed.equals(splash)).toBe(false);

  await page.locator(".xterm").click();
  await page.keyboard.type("ls /tmp/fromdisplay && echo fo''und\n");
  await expect(screen).toContainText("found");

  // A key held down while the display loses focus is released in the guest.
  await page.keyboard.type("od -An -tx1 -w24 -N96 /dev/input/event0 | cut -c50-; echo re''leased\n");
  await page.waitForTimeout(1000);
  await display.click();
  await page.keyboard.down("KeyA");
  await page.waitForTimeout(300);
  await page.locator(".xterm").click(); // blur: every held key goes up
  await expect(screen).toContainText("released");
  await expect(screen).toContainText("01 00 1e 00 01 00 00 00"); // KEY_A down
  await expect(screen).toContainText("01 00 1e 00 00 00 00 00"); // KEY_A up
  await page.keyboard.up("KeyA");

  const s = await stats(page);
  expect(s?.activeMips ?? 0).toBeGreaterThan(0);
  // The browser runs guest code with the WebAssembly JIT by default.
  expect(s?.engine.engine).toBe("jit");
  expect(s?.engine.compiled ?? 0).toBeGreaterThan(0);
});

test("the display can be turned off", async ({ page }) => {
  await page.goto("./?relay=&display=0");
  await expect(page.locator("#display-pane")).toBeHidden();
  await expect(page.locator("#display-toggle")).toBeHidden();
  const screen = await bootToPrompt(page);
  await page.locator(".xterm").click();
  await page.keyboard.type("ls /dev/fb0 2>&1 | head -c 60; echo; echo ch''ecked\n");
  await expect(screen).toContainText("checked");
  await expect(screen).toContainText("No such file");
});

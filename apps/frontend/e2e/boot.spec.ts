import { expect, test } from "@playwright/test";

test("boots Linux to a shell and runs commands", async ({ page }) => {
  await page.goto("./");
  const screen = page.locator(".xterm-rows");
  const status = page.locator("#status-text");

  await expect(screen).toContainText("root@bootlet:~#", { timeout: 150_000 });
  await expect(status).toContainText("Running");

  await page.locator(".xterm").click();
  await page.keyboard.type("uname -sm; echo hello > /tmp/x; cat /tmp/x; ls /\n");
  await expect(screen).toContainText("Linux riscv64");
  await expect(screen).toContainText("hello");
  await expect(screen).toContainText("proc");

  await page.getByRole("button", { name: "Stop" }).click();
  await expect(status).toHaveText("Stopped");
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await expect(status).toContainText("Running");

  await page.getByRole("button", { name: "Restart" }).click();
  await expect(screen).toContainText("root@bootlet:~#", { timeout: 150_000 });
});

test("files in /data survive a page reload", async ({ page }) => {
  const screen = page.locator(".xterm-rows");
  const prompt = async () => {
    await expect(screen).toContainText("root@bootlet:~#", { timeout: 150_000 });
    await page.locator(".xterm").click();
  };
  await page.goto("./");
  await prompt();
  await expect(page.locator("#status")).toHaveAttribute("title", /saved in this browser/);
  const token = `t${Date.now()}`;
  await page.keyboard.type(`while ! mountpoint -q /data; do sleep 0.2; done; echo ${token} > /data/token; echo wr''ote\n`);
  await expect(screen).toContainText("wrote", { timeout: 30_000 });
  await page.waitForTimeout(3000); // the worker saves dirty blocks every 2s

  await page.reload();
  await prompt();
  await page.keyboard.type("while ! mountpoint -q /data; do sleep 0.2; done; cat /data/token\n");
  await expect(screen).toContainText(token, { timeout: 30_000 });
});

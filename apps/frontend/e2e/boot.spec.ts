import { expect, test, type Page } from "@playwright/test";

const RELAY = "ws://127.0.0.1:18089/"; // not 8089, so a relay you are running doesn't interfere

async function bootToPrompt(page: Page) {
  const screen = page.locator(".xterm-rows");
  await expect(screen).toContainText("root@bootlet:~#", { timeout: 180_000 });
  await page.locator(".xterm").click();
  return screen;
}

test("boots Linux to a shell and runs commands", async ({ page }) => {
  await page.goto("./?relay=");
  const screen = await bootToPrompt(page);
  const status = page.locator("#status-text");
  await expect(status).toContainText("Running");
  await expect(page.locator("#net")).toHaveText("Network off");

  await page.keyboard.type("uname -sm; echo hello > /tmp/x; cat /tmp/x; ls /\n");
  await expect(screen).toContainText("Linux riscv64");
  await expect(screen).toContainText("hello");
  await expect(screen).toContainText("proc");

  await page.getByRole("button", { name: "Stop" }).click();
  await expect(status).toHaveText("Stopped");
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await expect(status).toContainText("Running");

  await page.getByRole("button", { name: "Restart" }).click();
  await bootToPrompt(page);
});

test("the filesystem survives a reload until Reset disk", async ({ page }) => {
  await page.goto("./?relay=");
  let screen = await bootToPrompt(page);
  await expect(page.locator("#status")).toHaveAttribute("title", /saved in this browser/);
  const token = `t${Date.now()}`;
  await page.keyboard.type(`echo ${token} > /root/token; sync; echo wr''ote\n`);
  await expect(screen).toContainText("wrote");
  await page.waitForTimeout(3000); // the worker saves dirty blocks every 2s

  await page.reload();
  screen = await bootToPrompt(page);
  await page.keyboard.type("cat /root/token\n");
  await expect(screen).toContainText(token);

  page.once("dialog", (d) => d.accept());
  await page.getByRole("button", { name: "Reset disk" }).click();
  screen = await bootToPrompt(page);
  await page.keyboard.type("cat /root/token 2>&1 | head -c 40; echo; echo che''cked\n");
  await expect(screen).toContainText("checked");
  await expect(screen).not.toContainText(token);
});

test("the guest reaches the network through a Wisp relay", async ({ page }) => {
  await page.goto(`./?relay=${encodeURIComponent(RELAY)}`);
  const screen = await bootToPrompt(page);
  await expect(page.locator("#net")).toHaveText("Online");
  // 10.0.2.2 is the gateway, which the relay maps to the host (the preview server).
  await page.keyboard.type("wget -qO- http://10.0.2.2:4174/bootlet/ | grep -o '<title>[a-z]*</title>'\n");
  await expect(screen).toContainText("<title>bootlet</title>");
});

test("networking can be connected from the network dialog", async ({ page }) => {
  await page.goto("./");
  const screen = await bootToPrompt(page);
  await expect(page.locator("#net")).toHaveText("Network off"); // no auto-connect on first visit

  await page.locator("#net").click();
  await page.locator("#net-url").fill(RELAY);
  await page.locator("#net-connect").click();
  await expect(page.locator("#net")).toHaveText("Online");
  await page.keyboard.press("Escape");
  await page.locator(".xterm").click();
  await page.keyboard.type("wget -qO- http://10.0.2.2:4174/bootlet/ | grep -o '<title>[a-z]*</title>'\n");
  await expect(screen).toContainText("<title>bootlet</title>");

  // The relay is remembered: after a reload the page connects by itself.
  await page.reload();
  await expect(page.locator("#net")).toHaveText("Online");
});

test("a large disk is read on demand, not held in memory", async ({ page }) => {
  const emulatorMiB = async () => {
    await expect(page.locator("#status")).toHaveAttribute("title", /Emulator memory: \d+ MiB/);
    return Number((await page.locator("#status").getAttribute("title"))!.match(/Emulator memory: (\d+) MiB/)![1]);
  };
  await page.goto("./?relay=");
  let screen = await bootToPrompt(page);
  const fresh = await emulatorMiB();
  await page.keyboard.type(
    "dd if=/dev/zero of=/root/big bs=1M count=64 2>/dev/null; echo marker >> /root/big; " +
      "sha1sum /root/big > /root/big.sha1; echo wr''ote\n",
  );
  await expect(screen).toContainText("wrote", { timeout: 180_000 });
  await page.waitForTimeout(6000); // writeback (~1 s), journal commit (1 s), save (2 s)

  await page.reload();
  screen = await bootToPrompt(page);
  await page.keyboard.type("sha1sum -c /root/big.sha1\n");
  await expect(screen).toContainText("/root/big: OK", { timeout: 180_000 });

  // Holding the disk in memory would add the 64 MB file and its copies;
  // read on demand, a reload costs about what a fresh boot does.
  const reloaded = await emulatorMiB();
  console.log(`emulator memory: fresh boot ${fresh} MiB, after reload with 64 MB on disk ${reloaded} MiB`);
  expect(reloaded).toBeLessThan(fresh + 64);
});

// End-to-end check of the customer and admin journeys in a real browser, at desktop and phone sizes.
//
// Needs a running stack: the API with an embedded worker (XENOS_RUN_WORKER=true), the in-memory fakes for
// iSpend and Proxmox, and XENOS_FAKE_ISPEND_CREDIT_UUSDT so new accounts can afford a VM. See e2e/README.md.
//
//   E2E_BASE_URL   default http://localhost:8080
//   E2E_API_LOG    path of the API's log file; verification emails are logged (not sent) in development
//   E2E_PROMOTE    shell command that makes the email in $E2E_EMAIL an admin, e.g. `xenosctl admin grant "$E2E_EMAIL"`
//   E2E_SHOTS      directory for screenshots (optional)
import { chromium } from "playwright-core";
import { execSync } from "node:child_process";
import { readFileSync, mkdirSync } from "node:fs";

const base = process.env.E2E_BASE_URL ?? "http://localhost:8080";
const logPath = process.env.E2E_API_LOG;
const shots = process.env.E2E_SHOTS;
const exe = process.env.E2E_CHROMIUM ?? "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
const email = `e2e-${Date.now()}@example.com`;
const password = "correct-horse-battery";
const failures = [];

if (shots) mkdirSync(shots, { recursive: true });
const step = (msg) => console.log(`• ${msg}`);
const check = (cond, msg) => {
  if (!cond) {
    failures.push(msg);
    console.error(`  ✗ ${msg}`);
  }
};

function emailLink(path) {
  // The development mailer logs each email (recipient and body, which holds the link) on one line.
  const re = new RegExp(`${path}#token=([A-Za-z0-9_-]+)`);
  for (const line of readFileSync(logPath, "utf8").split("\n").reverse()) {
    const m = line.includes(email) ? re.exec(line) : null;
    if (m) return m[1];
  }
  throw new Error(`no ${path} link for ${email} in ${logPath}`);
}

const browser = await chromium.launch({ executablePath: exe, args: ["--no-sandbox"] });

async function newPage(viewport) {
  const ctx = await browser.newContext({ viewport });
  const page = await ctx.newPage();
  const problems = [];
  page.on("pageerror", (e) => problems.push(`page error: ${e.message}`));
  page.on("console", (m) => {
    if (m.type() !== "error") return;
    // The browser logs every 4xx API answer as a console error. Those are expected here (the anonymous
    // session check returns 401, and the journey submits an invalid key on purpose); anything else,
    // such as a Content-Security-Policy violation or a crash, is a real problem.
    if (/Failed to load resource: the server responded with a status of 4\d\d/.test(m.text())) return;
    problems.push(`console: ${m.text()}`);
  });
  return { page, ctx, problems };
}
const shot = async (page, name) => shots && (await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true }));
const noOverflow = async (page, where) => {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  check(over <= 1, `${where}: page scrolls sideways by ${over}px`);
};

// ---------------------------------------------------------------- desktop journey
{
  const { page, problems } = await newPage({ width: 1280, height: 900 });

  step("landing page shows plans");
  await page.goto(base);
  await page.getByRole("heading", { name: /paid for in naira/i }).waitFor();
  await page.getByText("nano", { exact: false }).first().waitFor();
  await shot(page, "01-landing");

  step("sign up (acceptable use must be accepted)");
  await page.getByRole("link", { name: "Create an account" }).click();
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Phone").fill("+2348012345678");
  await page.getByLabel("Password").fill(password);
  check(await page.getByRole("button", { name: "Sign up" }).isDisabled(), "Sign up must be disabled until the policy is accepted");
  await page.getByRole("checkbox").check();
  await page.getByRole("button", { name: "Sign up" }).click();
  await page.getByRole("heading", { name: "Overview" }).waitFor();
  await page.getByText(/Verify your email/).first().waitFor();
  await shot(page, "02-overview-unverified");

  step("create VM is blocked without an SSH key");
  await page.goto(`${base}/vms/new`);
  await page.getByText("Add an SSH key first").waitFor();
  check(await page.getByRole("button", { name: "Create VM" }).isDisabled(), "Create VM must be disabled with the reason shown");

  step("add an SSH key");
  await page.goto(`${base}/ssh-keys`);
  await page.getByLabel("Name").fill("laptop");
  await page.getByLabel("Public key").fill("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl e2e");
  await page.getByRole("button", { name: "Add key" }).click();
  await page.getByText("SHA256:").waitFor();
  await page.getByLabel("Public key").fill("not a key");
  await page.getByLabel("Name").fill("bad");
  await page.getByRole("button", { name: "Add key" }).click();
  await page.getByRole("alert").waitFor();

  step("verify email from the emailed link");
  await new Promise((r) => setTimeout(r, 500));
  const token = emailLink("verify-email");
  await page.goto(`${base}/verify-email#token=${token}`);
  await page.getByText("Your email is verified").waitFor();

  step("wallet shows the funding details once verified");
  await page.goto(`${base}/wallet`);
  await page.getByText("Compute credit", { exact: true }).waitFor();
  await page.getByText("Account number", { exact: true }).waitFor();
  await shot(page, "03-wallet");

  step("create a VM (hostname is validated)");
  await page.goto(`${base}/vms/new`);
  await page.getByRole("radio", { name: /small/i }).check({ force: true });
  await page.getByLabel(/Hostname/).fill("-Bad_Name");
  await page.getByText(/lowercase letters, digits and hyphens/).first().waitFor();
  check(await page.getByRole("button", { name: "Create VM" }).isDisabled(), "Create VM must be disabled for a bad hostname");
  await page.getByLabel(/Hostname/).fill("web-1");
  await shot(page, "04-create");
  await page.getByRole("button", { name: "Create VM" }).click();

  step("VM page updates by itself from provisioning to running");
  await page.getByRole("heading", { name: "web-1" }).waitFor();
  await page.getByText("running", { exact: true }).waitFor({ timeout: 40_000 });
  await page.getByText(/^ssh root@203\.0\.113\./).waitFor();
  await shot(page, "05-vm-running");

  step("stop, then start");
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await page.getByText("stopped", { exact: true }).waitFor({ timeout: 30_000 });
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await page.getByText("running", { exact: true }).waitFor({ timeout: 30_000 });

  step("snapshot: take one, restore it, delete it");
  await page.getByLabel("Snapshot name").fill("before upgrade");
  await page.getByRole("button", { name: "Take snapshot" }).click();
  await page.getByText("before upgrade").waitFor();
  const restore = page.getByRole("button", { name: "Restore" });
  await restore.waitFor();
  await page.waitForFunction(() => [...document.querySelectorAll("li button")].some((b) => b.textContent === "Restore" && !b.disabled), null, { timeout: 40_000 });
  await restore.click();
  const restoreOk = page.getByRole("dialog").getByRole("button", { name: "Restore" });
  check(await restoreOk.isDisabled(), "restore must stay disabled until the VM name is typed");
  await page.getByRole("dialog").getByRole("textbox").fill("web-1");
  await restoreOk.click();
  await page.getByText(/Restoring a snapshot/).waitFor({ timeout: 20_000 });
  await page.getByText(/Restoring a snapshot/).waitFor({ state: "detached", timeout: 40_000 });
  await page.locator("li").getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByText("before upgrade").waitFor({ state: "detached", timeout: 40_000 });
  await shot(page, "05b-snapshots");

  step("resize to a larger plan");
  await page.getByRole("button", { name: /^medium:/ }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Resize" }).click();
  await page.getByText(/Resizing\./).waitFor({ timeout: 20_000 });
  await page.getByText(/^medium · /).waitFor({ timeout: 60_000 });
  await page.getByText("running", { exact: true }).waitFor({ timeout: 30_000 });
  await shot(page, "05c-resized");

  step("rebuild from another template (typing the name confirms)");
  await page.getByRole("button", { name: "Rebuild…" }).click();
  const rebuildOk = page.getByRole("dialog").getByRole("button", { name: "Erase and rebuild" });
  check(await rebuildOk.isDisabled(), "rebuild must stay disabled until the VM name is typed");
  await page.getByRole("dialog").getByRole("combobox").selectOption("debian-12");
  await page.getByRole("dialog").getByRole("textbox").fill("web-1");
  await rebuildOk.click();
  await page.getByText(/Rebuilding\./).waitFor({ timeout: 20_000 });
  await page.getByText(/ · debian-12 · /).waitFor({ timeout: 60_000 });
  await page.getByText("running", { exact: true }).waitFor({ timeout: 30_000 });
  await shot(page, "05d-rebuilt");

  step("browser console connects through our server (the fake host speaks no real VNC, so only the transport is checked)");
  const before = problems.length;
  await page.getByRole("link", { name: "Console" }).click();
  await page.getByRole("heading", { name: /^Console: web-1/ }).waitFor();
  await page.getByText(/Connecting…|Connected|Disconnected|Could not connect/).first().waitFor({ timeout: 20_000 });
  await new Promise((r) => setTimeout(r, 1000));
  const fresh = problems.splice(before); // noVNC logs protocol errors against the fake; a CSP block would be ours
  check(!fresh.some((m) => /Content.Security|Refused to/i.test(m)), `console blocked by CSP:\n    ${fresh.join("\n    ")}`);
  await page.goto(page.url().replace(/\/console$/, ""));
  await page.getByRole("heading", { name: "web-1" }).waitFor();

  step("wallet charge for the running hour appears");
  await page.goto(`${base}/wallet`);
  await page.getByText("web-1").first().waitFor({ timeout: 90_000 });

  step("account page and password change");
  await page.goto(`${base}/account`);
  await page.getByLabel("Current password").fill(password);
  await page.getByLabel("New password").fill("another-long-password");
  await page.getByRole("button", { name: "Change password" }).click();
  await page.getByText("Password changed").waitFor();
  await page.goto(`${base}/vms`);
  await page.getByText("web-1").waitFor(); // still signed in after the session rotated

  step("delete needs the VM name typed");
  await page.getByText("web-1").click();
  await page.getByRole("button", { name: "Delete" }).click();
  const confirm = page.getByRole("dialog").getByRole("button", { name: "Delete VM" });
  check(await confirm.isDisabled(), "delete must stay disabled until the name is typed");
  await page.getByRole("dialog").getByRole("textbox").fill("web-1");
  await confirm.click();
  await page.getByText("No VMs yet").waitFor({ timeout: 40_000 });

  step("customers cannot open the admin area");
  await page.goto(`${base}/admin`);
  await page.getByRole("heading", { name: "Overview" }).waitFor();
  check(!(await page.getByRole("link", { name: "Admin" }).count()), "admin link must not be shown to customers");

  check(problems.length === 0, `desktop console problems:\n    ${problems.join("\n    ")}`);
  await page.context().close();
}

// ---------------------------------------------------------------- admin
if (process.env.E2E_PROMOTE) {
  step("promote the user to admin and review the admin area");
  execSync(process.env.E2E_PROMOTE, { env: { ...process.env, E2E_EMAIL: email }, stdio: "inherit" });
  const { page, problems } = await newPage({ width: 1280, height: 900 });
  await page.goto(`${base}/login`);
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("another-long-password");
  await page.getByRole("button", { name: "Log in" }).click();
  await page.getByRole("link", { name: "Admin" }).first().click();
  await page.getByRole("heading", { name: "Admin" }).waitFor();
  await page.getByRole("link", { name: email }).first().waitFor();
  await shot(page, "10-admin-users");

  await page.getByRole("link", { name: email }).first().click();
  await page.getByText("Account controls", { exact: true }).waitFor();
  await page.getByLabel("Amount (USDT)").fill("0.005");
  await page.getByLabel("Note").fill("e2e goodwill credit");
  await page.getByRole("button", { name: /Credit 0\.0050 USDT/ }).click();
  await page.getByText("e2e goodwill credit").waitFor();
  await shot(page, "11-admin-user");

  for (const [tab, text] of [["VMs", /No VMs match|Force stop/], ["Capacity", /IPv4 addresses in use/], ["Jobs", /No failed jobs|Retry/], ["Revenue", /Naira converted/]]) {
    await page.getByRole("navigation", { name: "Admin" }).getByRole("link", { name: tab }).click();
    await page.getByText(text).first().waitFor();
  }
  await shot(page, "12-admin-revenue");
  check(problems.length === 0, `admin console problems:\n    ${problems.join("\n    ")}`);
  await page.context().close();
}

// ---------------------------------------------------------------- phone
{
  const { page, problems } = await newPage({ width: 375, height: 760 });
  step("phone: log in, navigate with the menu, no sideways scrolling");
  await page.goto(`${base}/login`);
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("another-long-password");
  await page.getByRole("button", { name: "Log in" }).click();
  await page.getByRole("heading", { name: "Overview" }).waitFor();
  await noOverflow(page, "overview");
  await shot(page, "20-phone-overview");

  for (const [name, path] of [["VMs", "/vms"], ["Create", "/vms/new"], ["Wallet", "/wallet"], ["SSH keys", "/ssh-keys"], ["Account", "/account"], ["Admin users", "/admin"], ["Admin VMs", "/admin/vms"], ["Admin revenue", "/admin/revenue"]]) {
    await page.goto(`${base}${path}`);
    await page.waitForLoadState("networkidle");
    await noOverflow(page, `phone ${name}`);
  }
  await page.goto(`${base}/wallet`);
  await shot(page, "21-phone-wallet");
  await page.getByRole("button", { name: "Menu" }).click();
  await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: "VMs" }).click();
  await page.getByRole("heading", { name: "Virtual machines" }).waitFor();

  await page.goto(`${base}/vms/new`);
  await shot(page, "22-phone-create");
  check(problems.length === 0, `phone console problems:\n    ${problems.join("\n    ")}`);
  await page.context().close();
}

await browser.close();
if (failures.length) {
  console.error(`\nFAILED (${failures.length})`);
  process.exit(1);
}
console.log("\nAll journeys passed.");

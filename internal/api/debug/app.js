const $ = (id) => document.getElementById(id);
let logJob = null;

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (method !== "GET" || !res.ok)
    $("out").textContent =
      `${method} ${path} -> ${res.status}\n${JSON.stringify(data, null, 2)}`;
  return res.ok ? data : null;
}

function el(tag, text, onclick) {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (onclick) e.onclick = onclick;
  return e;
}

const org = () => $("org").value;

async function loadMe() {
  const me = await api("GET", "/v1/me");
  if (!me) {
    $("who").textContent = "not logged in";
    return;
  }
  $("who").textContent =
    `${me.user.email} | platform: [${me.platform_permissions.join(", ")}]`;
  const keep = org();
  $("org").replaceChildren(
    ...me.orgs.map((o) => {
      const opt = el("option", `${o.name} (${o.role})`);
      opt.value = o.id;
      return opt;
    }),
  );
  if (keep) $("org").value = keep;
  await loadOrg();
}

async function loadOrg() {
  if (!org()) return;
  const base = `/v1/orgs/${org()}`;
  const [jobs, queues] = await Promise.all([
    api("GET", `${base}/jobs?limit=50`),
    api("GET", `${base}/queues`),
  ]);

  $("queues").replaceChildren(
    ...((queues && queues.queues) || []).flatMap((q) => [
      el(
        "span",
        `${q.name}: ${q.pending} pending, ${q.running} running${q.paused ? " (PAUSED) " : " "}`,
      ),
      el("button", q.paused ? "resume" : "pause", () =>
        act(
          "POST",
          `${base}/queues/${q.name}/${q.paused ? "resume" : "pause"}`,
        ),
      ),
      el("br"),
    ]),
  );

  const head = el("tr");
  [
    "id",
    "queue",
    "status",
    "attempts",
    "image",
    "command",
    "error",
    "created",
    "",
  ].forEach((h) => head.append(el("th", h)));
  const rows = ((jobs && jobs.jobs) || []).map((j) => {
    const tr = el("tr");
    [
      j.id.slice(0, 8),
      j.queue,
      j.status + (j.cancel_requested ? " (cancelling)" : ""),
      `${j.attempts}/${j.max_attempts}`,
      j.image,
      JSON.stringify(j.command),
      j.last_error || "",
      new Date(j.created_at).toLocaleTimeString(),
    ].forEach((v) => tr.append(el("td", v)));
    const td = el("td");
    td.append(
      el("button", "logs", () => {
        logJob = j.id;
        loadLogs();
      }),
      el("button", "cancel", () => act("POST", `${base}/jobs/${j.id}/cancel`)),
      el("button", "retry", () => act("POST", `${base}/jobs/${j.id}/retry`)),
      el("button", "delete", () => act("DELETE", `${base}/jobs/${j.id}`)),
    );
    tr.append(td);
    return tr;
  });
  $("jobs").replaceChildren(head, ...rows);

  const members = await api("GET", `${base}/members`);
  $("members").textContent = members
    ? members.members
        .map((m) => `${m.email}  ${m.role}  [${m.permissions.join(", ")}]`)
        .join("\n")
    : "";
  await loadLogs();
}

async function loadLogs() {
  if (!logJob || !org()) return;
  $("logjob").textContent = logJob.slice(0, 8);
  const r = await api(
    "GET",
    `/v1/orgs/${org()}/jobs/${logJob}/logs?limit=1000`,
  );
  $("logs").textContent = r
    ? r.logs
        .map((l) => `[${l.attempt}] ${l.stream.padEnd(6)} ${l.line}`)
        .join("\n")
    : "(no logs / no access)";
}

async function act(method, path, body) {
  await api(method, path, body);
  await loadOrg();
}

const creds = () => ({
  email: $("email").value,
  password: $("password").value,
});
$("login").onclick = async () => {
  await api("POST", "/v1/auth/login", creds());
  loadMe();
};
$("register").onclick = async () => {
  await api("POST", "/v1/auth/register", creds());
  loadMe();
};
$("logout").onclick = async () => {
  await api("POST", "/v1/auth/logout");
  location.reload();
};
$("neworg").onclick = async () => {
  await api("POST", "/v1/orgs", { name: $("orgname").value });
  loadMe();
};
$("refresh").onclick = loadMe;
$("org").onchange = () => {
  logJob = null;
  $("logs").textContent = "";
  loadOrg();
};
$("enqueue").onclick = () => {
  let command;
  try {
    command = JSON.parse($("command").value);
  } catch {
    $("out").textContent = "command must be a JSON array of strings";
    return;
  }
  act("POST", `/v1/orgs/${org()}/jobs`, {
    image: $("image").value,
    command,
    queue: $("queue").value,
    timeout_seconds: Number($("timeout").value),
    max_attempts: Number($("attempts").value),
  });
};
$("admin").onclick = async () => {
  const [stats, workers] = await Promise.all([
    api("GET", "/v1/admin/stats"),
    api("GET", "/v1/admin/workers"),
  ]);
  $("adminout").textContent = stats
    ? JSON.stringify({ stats, workers: workers.workers }, null, 2)
    : "not a platform admin (404)";
};

setInterval(() => {
  if ($("auto").checked && org()) loadOrg();
}, 2000);
loadMe();

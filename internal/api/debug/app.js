const $ = (id) => document.getElementById(id);
let logJob = null;
let runView = null;

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
  return res.ok ? data || {} : null;
}

function el(tag, text, onclick) {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (onclick) e.onclick = onclick;
  return e;
}

function table(id, headers, rows) {
  const head = el("tr");
  headers.forEach((h) => head.append(el("th", h)));
  $(id).replaceChildren(head, ...rows);
}

function row(values, buttons) {
  const tr = el("tr");
  values.forEach((v) => tr.append(el("td", v)));
  const td = el("td");
  td.append(...buttons);
  tr.append(td);
  return tr;
}

const org = () => $("org").value;
const base = () => `/v1/orgs/${org()}`;

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
  await loadAll();
}

async function loadAll() {
  await Promise.all([loadOrg(), loadDetails()]);
}

async function loadOrg() {
  if (!org()) return;
  const [jobs, queues, stats, runs] = await Promise.all([
    api("GET", `${base()}/jobs?limit=50`),
    api("GET", `${base()}/queues`),
    api("GET", `${base()}/stats`),
    api("GET", `${base()}/runs`),
  ]);

  $("stats").textContent = stats
    ? JSON.stringify(stats.jobs_by_status)
    : "(no access)";

  $("queues").replaceChildren(
    ...((queues && queues.queues) || []).flatMap((q) => [
      el(
        "span",
        `${q.name}: ${q.pending} pending, ${q.running} running${q.paused ? " (PAUSED) " : " "}`,
      ),
      el("button", q.paused ? "resume" : "pause", () =>
        act(
          "POST",
          `${base()}/queues/${q.name}/${q.paused ? "resume" : "pause"}`,
        ),
      ),
      el("br"),
    ]),
  );

  table(
    "jobs",
    [
      "id",
      "name",
      "queue",
      "status",
      "attempts",
      "image",
      "network",
      "command",
      "error",
      "created",
      "",
    ],
    ((jobs && jobs.jobs) || []).map((j) =>
      row(
        [
          j.id.slice(0, 8),
          j.name,
          j.queue,
          j.status + (j.cancel_requested ? " (cancelling)" : ""),
          `${j.attempts}/${j.max_attempts}`,
          j.image,
          j.network,
          JSON.stringify(j.command),
          j.last_error || "",
          new Date(j.created_at).toLocaleTimeString(),
        ],
        jobButtons(j),
      ),
    ),
  );

  $("runs").replaceChildren(
    ...((runs && runs.runs) || []).flatMap((r) => [
      el(
        "span",
        `${r.id.slice(0, 8)} ${r.name}: ${r.status} (${new Date(r.created_at).toLocaleTimeString()}) `,
      ),
      el("button", "view", () => {
        runView = r.id;
        loadRun();
      }),
      el("button", "cancel", () =>
        act("POST", `${base()}/runs/${r.id}/cancel`),
      ),
      el("br"),
    ]),
  );

  await Promise.all([loadLogs(), loadRun()]);
}

function jobButtons(j) {
  return [
    el("button", "logs", () => {
      logJob = j.id;
      loadLogs();
    }),
    el("button", "cancel", () => act("POST", `${base()}/jobs/${j.id}/cancel`)),
    el("button", "retry", () => act("POST", `${base()}/jobs/${j.id}/retry`)),
    el("button", "delete", () => act("DELETE", `${base()}/jobs/${j.id}`)),
  ];
}

async function loadDetails() {
  if (!org()) return;
  const [info, images, hosts, workflows, invites, members] = await Promise.all(
    [
      api("GET", base()),
      api("GET", `${base()}/images`),
      api("GET", `${base()}/hosts`),
      api("GET", `${base()}/workflows`),
      api("GET", `${base()}/invites`),
      api("GET", `${base()}/members`),
    ],
  );

  if (info) {
    const st = info.org.settings;
    $("orginfo").textContent =
      `${info.org.name} | ${info.role} [${info.permissions.join(", ")}] | ${info.org.members} members | ` +
      `server internet: ${info.server.internet_available}, server max timeout: ${info.server.max_timeout_seconds}s`;
    $("allownetwork").checked = st.allow_network;
    $("maxtimeout").value = st.max_timeout_seconds;
    $("maxattempts").value = st.max_attempts;
    $("maxrunning").value = st.max_running;
    $("maxpending").value = st.max_pending;
  }

  catalogue("images", images && images.images);
  catalogue("hosts", hosts && hosts.hosts);

  $("workflows").replaceChildren(
    ...((workflows && workflows.workflows) || []).flatMap((w) => [
      el("span", `${w.id.slice(0, 8)} ${w.name} (${w.nodes} jobs) `),
      el("button", "run", () =>
        act("POST", `${base()}/workflows/${w.id}/run`),
      ),
      el("button", "edit", async () => {
        const r = await api("GET", `${base()}/workflows/${w.id}`);
        if (!r) return;
        $("wfname").value = r.workflow.name;
        $("wfgraph").value = JSON.stringify(r.workflow.graph);
      }),
      el("button", "save graph", () => {
        const graph = readGraph();
        if (graph)
          act("PUT", `${base()}/workflows/${w.id}`, {
            name: $("wfname").value || w.name,
            graph,
          });
      }),
      el("button", "delete", () =>
        act("DELETE", `${base()}/workflows/${w.id}`),
      ),
      el("br"),
    ]),
  );

  $("invites").replaceChildren(
    ...((invites && invites.invites) || []).flatMap((i) => [
      el(
        "span",
        `${i.id.slice(0, 8)} ${i.email || "(anyone)"} ${i.role} by ${i.invited_by}, expires ${new Date(i.expires_at).toLocaleString()} `,
      ),
      el("button", "revoke", () =>
        act("DELETE", `${base()}/invites/${i.id}`),
      ),
      el("br"),
    ]),
  );

  $("members").textContent = members
    ? members.members
        .map((m) => `${m.email}  ${m.role}  [${m.permissions.join(", ")}]`)
        .join("\n")
    : "(no access)";
}

function catalogue(id, items) {
  $(id).replaceChildren(
    ...(items || []).flatMap((it) => [
      el(
        "span",
        `${it.org_enabled ? "[on] " : "[off]"} ${it.value} (${it.label}) `,
      ),
      el("button", it.org_enabled ? "disable" : "enable", () =>
        act("PUT", `${base()}/${id}/${it.id}`, { enabled: !it.org_enabled }),
      ),
      el("br"),
    ]),
  );
}

async function loadLogs() {
  if (!logJob || !org()) return;
  $("logjob").textContent = logJob.slice(0, 8);
  const r = await api("GET", `${base()}/jobs/${logJob}/logs?limit=1000`);
  $("logs").textContent = r
    ? r.logs
        .map((l) => `[${l.attempt}] ${l.stream.padEnd(6)} ${l.line}`)
        .join("\n")
    : "(no logs / no access)";
}

async function loadRun() {
  if (!runView || !org()) return;
  const r = await api("GET", `${base()}/runs/${runView}`);
  if (!r) return;
  $("runid").textContent =
    `${runView.slice(0, 8)} ${r.run.name}: ${r.run.status}`;
  table(
    "runjobs",
    ["node", "job", "status", "attempts", "error", ""],
    r.jobs.map((j) =>
      row(
        [
          j.node_id || "",
          j.id.slice(0, 8),
          j.status,
          `${j.attempts}/${j.max_attempts}`,
          j.last_error || "",
        ],
        jobButtons(j),
      ),
    ),
  );
}

async function act(method, path, body) {
  await api(method, path, body);
  await loadAll();
}

function readGraph() {
  try {
    return JSON.parse($("wfgraph").value);
  } catch {
    $("out").textContent = "workflow graph must be valid JSON";
    return null;
  }
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
$("lookupinvite").onclick = () =>
  api("POST", "/v1/invites/lookup", { token: $("invitetoken").value });
$("acceptinvite").onclick = async () => {
  await api("POST", "/v1/invites/accept", { token: $("invitetoken").value });
  loadMe();
};
$("neworg").onclick = async () => {
  await api("POST", "/v1/orgs", { name: $("orgname").value });
  loadMe();
};
$("leaveorg").onclick = async () => {
  await api("POST", `${base()}/leave`);
  loadMe();
};
$("refresh").onclick = loadMe;
$("org").onchange = () => {
  logJob = null;
  runView = null;
  $("logs").textContent = "";
  $("runjobs").replaceChildren();
  $("runid").textContent = "";
  loadAll();
};
$("savesettings").onclick = () =>
  act("PATCH", `${base()}/settings`, {
    allow_network: $("allownetwork").checked,
    max_timeout_seconds: Number($("maxtimeout").value),
    max_attempts: Number($("maxattempts").value),
    max_running: Number($("maxrunning").value),
    max_pending: Number($("maxpending").value),
  });
$("enqueue").onclick = () => {
  let command;
  try {
    command = JSON.parse($("command").value);
  } catch {
    $("out").textContent = "command must be a JSON array of strings";
    return;
  }
  act("POST", `${base()}/jobs`, {
    name: $("jobname").value,
    image: $("image").value,
    command,
    queue: $("queue").value,
    timeout_seconds: Number($("timeout").value),
    max_attempts: Number($("attempts").value),
    network: $("network").value,
  });
};
$("createwf").onclick = () => {
  const graph = readGraph();
  if (graph)
    act("POST", `${base()}/workflows`, { name: $("wfname").value, graph });
};
$("createinvite").onclick = () =>
  act("POST", `${base()}/invites`, {
    email: $("inviteemail").value,
    role: $("inviterole").value,
  });
$("admin").onclick = async () => {
  const [stats, workers] = await Promise.all([
    api("GET", "/v1/admin/stats"),
    api("GET", "/v1/admin/workers"),
  ]);
  $("adminout").textContent = stats
    ? JSON.stringify({ stats, workers: workers.workers }, null, 2)
    : "not a platform admin (404)";
};

async function loadAdminCatalogue() {
  const [images, hosts] = await Promise.all([
    api("GET", "/v1/admin/images"),
    api("GET", "/v1/admin/hosts"),
  ]);
  if (!images || !hosts) {
    $("admincat").textContent = "not a platform admin (404)";
    return;
  }
  const entries = (items, path) =>
    items.flatMap((it) => [
      el(
        "span",
        `${it.enabled ? "[on] " : "[off]"} ${path} ${it.value} (${it.label}) `,
      ),
      el("button", it.enabled ? "disable" : "enable", async () => {
        await api("PATCH", `/v1/admin/${path}/${it.id}`, {
          enabled: !it.enabled,
        });
        loadAdminCatalogue();
      }),
      el("br"),
    ]);
  $("admincat").replaceChildren(
    el("p", `server allowlist: ${images.server_image_allowlist.join(", ")}`),
    ...entries(images.images, "images"),
    ...entries(hosts.hosts, "hosts"),
  );
}
$("admincatalogue").onclick = loadAdminCatalogue;
$("addimage").onclick = async () => {
  await api("POST", "/v1/admin/images", {
    ref: $("adminimageref").value,
    label: $("adminimagelabel").value,
  });
  loadAdminCatalogue();
};
$("addhost").onclick = async () => {
  await api("POST", "/v1/admin/hosts", {
    pattern: $("adminhostpattern").value,
    label: $("adminhostlabel").value,
  });
  loadAdminCatalogue();
};

setInterval(() => {
  if ($("auto").checked && org()) loadOrg();
}, 2000);
loadMe();

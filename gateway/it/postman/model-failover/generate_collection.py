#!/usr/bin/env python3
# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------
"""Generates model-failover.postman_collection.json.

Edit this file, not the JSON, then run:  python3 generate_collection.py

The collection runs against the gateway integration-test stack
(gateway/it/docker-compose.test.yaml), which provides the router on :8080,
the controller REST API on :9090 and three scriptable mock LLM backends
(tests/mock-servers/mock-llm-provider) on :8091-:8093.
"""

import json
import os
import textwrap

# --------------------------------------------------------------------------
# Low-level builders
# --------------------------------------------------------------------------

PORTS = {"ctrl": "{{ctrl_port}}", "router": "{{router_port}}", "a": "{{mock_a_port}}",
         "b": "{{mock_b_port}}", "anthropic": "{{mock_anthropic_port}}"}
CTRL_BASE = ["api", "management", "v1"]


def url(target, path):
    """A fully parsed Postman URL (newman rejects raw-only URLs)."""
    segs = [s for s in path.split("/") if s]
    if target == "ctrl":
        segs = CTRL_BASE + segs
    port = PORTS[target]
    return {
        "raw": "http://{{host}}:" + port + "/" + "/".join(segs),
        "protocol": "http",
        "host": ["{{host}}"],
        "port": port,
        "path": segs,
    }


def script(kind, js):
    return {"listen": kind, "script": {"type": "text/javascript", "exec": textwrap.dedent(js).strip("\n").split("\n")}}


def request(name, method, target, path, body=None, headers=None, tests=None, pre=None, body_type="application/json"):
    hdrs = [{"key": k, "value": v} for k, v in (headers or {}).items()]
    if body is not None:
        hdrs.append({"key": "Content-Type", "value": body_type})
    item = {
        "name": name,
        "request": {"method": method, "header": hdrs, "url": url(target, path)},
        "event": [],
    }
    if target == "ctrl":
        item["request"]["auth"] = {"type": "basic", "basic": [
            {"key": "username", "value": "{{admin_user}}", "type": "string"},
            {"key": "password", "value": "{{admin_pass}}", "type": "string"},
        ]}
    else:
        item["request"]["auth"] = {"type": "noauth"}
    if body is not None:
        item["request"]["body"] = {"mode": "raw", "raw": body}
    if pre:
        item["event"].append(script("prerequest", pre))
    if tests:
        item["event"].append(script("test", tests))
    return item


def folder(name, items, description=""):
    # Start every scenario folder from healthy mocks: a previous folder may have
    # left one scripted to fail, and readiness probes reach the primary.
    if not name.startswith(("00 ", "99 ")):
        items = reset_mocks() + items
    return {"name": name, "description": description, "item": items}


def jstr(v):
    return json.dumps(v)


# --------------------------------------------------------------------------
# Fixture: providers and proxies
# --------------------------------------------------------------------------

def pname(suffix):
    return "mfe{{run}}-" + suffix


PROVIDERS = [
    # suffix, upstream, template, auth header, auth value
    ("openai-a", "http://mock-llm-openai-a:8080", "openai", "Authorization", "Bearer " + pname("openai-a") + "-key"),
    ("openai-b", "http://mock-llm-openai-b:8080", "openai", "Authorization", "Bearer " + pname("openai-b") + "-key"),
    ("dead", "http://mock-llm-openai-a:9", "openai", "Authorization", "Bearer " + pname("dead") + "-key"),
    ("anthropic", "http://mock-llm-anthropic:8080", "anthropic", "x-api-key", pname("anthropic") + "-key"),
]


def provider_yaml(suffix, upstream, template, header, value):
    n = pname(suffix)
    return f"""apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: {n}
spec:
  displayName: {n}
  version: v1.0
  template: {template}
  context: /{n}
  upstream:
    url: {upstream}
    auth:
      type: api-key
      header: {header}
      value: {value}
  accessControl:
    mode: allow_all
"""


# PRIMARY maps a proxy or provider name to the model its (first) chain is
# keyed on; call() asks for it, since a request for any other model passes
# through without failover.
PRIMARY = {}
ANTHROPIC_TRANSFORMER = """      transformer:
        type: openai-to-anthropic-transformer
        version: v0
        params:
          model: claude-default
"""


def to_chains(name, params):
    """Converts a flat {"targets": [...]} list into one chain keyed on the first
    target's model: the first target is the primary, the rest its fallbacks.
    Returns the params and the primary's provider suffix (None if unknown)."""
    if "targets" not in params:
        return params, None
    rest = {k: v for k, v in params.items() if k != "targets"}
    ts = params["targets"]
    if not ts:
        return {"chains": [], **rest}, None
    first = ts[0]
    PRIMARY[name] = first["model"]
    suffix = first.get("provider", "").replace(pname(""), "") or None
    return {"chains": [{"primary": {"model": first["model"]}, "fallbacks": ts[1:]}], **rest}, suffix


def proxy_yaml(name, params, global_attach=False, extra_op_policies="", primary="openai-a", before_policies="", raw=False):
    n = pname(name)
    params, first = (params, None) if raw else to_chains(name, params)
    for c in params.get("chains", []):
        PRIMARY.setdefault(name, c["primary"]["model"])
    primary = first or primary
    p = json.dumps(params)
    attach = f"""  operationPolicies:
{before_policies}    - name: model-failover
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params: {p}
{extra_op_policies}"""
    if global_attach:
        attach = f"""  globalPolicies:
    - name: model-failover
      version: v0
      params: {p}
"""
    additional = ""
    for suffix in ["openai-a", "openai-b", "dead", "anthropic"]:
        if suffix == primary:
            continue
        additional += f"    - id: {pname(suffix)}\n"
        if suffix == "anthropic":
            additional += ANTHROPIC_TRANSFORMER
    return f"""apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProxy
metadata:
  name: {n}
spec:
  displayName: {n}
  version: v1.0
  context: /{n}
  provider:
    id: {pname(primary)}
  additionalProviders:
{additional}{attach}"""


def t(provider, model):
    return {"provider": pname(provider), "model": model}


# --------------------------------------------------------------------------
# Steps
# --------------------------------------------------------------------------

CHAT = jstr({"model": "client-model", "messages": [{"role": "user", "content": "hello"}]})
CHAT_STREAM = jstr({"model": "client-model", "stream": True, "messages": [{"role": "user", "content": "hello"}]})
MOCK_NAME = {"a": "mock-llm-openai-a", "b": "mock-llm-openai-b", "anthropic": "mock-llm-anthropic"}


def create_proxy(name, params, **kw):
    return request(f"Create proxy {name}", "POST", "ctrl", "llm-proxies", proxy_yaml(name, params, **kw),
                   body_type="application/yaml",
                   tests="""
                   pm.test("proxy is created (201)", () => pm.response.to.have.status(201));
                   """)


def wait_ready(name, path="chat/completions", body=None):
    """Polls the proxy until it answers 200, up to 60 times, 1s apart. It asks
    for the proxy's primary model, so a failing primary is covered by its chain."""
    key = f"ready_{name}".replace("-", "_")
    body = body or CHAT.replace('"client-model"', jstr(PRIMARY.get(name, "client-model")))
    return request(f"Wait until {name} is ready", "POST", "router", f"{pname(name)}/{path}", body,
                   tests=f"""
                   const n = Number(pm.collectionVariables.get("{key}") || 0);
                   if (pm.response.code !== 200 && n < 60) {{
                     pm.collectionVariables.set("{key}", n + 1);
                     setTimeout(() => {{}}, 1000);
                     postman.setNextRequest(pm.info.requestName);
                   }} else {{
                     pm.collectionVariables.unset("{key}");
                     pm.test("{name} becomes ready", () => pm.response.to.have.status(200));
                   }}
                   """)


def reset_mocks():
    return [request(f"Reset mock {m}", "DELETE", m, "__requests",
                    tests='pm.test("mock reset", () => pm.response.to.have.status(204));')
            for m in ("a", "b", "anthropic")]


def mode(mock, m):
    return request(f"Set mock {mock} to {m}", "PUT", mock, "__mode", jstr({"mode": m}),
                   tests='pm.test("mode set", () => pm.response.to.have.status(204));')


def count(mock, n):
    return request(f"Mock {mock} received {n}", "GET", mock, "__requests",
                   tests=f"""
                   pm.test("{MOCK_NAME[mock]} received {n} request(s)", () => pm.expect(pm.response.json().count).to.eql({n}));
                   """)


def last(mock, checks, label):
    return request(f"Mock {mock} last request: {label}", "GET", mock, "__requests",
                   tests="const last = pm.response.json().last;\npm.test('a request was recorded', () => pm.expect(last).to.be.an('object'));\n" + checks)


def call(name, proxy, tests, body=CHAT, headers=None, path="chat/completions"):
    """Sends a request asking for the proxy's primary model (see PRIMARY)."""
    if proxy in PRIMARY and body is not None:
        body = body.replace('"client-model"', jstr(PRIMARY[proxy]))
    return request(name, "POST", "router", f"{pname(proxy)}/{path}", body, headers=headers, tests=tests)


def sleep(seconds):
    return request(f"Wait {seconds}s", "GET", "a", "health",
                   pre=f"setTimeout(() => {{}}, {seconds * 1000});",
                   tests='pm.test("waited", () => pm.response.to.have.status(200));')


NO_INTERNAL_HEADERS = """
pm.test("no internal failover headers reach the client", () => {
  ["x-wso2-attempt-retry", "x-wso2-attempt-exhausted", "x-wso2-upstream-failure",
   "x-wso2-attempt-plan", "x-wso2-attempt-chain", "x-wso2-attempt-hop"]
    .forEach(h => pm.expect(pm.response.headers.has(h), h).to.be.false);
});
"""


def served_by(mock_name, status=200):
    return f"""
pm.test("status is {status}", () => pm.response.to.have.status({status}));
pm.test("served by {mock_name}", () => pm.expect(pm.response.text()).to.include("hello from {mock_name}"));
""" + NO_INTERNAL_HEADERS


def status_is(code):
    return f'pm.test("status is {code}", () => pm.response.to.have.status({code}));\n' + NO_INTERNAL_HEADERS


EXHAUSTION_BODY = '{"error":{"message":"All configured model targets are currently unavailable. Please retry later.","type":"model_failover_exhausted","param":null,"code":"all_targets_unavailable"}}'
EXHAUSTED = f"""
pm.test("status is 503", () => pm.response.to.have.status(503));
pm.test("body is the fixed exhaustion error", () => pm.expect(pm.response.text()).to.eql({jstr(EXHAUSTION_BODY)}));
""" + NO_INTERNAL_HEADERS

HIDDEN_HEADERS = """
pm.test("internal headers never reach the provider", () => {
  ["x-wso2-attempt-plan", "x-wso2-attempt-hop", "x-wso2-attempt-chain", "x-wso2-attempt-retry", "x-target-upstream"]
    .forEach(h => pm.expect(last.headers[h], h).to.be.undefined);
});
"""


# --------------------------------------------------------------------------
# Folders
# --------------------------------------------------------------------------

def setup():
    items = [request("Start run", "GET", "a", "health",
                     pre="""
                     const run = Math.random().toString(36).replace(/[^a-z]/g, "").slice(0, 6) || "run";
                     pm.collectionVariables.set("run", run);
                     console.log("model-failover e2e run id:", run);
                     """,
                     tests='pm.test("mock openai-a is up", () => pm.response.to.have.status(200));'),
             request("Mock openai-b is up", "GET", "b", "health", tests='pm.test("up", () => pm.response.to.have.status(200));'),
             request("Mock anthropic is up", "GET", "anthropic", "health", tests='pm.test("up", () => pm.response.to.have.status(200));')]
    for p in PROVIDERS:
        items.append(request(f"Create provider {p[0]}", "POST", "ctrl", "llm-providers", provider_yaml(*p),
                             body_type="application/yaml",
                             tests='pm.test("provider is created (201)", () => pm.response.to.have.status(201));'))
    return folder("00 Setup", items, "Creates the four LlmProviders every scenario uses.")


def basic():
    name = "basic"
    # A high threshold keeps the primary out of suspension across this matrix;
    # suspension has its own folder.
    params = {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")], "perAttemptTimeout": "2s",
              "suspendAfterConsecutiveFailures": 100}
    items = [create_proxy(name, params), wait_ready(name), *reset_mocks(),
             call("Healthy primary serves", name, served_by("mock-llm-openai-a")),
             count("a", 1), count("b", 0),
             last("a", 'pm.test("the requested primary model gpt-a is sent", () => pm.expect(JSON.parse(last.body).model).to.eql("gpt-a"));\n'
                  'pm.test("primary credential", () => pm.expect(last.headers.authorization).to.eql("Bearer " + "mfe" + pm.collectionVariables.get("run") + "-openai-a-key"));\n'
                  'pm.test("other body fields are kept", () => pm.expect(JSON.parse(last.body).messages[0].content).to.eql("hello"));\n'
                  + HIDDEN_HEADERS, "model, credential, hidden headers")]
    for code in (429, 500, 502, 503, 504):
        items += [*reset_mocks(), mode("a", f"status:{code}"),
                  call(f"{code} on primary fails over", name, served_by("mock-llm-openai-b")),
                  count("a", 1), count("b", 1)]
    items += [last("b", 'pm.test("fallback uses its own credential", () => pm.expect(last.headers.authorization).to.eql("Bearer mfe" + pm.collectionVariables.get("run") + "-openai-b-key"));\n'
                   'pm.test("fallback model is gpt-b", () => pm.expect(JSON.parse(last.body).model).to.eql("gpt-b"));\n' + HIDDEN_HEADERS,
                   "own credential and model")]
    for code in (400, 401, 403, 404, 422):
        items += [*reset_mocks(), mode("a", f"status:{code}"),
                  call(f"{code} is returned without failover", name, status_is(code)),
                  count("a", 1), count("b", 0)]
    items += [*reset_mocks(), mode("a", "reset"),
              call("Connection reset on primary fails over", name, served_by("mock-llm-openai-b")),
              count("b", 1),
              *reset_mocks(), mode("a", "hang:10"),
              call("Hanging primary is abandoned after 2s", name, served_by("mock-llm-openai-b") + """
pm.test("took at least the per-attempt timeout", () => pm.expect(pm.response.responseTime).to.be.at.least(1900));
pm.test("did not wait for the hanging primary", () => pm.expect(pm.response.responseTime).to.be.below(5000));
"""),
              *reset_mocks(), mode("a", "status:429"),
              call("Streaming request fails over before the stream starts", name, served_by("mock-llm-openai-b") + """
pm.test("an OpenAI stream is returned", () => {
  pm.expect(pm.response.text()).to.include("chat.completion.chunk");
  pm.expect(pm.response.text()).to.include("[DONE]");
});
""", body=CHAT_STREAM),
              *reset_mocks(),
              call("Client-supplied internal headers are ignored", name, served_by("mock-llm-openai-a"),
                   headers={"x-wso2-attempt-plan": "00112233445566778899aabbccddeeff",
                            "x-wso2-attempt-chain": "forged", "x-wso2-attempt-hop": "guess",
                            "x-wso2-attempt-retry": "status_429", "x-wso2-upstream-failure": "UF"}),
              last("a", HIDDEN_HEADERS + 'pm.test("no forged failure header", () => pm.expect(last.headers["x-wso2-upstream-failure"]).to.be.undefined);', "forged headers stripped"),
              *reset_mocks(), mode("a", "status:503"),
              call("Operations without model-failover are not failed over", name, status_is(503), path="embeddings",
                   body=jstr({"model": "e", "input": "x"})),
              count("b", 0),
              *reset_mocks(), mode("a", "status:503"),
              call("A 200 KB body is resent intact to the fallback", name, served_by("mock-llm-openai-b"),
                   body=jstr({"model": "client-model", "messages": [{"role": "user", "content": "x" * 200000 + "END-MARKER"}]})),
              last("b", 'pm.test("the replayed body is complete", () => pm.expect(last.body).to.include("END-MARKER"));', "complete replay")]
    return folder("01 Ordered failover", items,
                  "Two OpenAI targets, perAttemptTimeout 2s: every eligible status, non-eligible statuses, reset, timeout, streaming, header hygiene, operations without the policy, body replay.")


def allow_list():
    name = "allow"
    params = {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")], "failoverOn": {"statusCodes": [429, 529]},
              "suspendAfterConsecutiveFailures": 100}
    return folder("02 Status allow-list", [
        create_proxy(name, params), wait_ready(name),
        *reset_mocks(), mode("a", "status:503"),
        call("503 is passed through when not listed", name, status_is(503)), count("b", 0),
        *reset_mocks(), mode("a", "status:529"),
        call("A custom 529 fails over when listed", name, served_by("mock-llm-openai-b")),
        *reset_mocks(), mode("a", "status:429"),
        call("429 fails over", name, served_by("mock-llm-openai-b")),
    ], "failoverOn.statusCodes [429, 529]: unlisted server errors reach the client unchanged.")


def no_timeout():
    name = "notimeout"
    params = {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")], "perAttemptTimeout": "2s",
              "failoverOn": {"timeout": False}}
    return folder("03 Timeout not a failover condition", [
        create_proxy(name, params), wait_ready(name),
        *reset_mocks(), mode("a", "hang:10"),
        call("A timeout returns 504 without failover", name, status_is(504) + """
pm.test("gave up after the per-attempt timeout", () => pm.expect(pm.response.responseTime).to.be.below(5000));
"""),
        count("b", 0),
    ], "failoverOn.timeout false: a slow target is not failed over.")


def connection():
    return folder("04 Connection failures", [
        create_proxy("conn", {"targets": [t("dead", "gpt-dead"), t("openai-a", "gpt-a")]}), wait_ready("conn"),
        *reset_mocks(),
        call("Connection refused on the primary fails over", "conn", served_by("mock-llm-openai-a")),
        count("a", 1),
        create_proxy("noconn", {"targets": [t("dead", "gpt-dead"), t("openai-a", "gpt-a")], "failoverOn": {"connectFailure": False}}),
        request("Wait until noconn is routable", "POST", "router", f"{pname('noconn')}/chat/completions", CHAT,
                tests="""
                const n = Number(pm.collectionVariables.get("ready_noconn") || 0);
                if (pm.response.code === 404 && n < 60) {
                  pm.collectionVariables.set("ready_noconn", n + 1);
                  setTimeout(() => {}, 1000);
                  postman.setNextRequest(pm.info.requestName);
                } else { pm.collectionVariables.unset("ready_noconn"); }
                """),
        *reset_mocks(),
        call("With connectFailure false the failure reaches the client", "noconn", status_is(503)),
        count("a", 0),
    ], "A target on a closed port, with and without connectFailure.")


def cross_provider():
    name = "cross"
    params = {"targets": [t("openai-a", "gpt-a"), t("anthropic", "claude-sonnet-4-5")]}
    anth_checks = """
pm.test("Anthropic Messages path", () => pm.expect(last.path).to.include("/v1/messages"));
pm.test("Anthropic credential only", () => {
  pm.expect(last.headers["x-api-key"]).to.eql("mfe" + pm.collectionVariables.get("run") + "-anthropic-key");
  pm.expect(last.headers.authorization).to.be.undefined;
});
pm.test("anthropic-version header is set", () => pm.expect(last.headers["anthropic-version"]).to.be.a("string"));
pm.test("the target's model is requested", () => pm.expect(JSON.parse(last.body).model).to.eql("claude-sonnet-4-5"));
""" + HIDDEN_HEADERS
    return folder("05 Cross-provider failover", [
        create_proxy(name, params), wait_ready(name),
        *reset_mocks(), mode("a", "status:503"),
        call("Anthropic fallback answers in OpenAI format", name, served_by("mock-llm-anthropic") + """
pm.test("OpenAI ChatCompletion shape", () => {
  const b = pm.response.json();
  pm.expect(b.object).to.eql("chat.completion");
  pm.expect(b.choices[0].message.content).to.include("hello from mock-llm-anthropic");
});
"""),
        last("anthropic", anth_checks, "converted request"),
        *reset_mocks(), mode("a", "status:429"),
        call("Streamed Anthropic fallback reaches the client as an OpenAI stream", name, served_by("mock-llm-anthropic") + """
pm.test("OpenAI stream chunks and terminator", () => {
  pm.expect(pm.response.text()).to.include("chat.completion.chunk");
  pm.expect(pm.response.text()).to.include("[DONE]");
});
""", body=CHAT_STREAM),
        count("anthropic", 1),
    ], "An OpenAI primary with an Anthropic fallback, converted both ways, streaming included.")


def same_provider_two_models():
    name = "twomodels"
    params = {"targets": [t("openai-a", "gpt-a"), t("anthropic", "claude-opus"), t("anthropic", "claude-sonnet")]}
    return folder("06 Same provider, two models", [
        create_proxy(name, params), wait_ready(name),
        *reset_mocks(), mode("a", "status:503"), mode("anthropic", "seq:status:529,ok"),
        call("529 is not in the default list and reaches the client", name, status_is(529)),
        count("anthropic", 1),
        *reset_mocks(), mode("a", "status:503"), mode("anthropic", "seq:status:503,ok"),
        call("503 on the first model fails over to the second", name, served_by("mock-llm-anthropic")),
        count("anthropic", 2),
        last("anthropic", 'pm.test("second attempt asked for claude-sonnet", () => pm.expect(JSON.parse(last.body).model).to.eql("claude-sonnet"));', "second model"),
    ], "One provider listed twice with different models.")


def exhaustion():
    name = "exh"
    params = {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b"), t("anthropic", "claude-sonnet-4-5")]}
    return folder("07 Exhaustion", [
        create_proxy(name, params), wait_ready(name),
        *reset_mocks(), mode("a", "status:503"), mode("b", "status:429"), mode("anthropic", "status:500"),
        call("Every target fails: fixed error", name, EXHAUSTED),
        count("a", 1), count("b", 1), count("anthropic", 1),
        *reset_mocks(), mode("a", "reset"), mode("b", "status:502"), mode("anthropic", "status:503"),
        call("Mixed transport and status failures: same fixed error", name, EXHAUSTED),
    ], "Every target failing, each tried exactly once.")


def suspension():
    items = [
        create_proxy("susp", {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")],
                              "suspendAfterConsecutiveFailures": 2, "suspendDuration": "5s", "recoverAfterSuccessfulProbes": 1}),
        wait_ready("susp"),
        *reset_mocks(), mode("a", "status:503"),
        call("Failure 1 of 2", "susp", served_by("mock-llm-openai-b")),
        call("Failure 2 of 2 suspends the primary", "susp", served_by("mock-llm-openai-b")),
        count("a", 2),
        *reset_mocks(),
        call("A suspended primary is skipped even though it is healthy again", "susp", served_by("mock-llm-openai-b")),
        count("a", 0),
        sleep(6),
        call("After the suspension a probe reaches the primary and succeeds", "susp", served_by("mock-llm-openai-a")),
        call("The recovered primary takes normal traffic", "susp", served_by("mock-llm-openai-a")),
        count("a", 2), count("b", 1),
        create_proxy("reprobe", {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")],
                                 "suspendAfterConsecutiveFailures": 2, "suspendDuration": "5s"}),
        wait_ready("reprobe"),
        *reset_mocks(), mode("a", "status:503"),
        call("Reprobe failure 1", "reprobe", served_by("mock-llm-openai-b")),
        call("Reprobe failure 2 suspends", "reprobe", served_by("mock-llm-openai-b")),
        sleep(6),
        call("The probe fails and the fallback serves", "reprobe", served_by("mock-llm-openai-b")),
        count("a", 3),
        call("The primary is suspended again", "reprobe", served_by("mock-llm-openai-b")),
        count("a", 3),
        create_proxy("allsusp", {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")],
                                 "suspendAfterConsecutiveFailures": 1, "suspendDuration": "60s"}),
        wait_ready("allsusp"),
        *reset_mocks(), mode("a", "status:503"), mode("b", "status:503"),
        call("Both targets fail and are suspended", "allsusp", EXHAUSTED),
        *reset_mocks(),
        call("With every target suspended the gateway answers at once", "allsusp", EXHAUSTED),
        count("a", 0), count("b", 0),
    ]
    return folder("08 Suspension and recovery", items,
                  "Consecutive-failure suspension, skipping, probe recovery, a failed probe, and every target suspended.")


def global_and_update():
    params = {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")]}
    swapped = {"targets": [t("openai-b", "gpt-b"), t("openai-a", "gpt-a")]}
    return folder("09 Global attachment and updates", [
        create_proxy("global", params, global_attach=True), wait_ready("global"),
        *reset_mocks(), mode("a", "status:429"),
        call("A globally attached chain fails over", "global", served_by("mock-llm-openai-b")),
        request("Swap the target order", "PUT", "ctrl", f"llm-proxies/{pname('global')}",
                proxy_yaml("global", swapped, global_attach=True), body_type="application/yaml",
                tests='pm.test("proxy is updated (200)", () => pm.response.to.have.status(200));'),
        sleep(3),
        *reset_mocks(),
        call("After the update the new primary serves first", "global", served_by("mock-llm-openai-b")),
        count("a", 0),
    ], "model-failover in globalPolicies, then an update that reorders the chain.")


def validation():
    ok_chain = lambda **kw: {"chains": [{"primary": {"model": "m"}, "fallbacks": [t("openai-b", "m")]}], **kw}
    bad = [
        ("empty-chains", {"chains": []}),
        ("old-targets", {"targets": [t("openai-a", "m"), t("openai-b", "m")]}),
        ("no-fallbacks", {"chains": [{"primary": {"model": "m"}, "fallbacks": []}]}),
        ("primary-provider", {"chains": [{"primary": t("openai-a", "m"), "fallbacks": [t("openai-b", "m")]}]}),
        ("duplicate-primary", {"chains": ok_chain()["chains"] * 2}),
        ("unknown-provider", {"chains": [{"primary": {"model": "m"}, "fallbacks": [{"provider": "not-attached", "model": "m"}]}]}),
        ("duplicate-target", {"chains": [{"primary": {"model": "m"}, "fallbacks": [t("openai-a", "m")]}]}),
        ("too-many", {"chains": [{"primary": {"model": "m"}, "fallbacks": [t("openai-b", f"m{i}") for i in range(10)]}]}),
        ("status-404", ok_chain(failoverOn={"statusCodes": [404]})),
        ("status-dup", ok_chain(failoverOn={"statusCodes": [503, 503]})),
        ("timeout-zero", ok_chain(perAttemptTimeout="0s")),
        ("timeout-big", ok_chain(perAttemptTimeout="301s")),
        ("threshold-zero", ok_chain(suspendAfterConsecutiveFailures=0)),
        ("suspend-long", ok_chain(suspendDuration="2h")),
        ("probes-high", ok_chain(probeConcurrency=11)),
        ("recover-zero", ok_chain(recoverAfterSuccessfulProbes=0)),
        ("internal-key", ok_chain(_role="dispatch")),
    ]
    items = []
    for name, params in bad:
        items.append(request(f"Rejected: {name}", "POST", "ctrl", "llm-proxies", proxy_yaml("bad-" + name, params, raw=True),
                             body_type="application/yaml",
                             tests='pm.test("invalid configuration is rejected (400)", () => pm.response.to.have.status(400));'))
    router = """    - name: llm-header-router
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params: {}
"""
    items.append(request("Rejected: combined with llm-header-router", "POST", "ctrl", "llm-proxies",
                         proxy_yaml("bad-router", ok_chain(), extra_op_policies=router),
                         body_type="application/yaml",
                         tests='pm.test("provider-selecting policy is rejected (400)", () => pm.response.to.have.status(400));'))
    return folder("10 Configuration validation", items, "Every invalid configuration is rejected at registration with 400.")


def provider_yaml_with_failover(name, params, template="openai", path="/chat/completions"):
    n = pname(name)
    params, _ = to_chains(name, params)
    return f"""apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: {n}
spec:
  displayName: {n}
  version: v1.0
  template: {template}
  context: /{n}
  upstream:
    url: http://mock-llm-openai-a:8080
    auth:
      type: api-key
      header: Authorization
      value: Bearer {n}-key
  accessControl:
    mode: allow_all
  operationPolicies:
    - name: model-failover
      version: v0
      paths:
        - path: {path}
          methods: [POST]
          params: {json.dumps(params)}
"""


GEMINI_PATH_CHECKS = """
pm.test("attempt asked for gemini-2.5-flash in the path", () => pm.expect(last.path).to.include("/models/gemini-2.5-flash:generateContent"));
pm.test("query kept", () => pm.expect(last.path).to.include("alt=sse"));
"""
GEMINI_BODY = json.dumps({"contents": [{"role": "user", "parts": [{"text": "hi"}]}]})


def provider_mode():
    name = "pmodel"
    # A high threshold keeps gpt-a1 in rotation for the failover cases; the
    # suspension cases use their own provider (psusp) below.
    params = {"targets": [{"model": "gpt-a1"}, {"model": "gpt-a2"}], "perAttemptTimeout": "2s",
              "suspendAfterConsecutiveFailures": 100}
    susp_params = {"targets": [{"model": "gpt-a1"}, {"model": "gpt-a2"}],
                   "suspendAfterConsecutiveFailures": 2, "suspendDuration": "30s"}
    model_is = lambda m: f'pm.test("attempt asked for {m}", () => pm.expect(JSON.parse(last.body).model).to.eql("{m}"));\n'
    own_key = 'pm.test("the provider\'s own credential", () => pm.expect(last.headers.authorization).to.eql("Bearer mfe" + pm.collectionVariables.get("run") + "-pmodel-key"));\n'
    nested_targets = {"targets": [t("pmodel", "gpt-a1"), t("openai-b", "gpt-b")]}
    nested_proxy = proxy_yaml("nested", nested_targets)
    items = [
        request("Create provider with model failover", "POST", "ctrl", "llm-providers", provider_yaml_with_failover(name, params),
                body_type="application/yaml", tests='pm.test("provider is created (201)", () => pm.response.to.have.status(201));'),
        wait_ready_provider(name),
        *reset_mocks(),
        call("First model serves", name, served_by("mock-llm-openai-a")),
        count("a", 1), last("a", model_is("gpt-a1") + own_key + HIDDEN_HEADERS, "first model, own credential"),
        *reset_mocks(), mode("a", "seq:status:429,ok"),
        call("429 falls back to the second model", name, served_by("mock-llm-openai-a")),
        count("a", 2), last("a", model_is("gpt-a2") + own_key + HIDDEN_HEADERS, "second model, no internal headers"),
        *reset_mocks(), mode("a", "seq:reset,ok"),
        call("A connection reset falls back to the second model", name, served_by("mock-llm-openai-a")),
        last("a", model_is("gpt-a2"), "second model after reset"),
        *reset_mocks(), mode("a", "seq:hang:10,ok"),
        call("A hanging first model is abandoned after 2s", name, served_by("mock-llm-openai-a") + """
pm.test("took at least the per-attempt timeout", () => pm.expect(pm.response.responseTime).to.be.at.least(1900));
pm.test("did not wait for the hanging attempt", () => pm.expect(pm.response.responseTime).to.be.below(5000));
"""),
        *reset_mocks(), mode("a", "status:400"),
        call("A 400 is returned without trying the second model", name, status_is(400)),
        count("a", 1),
        *reset_mocks(), mode("a", "status:503"),
        call("Every model fails: fixed error", name, EXHAUSTED),
        count("a", 2),
        *reset_mocks(),
        request("Create provider psusp (suspension after 2 failures)", "POST", "ctrl", "llm-providers",
                provider_yaml_with_failover("psusp", susp_params),
                body_type="application/yaml", tests='pm.test("provider is created (201)", () => pm.response.to.have.status(201));'),
        wait_ready("psusp"),
        *reset_mocks(), mode("a", "seq:status:503,ok,status:503,ok,ok"),
        call("Model failure 1 of 2", "psusp", served_by("mock-llm-openai-a")),
        call("Model failure 2 of 2 suspends gpt-a1", "psusp", served_by("mock-llm-openai-a")),
        call("The suspended model is skipped", "psusp", served_by("mock-llm-openai-a")),
        count("a", 5), last("a", model_is("gpt-a2"), "suspended model skipped"),
        create_proxy_raw("nested", nested_proxy), wait_ready("nested"),
        *reset_mocks(), mode("a", "seq:status:429,ok"),
        call("A proxy over a provider with its own failover: the provider's second model serves", "nested", served_by("mock-llm-openai-a")),
        count("a", 2), count("b", 0),
        request("Create Gemini-template provider with model failover on /models/*", "POST", "ctrl", "llm-providers",
                provider_yaml_with_failover("pgemini", {"targets": [{"model": "gemini-2.5-pro"}, {"model": "gemini-2.5-flash"}]},
                                            template="gemini", path="/models/*"),
                body_type="application/yaml", tests='pm.test("provider is created (201)", () => pm.response.to.have.status(201));'),
        wait_ready("pgemini", path="models/client:generateContent", body=GEMINI_BODY),
        *reset_mocks(), mode("a", "seq:status:429,ok"),
        call("Gemini path model: 429 falls back to the second model", "pgemini", served_by("mock-llm-openai-a"),
             body=GEMINI_BODY, path="models/gemini-2.5-pro:generateContent?alt=sse"),
        count("a", 2), last("a", GEMINI_PATH_CHECKS + HIDDEN_HEADERS, "second model in the path"),
        request("Rejected: path that fixes the model", "POST", "ctrl", "llm-providers",
                provider_yaml_with_failover("bad-gemini", {"targets": [{"model": "g1"}, {"model": "g2"}]}, template="gemini",
                                            path="/models/gemini-2.5-pro:generateContent"),
                body_type="application/yaml",
                tests='pm.test("rejected (400)", () => pm.response.to.have.status(400));\npm.test("asks for a wildcard path", () => pm.expect(pm.response.text()).to.include("use a wildcard path"));'),
        request("Rejected: target naming another provider", "POST", "ctrl", "llm-providers",
                provider_yaml_with_failover("bad-cross", {"targets": [{"model": "m1"}, {"provider": "someone-else", "model": "m2"}]}),
                body_type="application/yaml",
                tests='pm.test("rejected (400)", () => pm.response.to.have.status(400));\npm.test("points to an LlmProxy", () => pm.expect(pm.response.text()).to.include("LlmProxy"));'),
    ]
    return folder("11 Provider-mode model failover", items,
                  "model-failover on an LlmProvider: fallback across its own models, reset, timeout, pass-through, exhaustion, suspension, nesting under a proxy, and the registration rules.")


def keyed_chains():
    chains = {"chains": [
        {"primary": {"model": "gpt-4o"}, "fallbacks": [t("openai-b", "gpt-4o"), t("anthropic", "claude-sonnet-4-5")]},
        {"primary": {"model": "gpt-4.1"}, "fallbacks": [{"model": "gpt-4.1-mini"}]},
    ]}
    ask = lambda m: jstr({"model": m, "messages": [{"role": "user", "content": "hello"}]})
    model_is = lambda m: f'pm.test("asked for {m}", () => pm.expect(JSON.parse(last.body).model).to.eql("{m}"));\n'
    rr = """    - name: model-round-robin
      version: v1
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            suspendDuration: 0
            models:
              - model: gpt-4.1
"""
    items = [
        create_proxy("keyed", chains), wait_ready("keyed"),
        *reset_mocks(), mode("a", "seq:status:429,ok"),
        request("gpt-4.1 falls back along its own chain", "POST", "router", f"{pname('keyed')}/chat/completions", ask("gpt-4.1"),
                tests=served_by("mock-llm-openai-a")),
        count("a", 2), count("b", 0), count("anthropic", 0),
        last("a", model_is("gpt-4.1-mini"), "gpt-4.1's fallback"),
        *reset_mocks(), mode("a", "status:429"),
        request("gpt-4o falls back along its own chain", "POST", "router", f"{pname('keyed')}/chat/completions", ask("gpt-4o"),
                tests=served_by("mock-llm-openai-b")),
        last("b", model_is("gpt-4o"), "gpt-4o's fallback"),
        *reset_mocks(), mode("a", "status:503"),
        request("A model with no chain passes through with one attempt", "POST", "router", f"{pname('keyed')}/chat/completions", ask("gpt-4o-mini"),
                tests=status_is(503) + 'pm.test("not the exhaustion body", () => pm.expect(pm.response.text()).to.not.include("all_targets_unavailable"));\n'),
        count("a", 1), count("b", 0),
        last("a", model_is("gpt-4o-mini") + HIDDEN_HEADERS, "unchanged model, no internal headers"),
        *reset_mocks(), mode("a", "status:503"), mode("b", "status:503"), mode("anthropic", "status:503"),
        request("Every model of gpt-4o's chain fails: fixed error", "POST", "router", f"{pname('keyed')}/chat/completions", ask("gpt-4o"),
                tests=EXHAUSTED),
        count("a", 1), count("b", 1), count("anthropic", 1),
        *reset_mocks(),
        request("Create proxy with round-robin before model-failover", "POST", "ctrl", "llm-proxies",
                proxy_yaml("rrfirst", dict(chains), before_policies=rr), body_type="application/yaml",
                tests='pm.test("proxy is created (201)", () => pm.response.to.have.status(201));'),
        wait_ready("rrfirst"),
        *reset_mocks(), mode("a", "seq:status:429,ok"),
        request("Round-robin picks gpt-4.1 and its chain serves", "POST", "router", f"{pname('rrfirst')}/chat/completions", ask("gpt-4o"),
                tests=served_by("mock-llm-openai-a")),
        count("a", 2), count("b", 0),
        last("a", model_is("gpt-4.1-mini"), "round-robin's pick fell back along its own chain"),
        request("Rejected: round-robin after model-failover", "POST", "ctrl", "llm-proxies",
                proxy_yaml("bad-rrafter", dict(chains), extra_op_policies=rr), body_type="application/yaml",
                tests='pm.test("rejected (400)", () => pm.response.to.have.status(400));\npm.test("names the order rule", () => pm.expect(pm.response.text()).to.include("must come before"));'),
    ]
    return folder("12 Chains keyed by the requested model", items,
                  "Two chains on one proxy: each model walks its own chain, an unconfigured model passes through, round-robin before failover picks the chain, and round-robin after it is rejected.")


def wait_ready_provider(name):
    return wait_ready(name)


def create_proxy_raw(name, yaml_text):
    return request(f"Create proxy {name}", "POST", "ctrl", "llm-proxies", yaml_text, body_type="application/yaml",
                   tests='pm.test("proxy is created (201)", () => pm.response.to.have.status(201));')


def cleanup():
    proxies = ["basic", "allow", "notimeout", "conn", "noconn", "cross", "twomodels", "exh",
               "susp", "reprobe", "allsusp", "global", "nested", "keyed", "rrfirst"]
    bad = ["empty-chains", "old-targets", "no-fallbacks", "primary-provider", "duplicate-primary",
           "unknown-provider", "duplicate-target", "too-many", "status-404", "status-dup",
           "timeout-zero", "timeout-big", "threshold-zero", "suspend-long", "probes-high", "recover-zero",
           "internal-key", "router", "rrafter"]
    items = []
    for p in proxies:
        items.append(request(f"Delete proxy {p}", "DELETE", "ctrl", f"llm-proxies/{pname(p)}",
                             tests='pm.test("deleted", () => pm.expect(pm.response.code).to.be.oneOf([200, 204]));'))
    for b in bad:
        items.append(request(f"Delete leftover bad-{b}", "DELETE", "ctrl", f"llm-proxies/{pname('bad-' + b)}",
                             tests='pm.test("not left behind", () => pm.expect(pm.response.code).to.be.oneOf([200, 204, 404]));'))
    for p in ["pmodel", "psusp", "pgemini", "bad-gemini", "bad-cross"]:
        items.append(request(f"Delete provider {p}", "DELETE", "ctrl", f"llm-providers/{pname(p)}",
                             tests='pm.test("not left behind", () => pm.expect(pm.response.code).to.be.oneOf([200, 204, 404]));'))
    for p in PROVIDERS:
        items.append(request(f"Delete provider {p[0]}", "DELETE", "ctrl", f"llm-providers/{pname(p[0])}",
                             tests='pm.test("deleted", () => pm.expect(pm.response.code).to.be.oneOf([200, 204]));'))
    items += reset_mocks()
    return folder("99 Cleanup", items, "Deletes everything this run created.")


def build():
    return {
        "info": {
            "name": "Model Failover E2E",
            "description": "End-to-end tests for the model-failover policy against the gateway IT stack. "
                           "Generated by generate_collection.py; run with run-e2e.sh.",
            "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
        },
        "variable": [{"key": "run", "value": ""}],
        "item": [setup(), basic(), allow_list(), no_timeout(), connection(), cross_provider(),
                 same_provider_two_models(), exhaustion(), suspension(), global_and_update(), validation(), provider_mode(), keyed_chains(), cleanup()],
    }


def environment():
    values = {"host": "localhost", "ctrl_port": "9090", "router_port": "8080", "mock_a_port": "8091",
              "mock_b_port": "8092", "mock_anthropic_port": "8093", "admin_user": "admin", "admin_pass": "admin"}
    return {"name": "model-failover-it", "values": [{"key": k, "value": v, "enabled": True} for k, v in values.items()]}


if __name__ == "__main__":
    here = os.path.dirname(os.path.abspath(__file__))
    with open(os.path.join(here, "model-failover.postman_collection.json"), "w") as f:
        json.dump(build(), f, indent=2)
        f.write("\n")
    with open(os.path.join(here, "model-failover.postman_environment.json"), "w") as f:
        json.dump(environment(), f, indent=2)
        f.write("\n")
    print("wrote model-failover.postman_collection.json and model-failover.postman_environment.json")

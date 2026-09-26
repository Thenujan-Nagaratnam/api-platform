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


def proxy_yaml(name, params, global_attach=False, extra_op_policies=""):
    n = pname(name)
    p = json.dumps(params)
    attach = f"""  operationPolicies:
    - name: model-failover
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
    return f"""apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProxy
metadata:
  name: {n}
spec:
  displayName: {n}
  version: v1.0
  context: /{n}
  provider:
    id: {pname('openai-a')}
  additionalProviders:
    - id: {pname('openai-b')}
    - id: {pname('dead')}
    - id: {pname('anthropic')}
      transformer:
        type: openai-to-anthropic-transformer
        version: v0
        params:
          model: claude-default
{attach}"""


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


def wait_ready(name):
    """Polls the proxy until it answers 200, up to 60 times, 1s apart."""
    key = f"ready_{name}".replace("-", "_")
    return request(f"Wait until {name} is ready", "POST", "router", f"{pname(name)}/chat/completions", CHAT,
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
    return request(name, "POST", "router", f"{pname(proxy)}/{path}", body, headers=headers, tests=tests)


def sleep(seconds):
    return request(f"Wait {seconds}s", "GET", "a", "health",
                   pre=f"setTimeout(() => {{}}, {seconds * 1000});",
                   tests='pm.test("waited", () => pm.response.to.have.status(200));')


NO_INTERNAL_HEADERS = """
pm.test("no internal failover headers reach the client", () => {
  ["x-wso2-failover-retry", "x-wso2-failover-exhausted", "x-wso2-upstream-failure",
   "x-wso2-failover-plan", "x-wso2-failover-chain", "x-wso2-failover-hop"]
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
  ["x-wso2-failover-plan", "x-wso2-failover-hop", "x-wso2-failover-chain", "x-wso2-failover-retry", "x-target-upstream"]
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
    params = {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")], "perAttemptTimeout": "2s"}
    items = [create_proxy(name, params), wait_ready(name), *reset_mocks(),
             call("Healthy primary serves", name, served_by("mock-llm-openai-a")),
             count("a", 1), count("b", 0),
             last("a", 'pm.test("model rewritten to gpt-a", () => pm.expect(JSON.parse(last.body).model).to.eql("gpt-a"));\n'
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
                   headers={"x-wso2-failover-plan": "00112233445566778899aabbccddeeff",
                            "x-wso2-failover-chain": "forged", "x-wso2-failover-hop": "guess",
                            "x-wso2-failover-retry": "status_429", "x-wso2-upstream-failure": "UF"}),
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
    params = {"targets": [t("openai-a", "gpt-a"), t("openai-b", "gpt-b")], "failoverOn": {"statusCodes": [429, 529]}}
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
    params = {"targets": [t("anthropic", "claude-opus"), t("anthropic", "claude-sonnet")]}
    return folder("06 Same provider, two models", [
        create_proxy(name, params), wait_ready(name),
        *reset_mocks(), mode("anthropic", "seq:status:529,ok"),
        call("529 is not in the default list and reaches the client", name, status_is(529)),
        count("anthropic", 1),
        *reset_mocks(), mode("anthropic", "seq:status:503,ok"),
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
        create_proxy("single", {"targets": [t("openai-a", "gpt-a")]}), wait_ready("single"),
        *reset_mocks(), mode("a", "status:429"),
        call("A single-target chain that fails gets the fixed error", "single", EXHAUSTED),
        count("a", 1),
        *reset_mocks(),
        call("A single-target chain that succeeds is untouched", "single", served_by("mock-llm-openai-a")),
    ], "Every target failing, each tried exactly once, and single-target chains.")


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
    bad = [
        ("empty-targets", {"targets": []}),
        ("unknown-provider", {"targets": [{"provider": "not-attached", "model": "m"}]}),
        ("duplicate-target", {"targets": [t("openai-a", "m"), t("openai-a", "m")]}),
        ("too-many", {"targets": [t("openai-a", f"m{i}") for i in range(11)]}),
        ("status-404", {"targets": [t("openai-a", "m")], "failoverOn": {"statusCodes": [404]}}),
        ("status-dup", {"targets": [t("openai-a", "m")], "failoverOn": {"statusCodes": [503, 503]}}),
        ("timeout-zero", {"targets": [t("openai-a", "m")], "perAttemptTimeout": "0s"}),
        ("timeout-big", {"targets": [t("openai-a", "m")], "perAttemptTimeout": "301s"}),
        ("threshold-zero", {"targets": [t("openai-a", "m")], "suspendAfterConsecutiveFailures": 0}),
        ("suspend-long", {"targets": [t("openai-a", "m")], "suspendDuration": "2h"}),
        ("probes-high", {"targets": [t("openai-a", "m")], "probeConcurrency": 11}),
        ("recover-zero", {"targets": [t("openai-a", "m")], "recoverAfterSuccessfulProbes": 0}),
        ("internal-key", {"targets": [t("openai-a", "m")], "_role": "dispatch"}),
    ]
    items = []
    for name, params in bad:
        items.append(request(f"Rejected: {name}", "POST", "ctrl", "llm-proxies", proxy_yaml("bad-" + name, params),
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
                         proxy_yaml("bad-router", {"targets": [t("openai-a", "m")]}, extra_op_policies=router),
                         body_type="application/yaml",
                         tests='pm.test("provider-selecting policy is rejected (400)", () => pm.response.to.have.status(400));'))
    return folder("10 Configuration validation", items, "Every invalid configuration is rejected at registration with 400.")


def cleanup():
    proxies = ["basic", "allow", "notimeout", "conn", "noconn", "cross", "twomodels", "exh", "single",
               "susp", "reprobe", "allsusp", "global"]
    bad = ["empty-targets", "unknown-provider", "duplicate-target", "too-many", "status-404", "status-dup",
           "timeout-zero", "timeout-big", "threshold-zero", "suspend-long", "probes-high", "recover-zero",
           "internal-key", "router"]
    items = []
    for p in proxies:
        items.append(request(f"Delete proxy {p}", "DELETE", "ctrl", f"llm-proxies/{pname(p)}",
                             tests='pm.test("deleted", () => pm.expect(pm.response.code).to.be.oneOf([200, 204]));'))
    for b in bad:
        items.append(request(f"Delete leftover bad-{b}", "DELETE", "ctrl", f"llm-proxies/{pname('bad-' + b)}",
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
                 same_provider_two_models(), exhaustion(), suspension(), global_and_update(), validation(), cleanup()],
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

# inference-vote

The audience votes on how to serve a model, Modelplane deploys what they picked,
and it races an optimized deployment on the same hardware.

Three questions, each one an inference concept:

| # | Question | A | B |
|---|---|---|---|
| 1 | What do you care about more? | Keep the model as precise as it was trained (16-bit) | Make room for more conversations (FP8 weights and KV cache) |
| 2 | You have two GPUs. How do you use them? | One engine split across both (tensor parallelism) | Two copies, one per GPU, routed by the endpoint picker |
| 3 | How should the model write its answers? | One token at a time | Guess ahead from what's already in the conversation, the model checks (n-gram speculative decoding) |

Every one of the eight answer sets is two NVIDIA L4s serving Qwen3-8B with
vLLM. `inference-vote render --config BBA` prints any of them.

## How it fits together

```
 laptop (kind)                         cloud (GKE or EKS, created by Modelplane)
 +----------------------+              +------------------------------------------+
 | Crossplane           |   creates    | nodes of 2x L4                           |
 | Modelplane           | -----------> |   optimized ModelDeployment  (2 GPUs)    |
 | bridge  ------------------ polls ---->  voting app  <---- phones (QR code)     |
 |   applies the        |  (outbound   |   audience ModelDeployment   (2 GPUs)    |
 |   audience's pick    |   HTTPS)     |   InferenceGateway  <- chat, load test   |
 +----------------------+              |   OpenTelemetry collector -> Datadog     |
                                       +------------------------------------------+
```

- **The control plane is kind on the presenter's laptop.** Nothing on the
  internet can reach it, so the bridge reaches out: it polls the voting app for
  the winning config, applies the `ModelDeployment`, and reports its status
  back. Only outbound HTTPS, so a phone's hotspot is a working fallback.
- **The voting app** runs on the inference cluster, at
  `https://<load balancer IP>.nip.io`. It gets its own Let's Encrypt
  certificate and keeps it in a Secret, so a restart reuses it. Phones never
  navigate: the page follows whatever the presenter advances to.
- **The optimized config stays hidden until the race**, so the room votes on
  the questions rather than against an answer key.
- **If the audience picks exactly the optimized config**, the opposite is
  deployed instead, so there is still something to compare. Ties go against the
  optimized answer for the same reason.
- **The race** is the last screen. When the presenter starts it, a load test
  (100 coding agents for 12 minutes) and an accuracy test (200 GSM8K math
  problems) start against every deployment that serves; the audience's joins
  whenever it comes up. The screen shows live numbers for each, audience chat
  and load test counted apart, and the two manifests side by side, line by line.
- **Modelplane counts capacity in whole nodes, not GPUs**
  ([modelplane#172](https://github.com/modelplaneai/modelplane/issues/172)):
  each engine pod takes a node to itself. Setup sizes the GPU pool for it: 3
  nodes when the optimized config is one engine across both GPUs, 4 when it is
  two copies.

## Before the talk

You need Docker (give it 8 GB of memory), Devbox, a Datadog API key (unless
you pass `--no-datadog`), and a Google Cloud login, or, for `aws`, an AWS
access key.

```sh
devbox shell

export DD_API_KEY=...

./dot.nu setup google

source .env
```

Setup takes 20 to 30 minutes, most of it Modelplane creating the cluster, and
prints the voting app's address and the presenter token at the end. Accounts
outside Datadog's US1 site also need `DD_SITE`, such as `datadoghq.eu`.

- **GPUs run out one zone at a time**, and an L4 zone that has none now may
  have some a minute later. Setup tries every zone with L4s, US first, and
  reserves the GPU nodes in the first one that grants them; GKE's nodes then
  use that reservation, so they can't be left waiting. `--zone` picks the zone
  instead, and the cluster's region follows it.
- **If setup stops after the cluster is up**, `./dot.nu setup app` finishes it
  without creating another.
- **If nip.io is down**, `--domain` uses your own name instead; setup then
  prints the DNS record to create.

### Rehearsing

```sh
./dot.nu measure
```

`measure` deploys each of the eight configs in turn and writes two times for
each to `timings.md`: when its engines were ready, and when the gateway first
answered for it. The second is what the audience waits for, and the slowest of
the eight is how long they may wait.

`--optimized` defaults to `BAA`, FP8 in one engine split across both GPUs, the
best of the eight in a rehearsal on AWS under the 100-agent load: about 430
tokens a second against about 210 for `BBA`, the same with two copies. Pooling
the GPUs' memory gave one engine room for 48 full-length conversations, against
34 for two copies together, and one shared prefix cache instead of two, which
outweighed the GPUs talking over PCIe. Full precision as two copies was the
worst, at about 95.

### Right before going on stage

```sh
./dot.nu reset
```

It removes the audience's deployment and clears every vote.

## On stage

Open `https://<IP>.nip.io/presenter` and paste the token; the lobby shows the
QR code. **Next** (or the right arrow, or a clicker):

1. opens question 1,
2. closes each question and opens the next, with the bars live as votes come in,
3. after the third, applies the result and shows it deploying: the engines
   loading, then the gateway waiting for Modelplane's route, which takes a few
   minutes more,
4. starts the race, whether or not the audience's deployment serves yet. If it
   crashed, its column says why; the race goes on with the optimized one.

**Reset** needs two clicks.

## After the talk

```sh
./dot.nu destroy google
```

## What a rehearsal on Google showed

Verified on GKE with Modelplane v0.5.0: setup (about 33 minutes), reserving the
GPU nodes, nip.io with a real certificate kept across restarts, voting, the
bridge applying the result two seconds after the vote, FP8 and full precision,
tensor parallelism (about 4.5 minutes from the vote to ready engines), the race
screen, and the load test.

- **Modelplane routes the gateway to ready engines about five minutes after
  they are ready.** Until then every request is a 404. The app counts the
  audience's deployment as serving only once the gateway answers, so the race
  never starts a test against it early. The bridge nudges the ModelService the
  moment its replica is ready, in case the wait is a periodic reconcile; whether
  that shortens it is not verified yet. The whole wait, vote to answers, is
  about ten minutes.

- Full precision leaves room for 2.79 conversations of 8,192 tokens on an L4;
  FP8 weights and KV cache leave room for 17.03. Under the load test, FP8
  served about 43% more tokens per second.
- A draft model doesn't fit next to full-precision Qwen3-8B on one L4 (0.25 GiB
  left for the KV cache), which is why question 3 uses n-gram speculation.

## What a rehearsal on AWS showed

Verified on EKS on 2026-10-18: setup (about 33 minutes, GPUs reserved in the
first zone tried), nip.io with a certificate, both node pools (the two-GPU
engine on the `g6.12xlarge`, copies on the `g6.2xlarge` nodes), tensor
parallelism, n-gram speculation under load, the accuracy test, chat, and 100
load-test agents. From the vote to serving took 3 to 6 minutes: Modelplane
routed the gateway within about half a minute of ready engines.

- **The gateway retries an attempt that sends nothing for a while**, 60 seconds
  unless the ModelService says otherwise, and after five failures in a row it
  stops sending to that deployment for 30 seconds or more. Under 100 agents
  that turned slow answers into bursts of instant 503s. So every caller in the
  app streams, both ModelServices set `timeouts.idle` to 180 seconds, the
  load test's own limit, and the app counts the audience's deployment as
  serving only after both deployments have answered for 20 seconds in a row.
- **On AWS, the gateways sit behind Classic Load Balancers** that cut a
  connection quiet for 60 seconds. Setup raises that to 10 minutes, and the app
  calls the gateway inside the cluster rather than through its load balancer.
- **An NLB has an IP in every zone but forwards only within its own**, so setup
  takes the one in the GPU zone for nip.io.
- Under 100 agents, the optimized config served about 260 tokens a second with
  no failures. One engine across two GPUs at full precision with n-gram
  guessing served about 225. Two full-precision copies with n-gram guessing
  started near 200 and fell to about 80 as the agents' conversations grew,
  with requests timing out after three minutes in the queue.

Not verified yet:

- **Crash reasons** against real engines: no engine crashed in the AWS races.
  They are tested against stand-in logs.
- **`--cache`**, and `runai_streamer` being in the `vllm/vllm-openai` image.
- **The Datadog sink**: `type: datadog` with `api.key` and `api.site` in its
  config.

AWS has no two-L4 instance, so it runs two pools: one-L4 `g6.2xlarge` nodes for
copies and a four-L4 `g6.12xlarge` for one engine split across two GPUs. Setup
reserves both in the first zone of us-east-1, us-east-2 or us-west-2 that has
them, and destroy cancels the reservations, which otherwise bill on their own.
It needs a G-instance vCPU quota of 80 and an access key: setup asks for it
unless `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` are set.

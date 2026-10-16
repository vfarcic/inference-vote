# inference-vote

The audience votes on how to serve a model, Modelplane deploys what they picked,
and it goes head to head with an optimized deployment on the same hardware.

Three questions, each one an inference concept:

| # | Question | A | B |
|---|---|---|---|
| 1 | What do you care about more? | Keep the model as precise as it was trained (16-bit) | Make room for more conversations (FP8 weights and KV cache) |
| 2 | You have two GPUs. How do you use them? | One engine split across both (tensor parallelism) | Two copies, one per GPU, routed by the endpoint picker |
| 3 | How should the model write its answers? | One token at a time | A small draft model guesses ahead, the big one checks (speculative decoding) |

Every one of the eight answer sets is two NVIDIA L4s serving Qwen3-8B with
vLLM. `inference-vote render --config BBA` prints any of them.

## How it fits together

```
 laptop (kind)                         cloud (GKE or EKS, created by Modelplane)
 +----------------------+              +------------------------------------------+
 | Crossplane           |   creates    | 4x L4                                    |
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
- **The voting app** runs on the inference cluster. Phones never navigate: the
  page follows whatever the presenter advances to. It terminates TLS itself with
  a Let's Encrypt certificate, so there is no ingress controller to install.
- **If the audience picks exactly the optimized config**, the opposite is
  deployed instead, so there is still something to compare. Ties go against the
  optimized answer for the same reason.
- **The load test** starts on its own the moment the audience's deployment
  serves, against both deployments at once, as its own gateway caller. The
  verdict is based on it, not on the audience's chat, which is too small and too
  uneven a sample.

## Before the talk

You need Docker (give it 8 GB of memory), Devbox, a Datadog API key, and an
account on the cloud you set up on. On AWS, request a G-instance vCPU quota of
at least 48 for `g6.12xlarge`.

```sh
devbox shell

export DD_API_KEY=...

./dot.nu setup google --domain vote.devopstoolkit.ai
```

`aws` instead of `google` is the fallback when one cloud has no GPUs. Setup
takes 20 to 30 minutes, most of it Modelplane creating the cluster. At the end
it prints the record to set in GoDaddy (an `A` record on Google, a `CNAME` on
AWS, since that load balancer has a name rather than an IP) and the presenter
token.

### Rehearsing

```sh
./dot.nu setup google --domain vote-rehearsal.devopstoolkit.ai --acme-staging

./dot.nu measure

./dot.nu measure --cache    # after a setup with --cache
```

`--acme-staging` keeps rehearsals from using up Let's Encrypt's five
certificates per name per week; phones will warn about the certificate, which
is expected. `measure` deploys each of the eight configs in turn and writes how
long each took to `timings.md`: the slowest is how long the audience may wait.

Rehearsal is also where `--optimized` gets decided. The default, `BBA`, is a
guess, and under heavy load speculative decoding in particular may lose.

### Right before going on stage

```sh
./dot.nu reset
```

It removes the audience's deployment and restarts the voting app and the
bridge, which clears every vote.

## On stage

Open `https://<domain>/presenter`, paste the token, and put the QR code for
`https://<domain>` on screen. **Next** (or the right arrow, or a clicker) opens a
question, closes it, moves on, and after the third applies the result. **Reset**
needs two clicks.

## After the talk

```sh
./dot.nu destroy google
```

## Not verified yet

Everything here was written against Modelplane v0.5.0 and has not yet run on a
real cluster. These are the parts most likely to need a change in the first
rehearsal:

- **Tensor parallelism without `/dev/shm`.** The engine template accepts no
  volumes. Modelplane's own TP8 recipe runs without one; if NCCL complains,
  `NCCL_SHM_DISABLE=1` is the fallback.
- **The draft model alongside a ModelCache.** The cache sets `HF_HUB_CACHE` to
  its mount; if that mount is read-only, downloading the draft model fails.
- **`Qwen/Qwen3-8B-FP8` with `--kv-cache-dtype=fp8` and a draft model** on vLLM
  v0.27.1.
- **`runai_streamer`** being in the `vllm/vllm-openai` image, for `--cache`.
- **The Datadog sink**: `type: datadog` with `api.key` and `api.site` in its
  config.
- **The kubeconfig Secret name** setup looks for (`*vote*-kubeconfig`).
- **Measuring answer quality.** Only question 1 can change an answer; the other
  two are lossless. Nothing measures quality yet.

#!/usr/bin/env nu

source scripts/common.nu

# Everything here runs before or after the talk. During it, the only moving
# parts are the voting app, which the audience reaches from the QR code, and the
# bridge on the kind control plane, which applies what they voted for.

const KIND_CLUSTER = "inference-vote"
const IMAGE = "ghcr.io/vfarcic/inference-vote:latest"
const WORKLOAD_KUBECONFIG = "kubeconfig-vote.yaml"

def main [] {}

# Sets up everything the talk needs
#
# A kind cluster on this machine becomes the Modelplane control plane. Modelplane
# then creates the inference cluster in the chosen cloud, with four L4 GPUs: two
# for the optimized deployment, which is deployed here, and two left free for
# whatever the audience picks. The voting app runs on that cluster too.
#
# Examples:
# > ./dot.nu setup google --domain vote.devopstoolkit.ai
# > ./dot.nu setup aws --domain vote.devopstoolkit.ai --acme-staging
# > ./dot.nu setup google --domain vote.devopstoolkit.ai --no-datadog
# > ./dot.nu setup google    # testing: <load balancer IP>.nip.io, no DNS to set
def --env "main setup" [
    provider: string             # Cloud provider. `google` or `aws`
    --domain = ""                # Public name for the voting app, such as vote.devopstoolkit.ai. Empty uses <IP>.nip.io (Google only)
    --optimized = "BBA"          # The config the audience is compared against, one letter per question
    --cache                      # Serve weights from a ModelCache instead of downloading them on every start
    --acme-staging               # Use Let's Encrypt staging. For rehearsals: real certificates are 5 per name per week
    --billing-account = ""       # Google billing account to link. Auto-detected when empty
    --image = $IMAGE             # The inference-vote image
    --no-datadog                 # Do not send metrics to Datadog, so no API key is needed
    --zone = ""                  # The zone for the GPU nodes. Defaults to us-central1-a on Google, us-east-1b on AWS. Change it when that zone is out of GPUs
] {

    if not ($provider in ["google" "aws"]) {
        print $"(ansi red_bold)($provider)(ansi reset) is not supported. Use `google` or `aws`."
        exit 1
    }
    if $domain == "" and $provider != "google" {
        print $"(ansi red_bold)--domain is required on ($provider)(ansi reset): its load balancer has a name rather than an IP, so nip.io cannot point to it."
        exit 1
    }

    let datadog = not $no_datadog
    let dd_api_key = if not $datadog { "" } else if "DD_API_KEY" in $env { $env.DD_API_KEY } else {
        input --suppress-output $"(ansi green_bold)Datadog API key: (ansi reset)"
    }
    let dd_site = ($env.DD_SITE? | default "datadoghq.com")

    rm --force .env
    $"export PROVIDER=($provider)\n" | save --append .env
    $env.PROVIDER = $provider

    let creds = (main get creds $provider)

    main create control-plane

    if $provider == "google" {
        setup google $billing_account
    } else {
        setup aws $creds
    }

    # Two gateway callers, so Datadog can tell the audience's chat from the load
    # test.
    let audience_key = (random chars --length 32)
    let load_key = (random chars --length 32)

    (
        kubectl --namespace modelplane-system create secret generic inference-vote-keys
            --from-literal $"audience=($audience_key)"
            --from-literal $"loadtest=($load_key)"
            --dry-run=client --output yaml
    ) | kubectl apply --filename -
    kubectl --namespace modelplane-system label secret inference-vote-keys inference-vote/keys=true --overwrite

    # GPUs run out one zone at a time, so the zone is the first thing to change
    # when nodes stay stuck provisioning.
    let default_zone = if $provider == "google" { "us-central1-a" } else { "us-east-1b" }
    let zone = if $zone == "" { $default_zone } else { $zone }
    # Modelplane counts capacity in whole nodes, not GPUs: every engine pod that
    # claims a GPU takes a node to itself. Two copies take two nodes, one engine
    # split across both GPUs takes one. The audience may pick two copies, so
    # there are two nodes for them on top of what the optimized config takes.
    let nodes = 2 + (if ($optimized | str substring 1..1) == "B" { 2 } else { 1 })
    open --raw $"modelplane/($provider)/platform.yaml"
        | str replace $"- ($default_zone)" $"- ($zone)"
        | str replace --all --regex '(?m)^(\s+(?:min|max)?[nN]odeCount): \d+' $"${1}: ($nodes)"
        | kubectl apply --filename -
    kubectl apply --filename modelplane/inference.yaml

    if $datadog {
        (
            kubectl --namespace modelplane-system create secret generic datadog
                --from-literal $"DD_API_KEY=($dd_api_key)"
                --from-literal $"DD_SITE=($dd_site)"
                --dry-run=client --output yaml
        ) | kubectl apply --filename -
        kubectl apply --filename modelplane/datadog.yaml
    }
    if $cache {
        kubectl apply --filename modelplane/cache.yaml
    }

    # The optimized deployment comes from the same renderer the bridge uses for
    # the audience's, so the two can only differ in the three answers.
    docker run --rm $image render --name optimized --config $optimized $"--cache=($cache)"
        | kubectl apply --filename -

    print $"
Modelplane is creating the inference cluster. (ansi yellow_bold)This takes 15 to 25 minutes(ansi reset)."
    kubectl wait inferencecluster/vote --for=condition=Ready --timeout=60m

    (
        main setup app --domain $domain --optimized $optimized --cache=$cache
            --acme-staging=$acme_staging --image $image
    )

    main print source

}

# Deploys the voting app and the bridge onto a cluster setup created
#
# Setup runs it last. Run it on its own to finish a setup that stopped after
# Modelplane created the cluster, without creating a new one.
#
# Examples:
# > ./dot.nu setup app
# > ./dot.nu setup app --domain vote.devopstoolkit.ai
def --env "main setup app" [
    --domain = ""                # Public name for the voting app. Empty uses <IP>.nip.io (Google only)
    --optimized = "BBA"          # The config the audience is compared against, one letter per question
    --cache                      # Must match setup's --cache
    --acme-staging               # Use Let's Encrypt staging
    --image = $IMAGE             # The inference-vote image
] {

    let provider = ($env.PROVIDER? | default "")
    main get workload-kubeconfig --provider $provider

    # The gateway keys setup created, and two tokens the app checks: one for the
    # presenter, one for the bridge.
    let keys = (kubectl --namespace modelplane-system get secret inference-vote-keys --output json | from json | get data)
    let audience_key = ($keys.audience | decode base64 | decode)
    let load_key = ($keys.loadtest | decode base64 | decode)
    let presenter_token = if "PRESENTER_TOKEN" in $env { $env.PRESENTER_TOKEN } else {
        let token = (random chars --length 12)
        $"export PRESENTER_TOKEN=($token)\n" | save --append .env
        $token
    }
    let bridge_token = (random chars --length 32)

    kubectl wait inferencegateway/public --for=condition=Ready --timeout=20m
    let gateway = (kubectl get inferencegateway public --output jsonpath="{.status.address}")

    # The voting app and the image pre-puller, on the inference cluster.
    open --raw k8s/vote.yaml | str replace --all $IMAGE $image
        | kubectl --kubeconfig $WORKLOAD_KUBECONFIG apply --filename -
    kubectl --kubeconfig $WORKLOAD_KUBECONFIG apply --filename k8s/prepull.yaml

    # The voting app waits for its Secret, which needs the domain, which on
    # nip.io needs the load balancer's address.
    let address = (main get vote-address)
    let nip = $domain == ""
    let domain = if $nip { $"($address).nip.io" } else { $domain }
    $"export DOMAIN=($domain)\n" | save --append .env

    (
        kubectl --kubeconfig $WORKLOAD_KUBECONFIG --namespace inference-vote
            create secret generic inference-vote
            --from-literal $"PRESENTER_TOKEN=($presenter_token)"
            --from-literal $"BRIDGE_TOKEN=($bridge_token)"
            --from-literal $"GATEWAY_URL=http://($gateway)"
            --from-literal $"GATEWAY_KEY=($audience_key)"
            --from-literal $"LOAD_KEY=($load_key)"
            --from-literal $"DOMAIN=($domain)"
            --from-literal $"ACME_STAGING=($acme_staging)"
            --from-literal $"OPTIMIZED=($optimized)"
            --dry-run=client --output yaml
    ) | kubectl --kubeconfig $WORKLOAD_KUBECONFIG apply --filename -

    # The bridge, on the control plane, polling the voting app by its public name.
    (
        kubectl --namespace ml-team create secret generic inference-vote-bridge
            --from-literal $"VOTE_URL=https://($domain)"
            --from-literal $"BRIDGE_TOKEN=($bridge_token)"
            --from-literal $"MODEL_CACHE=($cache)"
            --from-literal $"ACME_STAGING=($acme_staging)"
            --dry-run=client --output yaml
    ) | kubectl apply --filename -
    open --raw k8s/bridge.yaml | str replace --all $IMAGE $image | kubectl apply --filename -

    let record = if ($address | str replace --all --regex '[0-9.]' '' | is-empty) {
        $"an (ansi yellow_bold)A(ansi reset) record for ($domain) pointing to (ansi yellow_bold)($address)(ansi reset)"
    } else {
        $"a (ansi yellow_bold)CNAME(ansi reset) record for ($domain) pointing to (ansi yellow_bold)($address)(ansi reset)"
    }

    if not $nip {
        print $"
In GoDaddy, set ($record), with a low TTL such as 600 seconds.
The voting app requests its certificate on its own once the name resolves."
    }

    print $"
Audience:  https://($domain)
Presenter: https://($domain)/presenter  \(token: (ansi yellow_bold)($presenter_token)(ansi reset)\)
Optimized: ($optimized)
"

}

# Creates the kind control plane and installs Crossplane and Modelplane on it
def --env "main create control-plane" [] {

    $env.KUBECONFIG = $"($env.PWD)/kubeconfig-kind.yaml"
    $"export KUBECONFIG=($env.KUBECONFIG)\n" | save --append .env

    # Re-running setup after a failure keeps the control plane already created.
    if ($KIND_CLUSTER in (kind get clusters | lines)) {
        kind export kubeconfig --name $KIND_CLUSTER
    } else {
        kind create cluster --name $KIND_CLUSTER --config modelplane/kind.yaml
    }

    helm repo add crossplane-stable https://charts.crossplane.io/stable --force-update
    (
        helm upgrade --install crossplane crossplane-stable/crossplane
            --namespace crossplane-system --create-namespace
            --set "args={--enable-dependency-version-upgrades}"
            --set-json "provider.defaultActivations=[]"
            --wait
    )

    kubectl apply --filename modelplane/modelplane.yaml
    kubectl wait configuration/modelplane --for=condition=Healthy --timeout=15m

}

# A fresh Google project, so nothing already there is touched, with a service
# account holding exactly the roles Modelplane documents.
def --env "setup google" [billing_account: string] {

    let project_id = $"inference-vote-(date now | format date "%Y%m%d%H%M%S")"
    $env.PROJECT_ID = $project_id
    $"export PROJECT_ID=($project_id)\n" | save --append .env

    gcloud projects create $project_id

    mut billing = $billing_account
    if $billing == "" {
        let accounts = (gcloud billing accounts list --filter "open=true" --format "value(name)" | lines)
        if ($accounts | is-empty) {
            print $"(ansi red_bold)No open billing account found(ansi reset). Pass --billing-account."
            exit 1
        }
        $billing = ($accounts | first)
    }
    gcloud billing projects link $project_id --billing-account $billing

    (
        gcloud services enable --project $project_id
            container.googleapis.com compute.googleapis.com file.googleapis.com
            iam.googleapis.com cloudresourcemanager.googleapis.com serviceusage.googleapis.com
    )

    gcloud iam service-accounts create modelplane --project $project_id
    let sa = $"modelplane@($project_id).iam.gserviceaccount.com"
    let roles = [
        roles/container.admin
        roles/compute.admin
        roles/serviceusage.serviceUsageAdmin
        roles/iam.serviceAccountAdmin
        roles/iam.serviceAccountKeyAdmin
        roles/iam.serviceAccountUser
        roles/resourcemanager.projectIamAdmin
    ]
    for role in $roles {
        (
            gcloud projects add-iam-policy-binding $project_id
                --member $"serviceAccount:($sa)" --role $role --condition=None --quiet
        ) | ignore
    }
    gcloud iam service-accounts keys create gcp-creds.json --iam-account $sa

    (
        kubectl --namespace crossplane-system create secret generic gcp-creds
            --from-file=credentials=gcp-creds.json --dry-run=client --output yaml
    ) | kubectl apply --filename -

    wait crd clusterproviderconfigs.gcp.m.upbound.io
    {
        apiVersion: "gcp.m.upbound.io/v1beta1"
        kind: ClusterProviderConfig
        metadata: { name: default }
        spec: {
            projectID: $project_id
            credentials: {
                source: Secret
                secretRef: { namespace: crossplane-system, name: gcp-creds, key: credentials }
            }
        }
    } | to yaml | kubectl apply --filename -

}

def "setup aws" [creds: record] {

    $"[default]
aws_access_key_id = ($creds.aws_access_key_id)
aws_secret_access_key = ($creds.aws_secret_access_key)
" | save --force aws-creds.conf

    (
        kubectl --namespace crossplane-system create secret generic aws-creds
            --from-file=credentials=aws-creds.conf --dry-run=client --output yaml
    ) | kubectl apply --filename -

    wait crd clusterproviderconfigs.aws.m.upbound.io
    {
        apiVersion: "aws.m.upbound.io/v1beta1"
        kind: ClusterProviderConfig
        metadata: { name: default }
        spec: {
            credentials: {
                source: Secret
                secretRef: { namespace: crossplane-system, name: aws-creds, key: credentials }
            }
        }
    } | to yaml | kubectl apply --filename -

}

# Waits for a CRD that a Crossplane package installs asynchronously
def "wait crd" [name: string] {
    mut attempts = 0
    while (kubectl get crd $name | complete).exit_code != 0 {
        $attempts = $attempts + 1
        if $attempts > 120 {
            error make { msg: $"CRD ($name) did not appear within 10 minutes" }
        }
        sleep 5sec
    }
    kubectl wait --for condition=established $"crd/($name)" --timeout=2m
}

# Writes the inference cluster's kubeconfig from the Secret Modelplane keeps for it
#
# On Google that kubeconfig carries no credentials, so it gets an access token
# from the current gcloud login. The token, like EKS's, lasts about an hour, so
# fetch it again rather than reusing an old file.
def "main get workload-kubeconfig" [
    --provider = ""   # `google` or `aws`. Defaults to PROVIDER from .env
] {
    let provider = if $provider == "" { $env.PROVIDER? | default "" } else { $provider }
    # Modelplane names it vote-kubeconfig-<suffix>.
    let secrets = (
        kubectl --namespace modelplane-system get secrets --output json | from json | get items
            | where { |s| ($s.metadata.name | str starts-with "vote-kubeconfig") and ("kubeconfig" in $s.data) }
    )
    if ($secrets | is-empty) {
        print $"(ansi red_bold)No kubeconfig Secret for the vote cluster yet(ansi reset). Is the InferenceCluster ready?"
        exit 1
    }
    mut config = ($secrets | first | get data.kubeconfig | decode base64 | decode | from yaml)
    if $provider == "google" {
        let token = (gcloud auth print-access-token | str trim)
        $config.users = ($config.users | each { |u| $u | upsert user { token: $token } })
    }
    $config | to yaml | save --force $WORKLOAD_KUBECONFIG
    chmod 600 $WORKLOAD_KUBECONFIG
    print $"Wrote (ansi yellow_bold)($WORKLOAD_KUBECONFIG)(ansi reset)"
}

# Waits for the voting app's load balancer and returns its IP or hostname
def "main get vote-address" [] {
    mut address = ""
    while $address == "" {
        let ingress = (
            kubectl --kubeconfig $WORKLOAD_KUBECONFIG --namespace inference-vote
                get service inference-vote --output json
                | from json | get status.loadBalancer.ingress? | default []
        )
        if ($ingress | is-not-empty) {
            $address = ($ingress.0.ip? | default ($ingress.0.hostname? | default ""))
        }
        if $address == "" { sleep 5sec }
    }
    $address
}

# Measures how long each of the eight configs takes to serve
#
# Applies each one as the audience deployment in turn, waits until it is ready,
# records the time, and removes it. Run it in rehearsal: the slowest of the
# eight is how long the audience may wait on stage. Writes timings.md.
#
# Examples:
# > ./dot.nu measure
# > ./dot.nu measure --configs [AAB BBA]
def "main measure" [
    --configs = [AAA AAB ABA ABB BAA BAB BBA BBB]
    --cache                # Measure with the ModelCache
    --image = $IMAGE
] {
    mut rows = []
    for config in $configs {
        print $"Measuring (ansi yellow_bold)($config)(ansi reset)..."
        let start = (date now)
        docker run --rm $image render --name audience --config $config $"--cache=($cache)"
            | kubectl apply --filename -
        mut ready = false
        while not $ready {
            sleep 5sec
            let status = (kubectl --namespace ml-team get modeldeployment audience --output json | from json | get status? | default {})
            let replicas = ($status.replicas? | default { total: 0, ready: 0 })
            $ready = ($replicas.total? | default 0) > 0 and ($replicas.ready? | default 0) == ($replicas.total? | default 0)
        }
        let seconds = ((date now) - $start | into int) / 1_000_000_000
        print $"  ready in ($seconds)s"
        $rows = ($rows | append { config: $config, seconds: $seconds })
        kubectl --namespace ml-team delete modeldeployment audience --wait=true
    }
    $rows | to md | save --force timings.md
    $rows
}

# Clears the audience's deployment and every vote, ready for the talk
#
# Restarting the voting app drops its in-memory votes; restarting the bridge
# drops its memory of what it last applied.
def "main reset" [] {
    kubectl --namespace ml-team delete modeldeployment audience --ignore-not-found --wait=true
    main get workload-kubeconfig
    kubectl --kubeconfig $WORKLOAD_KUBECONFIG --namespace inference-vote rollout restart deployment inference-vote
    kubectl --namespace ml-team rollout restart deployment inference-vote-bridge
}

# Destroys everything setup created
#
# Modelplane deletes the inference cluster it created; on Google the whole
# project goes too, which catches anything left behind.
#
# Examples:
# > ./dot.nu destroy google
# > ./dot.nu destroy aws
def --env "main destroy" [
    provider: string   # Cloud provider. `google` or `aws`
] {

    # An InferenceCluster cannot be deleted while anything composes onto it, so
    # the deployments, caches and gateway go first.
    kubectl --namespace ml-team delete modeldeployments --all --wait=true
    kubectl --namespace ml-team delete modelcaches --all --ignore-not-found --wait=true
    kubectl delete inferencegateway public --ignore-not-found --wait=true
    kubectl delete inferencecluster vote --ignore-not-found --timeout=60m

    if $provider == "google" and ("PROJECT_ID" in $env) {
        gcloud projects delete $env.PROJECT_ID --quiet
    }

    kind delete cluster --name $KIND_CLUSTER

    rm --force kubeconfig-kind.yaml $WORKLOAD_KUBECONFIG gcp-creds.json aws-creds.conf
    main delete temp_files

}

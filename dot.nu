#!/usr/bin/env nu

source scripts/common.nu

# Everything here runs before or after the talk. During it, the only moving
# parts are the voting app, which the audience reaches from the QR code, and the
# bridge on the kind control plane, which applies what they voted for.

const KIND_CLUSTER = "inference-vote"
const IMAGE = "ghcr.io/vfarcic/inference-vote:latest"
const WORKLOAD_KUBECONFIG = "kubeconfig-vote.yaml"

# Zones with L4s, in the order setup tries them: US first, then Europe. Setup
# takes the first that has the GPU nodes free right now.
const GOOGLE_ZONES = [
    us-central1-a us-central1-b us-central1-c
    us-east4-a us-east4-c us-east1-b us-east1-c us-east1-d
    us-west1-a us-west1-b us-west1-c us-west4-a us-west4-c
    europe-west4-a europe-west4-b europe-west4-c europe-west1-b europe-west1-c
]

def main [] {}

# Sets up everything the talk needs
#
# A kind cluster on this machine becomes the Modelplane control plane. Modelplane
# then creates the inference cluster in the chosen cloud, with nodes of two L4
# GPUs each: enough for the optimized deployment, which is deployed here, and
# for whatever the audience picks. The voting app runs on that cluster too.
#
# Examples:
# > ./dot.nu setup google                  # https://<load balancer IP>.nip.io, no DNS to set
# > ./dot.nu setup google --no-datadog
# > ./dot.nu setup google --zone us-central1-b
# > ./dot.nu setup aws
def --env "main setup" [
    provider: string             # Cloud provider. `google` or `aws`
    --domain = ""                # Public name for the voting app. Empty, the default, uses <IP>.nip.io (Google only). Set it only if nip.io is down
    --optimized = "BBA"          # The config the audience is compared against, one letter per question
    --cache                      # Serve weights from a ModelCache instead of downloading them on every start
    --acme-staging               # Use Let's Encrypt staging. Only needed with --domain, which has a limit of 5 certificates a week; every nip.io name is new
    --billing-account = ""       # Google billing account to link. Auto-detected when empty
    --image = $IMAGE             # The inference-vote image
    --no-datadog                 # Do not send metrics to Datadog, so no API key is needed
    --load-agents = 100          # Concurrent agents per deployment in the race
    --zone = ""                  # The zone for the GPU nodes; the cluster goes in its region. Empty tries every zone with L4s and takes the first that has them free
] {

    if not ($provider in ["google" "aws"]) {
        print $"(ansi red_bold)($provider)(ansi reset) is not supported. Use `google` or `aws`."
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

    let pools = (pool sizes $provider $optimized)

    # Keys that are not in the environment are asked for. With the keys but no
    # account ID, the ID comes from the keys.
    if $provider == "aws" and ("AWS_ACCESS_KEY_ID" in $env) and not ("AWS_ACCOUNT_ID" in $env) {
        $env.AWS_ACCOUNT_ID = (aws sts get-caller-identity --query Account --output text | str trim)
    }
    let creds = (main get creds $provider)

    main create control-plane

    let zone = if $provider == "google" {
        setup google $billing_account
        reserve gpus $env.PROJECT_ID (if $zone == "" { $GOOGLE_ZONES } else { [$zone] }) $pools.gpu-l4
    } else {
        setup aws $creds
        reserve aws gpus $zone $pools
    }
    $env.GPU_ZONE = $zone

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

    platform $provider $zone $pools | kubectl apply --filename -
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
            --acme-staging=$acme_staging --image $image --load-agents $load_agents
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
    --load-agents = 100          # Concurrent agents per deployment in the race. More load makes the differences bigger, and full precision's answers slower
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
    # The voting app runs next to the gateway, so it calls the gateway's own
    # Service rather than its load balancer. AWS puts a Classic Load Balancer in
    # front of it that drops any request quiet for 60 seconds, and slow answers
    # under load take longer than that.
    let gateway_service = (
        kubectl --kubeconfig $WORKLOAD_KUBECONFIG --namespace envoy-gateway-system get services
            --selector gateway.envoyproxy.io/owning-gateway-name=inference-gateway,gateway.envoyproxy.io/owning-gateway-namespace=modelplane-system
            --output jsonpath="{.items[0].metadata.name}"
    )
    let gateway = $"($gateway_service).envoy-gateway-system.svc"

    # Modelplane's public gateway reaches the cluster gateway through the
    # cluster gateway's load balancer. On AWS that is a Classic Load Balancer
    # that cuts any request quiet for 60 seconds, which slow answers under load
    # are, and after five cuts the public gateway stops sending to it for 30
    # seconds.
    if $provider == "aws" {
        (
            kubectl --kubeconfig $WORKLOAD_KUBECONFIG --namespace envoy-gateway-system
                annotate services --selector app.kubernetes.io/component=proxy --overwrite
                service.beta.kubernetes.io/aws-load-balancer-connection-idle-timeout=600
        )
    }

    # The voting app and the image pre-puller, on the inference cluster.
    open --raw k8s/vote.yaml | str replace --all $IMAGE $image
        | kubectl --kubeconfig $WORKLOAD_KUBECONFIG apply --filename -
    kubectl --kubeconfig $WORKLOAD_KUBECONFIG apply --filename k8s/prepull.yaml

    # The voting app waits for its Secret, which needs the domain, which on
    # nip.io needs the load balancer's address.
    let address = (main get vote-address)
    let nip = $domain == ""
    let domain = if $nip { $"(ip of $address ($env.GPU_ZONE? | default "")).nip.io" } else { $domain }
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
            --from-literal $"MODEL_CACHE=($cache)"
            --from-literal $"LOAD_AGENTS=($load_agents)"
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

    # Pods read their Secrets only when they start. A restart also spares the
    # app the back-off it entered while its Secret didn't exist yet.
    kubectl --kubeconfig $WORKLOAD_KUBECONFIG --namespace inference-vote rollout restart deployment inference-vote
    kubectl --namespace ml-team rollout restart deployment inference-vote-bridge

    let record = if ($address | str replace --all --regex '[0-9.]' '' | is-empty) {
        $"an (ansi yellow_bold)A(ansi reset) record for ($domain) pointing to (ansi yellow_bold)($address)(ansi reset)"
    } else {
        $"a (ansi yellow_bold)CNAME(ansi reset) record for ($domain) pointing to (ansi yellow_bold)($address)(ansi reset)"
    }

    if not $nip {
        print $"
In your DNS, set ($record), with a low TTL such as 600 seconds.
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

# How many nodes each GPU pool needs
#
# Modelplane counts capacity in whole nodes, not GPUs: every engine pod that
# claims a GPU takes a node to itself. Two copies take two nodes, one engine
# split across two GPUs takes one. The audience may pick either, so each pool
# has room for the audience's worst case on top of what the optimized config
# takes. On Google every node has two L4s, so one pool serves both; AWS has no
# two-L4 instance, so copies get one-L4 nodes and the split engine a four-L4 one.
def "pool sizes" [provider: string, optimized: string] {
    let copies = ($optimized | str substring 1..1) == "B"
    if $provider == "google" {
        { gpu-l4: (2 + (if $copies { 2 } else { 1 })) }
    } else {
        { copies: (2 + (if $copies { 2 } else { 0 })), one-engine: (1 + (if $copies { 0 } else { 1 })) }
    }
}

# The platform manifest for a provider, with the zone, its region, and the pool
# sizes filled in
def platform [provider: string, zone: string, pools: record] {
    # The cluster's region follows the zone: us-central1-a is in us-central1,
    # us-east-1b in us-east-1.
    let region = if $provider == "google" { $zone | str replace --regex '-[a-z]$' '' } else { $zone | str replace --regex '[a-z]$' '' }
    let source = if $provider == "google" { "gke" } else { "eks" }
    open --raw $"modelplane/($provider)/platform.yaml" | from yaml | each { |doc|
        if $doc.kind != "InferenceCluster" { return $doc }
        $doc
            | upsert spec.cluster ($doc.spec.cluster | upsert $source { region: $region })
            | upsert spec.nodePools ($doc.spec.nodePools | each { |pool|
                let n = ($pools | get $pool.name)
                $pool | upsert nodeCount $n | upsert minNodeCount $n | upsert maxNodeCount $n | upsert zones [$zone]
            })
    } | each { to yaml } | str join "---\n"
}

# Regions setup tries on AWS, in order. Every zone in them that offers both
# instance types is a candidate.
const AWS_REGIONS = [us-east-1 us-east-2 us-west-2]

# The AWS counterpart of `reserve gpus`: holds both pools' nodes in one zone
#
# Both pools must be in the same zone, so a zone counts only if it grants every
# reservation; one that grants some gets those cancelled before the next is
# tried. EC2 instances use an open capacity reservation that matches them on
# their own, the EKS nodes included. Unlike Google's, these reservations don't
# belong to a project that destroy deletes, so destroy cancels them.
def "reserve aws gpus" [zone: string, pools: record] {
    let types = { copies: "g6.2xlarge", one-engine: "g6.12xlarge" }
    let wanted = ($pools | transpose pool count | where count > 0 | each { |p| { type: ($types | get $p.pool), count: $p.count } })
    let candidates = if $zone != "" {
        [{ region: ($zone | str replace --regex '[a-z]$' ''), zone: $zone }]
    } else {
        $AWS_REGIONS | each { |region|
            let offered = ($wanted | each { |w|
                (
                    aws ec2 describe-instance-type-offerings --region $region --location-type availability-zone
                        --filters $"Name=instance-type,Values=($w.type)" --query "InstanceTypeOfferings[].Location" --output json
                ) | from json
            })
            $offered | reduce { |it, acc| $acc | where { |z| $z in $it } } | sort | each { |z| { region: $region, zone: $z } }
        } | flatten
    }
    for c in $candidates {
        mut ids = []
        mut failure = ""
        for w in $wanted {
            let result = (
                aws ec2 create-capacity-reservation --region $c.region --availability-zone $c.zone
                    --instance-type $w.type --instance-platform "Linux/UNIX" --instance-count $w.count
                    --instance-match-criteria open --end-date-type unlimited
                    --query CapacityReservation.CapacityReservationId --output text
            ) | complete
            if $result.exit_code != 0 {
                $failure = $result.stderr
                break
            }
            $ids = ($ids | append ($result.stdout | str trim))
        }
        if $failure == "" {
            print $"Reserved ($wanted | each { |w| $'($w.count) x ($w.type)' } | str join ' and ') in (ansi yellow_bold)($c.zone)(ansi reset)."
            $"export GPU_ZONE=($c.zone)\nexport AWS_RESERVATION_REGION=($c.region)\nexport AWS_RESERVATIONS=($ids | str join ',')\n" | save --append .env
            return $c.zone
        }
        for id in $ids {
            aws ec2 cancel-capacity-reservation --region $c.region --capacity-reservation-id $id | ignore
        }
        if ($failure | str contains "LimitExceeded") or ($failure | str contains "quota") {
            print $"(ansi red_bold)Not enough quota in ($c.region)(ansi reset):\n($failure)"
            print "Request a higher \"Running On-Demand G and VT instances\" vCPU quota (80 for this setup), or pass --zone with another region."
            exit 1
        }
        let why = if ($failure | str contains "InsufficientInstanceCapacity") { "no GPUs free" } else { $failure | lines | first }
        print $"($c.zone): ($why)"
    }
    print $"(ansi red_bold)None of ($candidates | get zone | str join ', ') has the GPU nodes free right now.(ansi reset) Try again in a few minutes."
    exit 1
}

# Holds the GPU nodes before anything asks for them, and returns the zone
#
# GPUs run out one zone at a time, and a GKE node pool that can't get them
# just waits. A reservation succeeds only if the capacity is free right now, so
# setup tries zones in order and keeps the first one that grants it. GKE nodes
# consume a matching reservation in their own project on their own, so the node
# pool lands on capacity that is already ours. Deleting the project, as destroy
# does, deletes the reservation with it.
def "reserve gpus" [project: string, zones: list<string>, nodes: int] {
    for zone in $zones {
        let result = (
            gcloud compute reservations create inference-vote-gpus --project $project --zone $zone
                --vm-count $nodes --machine-type g2-standard-24 --accelerator count=2,type=nvidia-l4
        ) | complete
        if $result.exit_code == 0 {
            print $"Reserved (ansi yellow_bold)($nodes)(ansi reset) GPU nodes in (ansi yellow_bold)($zone)(ansi reset)."
            $"export GPU_ZONE=($zone)\n" | save --append .env
            return $zone
        }
        if ($result.stderr | str contains "Quota") {
            print $"(ansi red_bold)Not enough GPU quota in ($zone)(ansi reset):\n($result.stderr)"
            print $"Request NVIDIA_L4_GPUS of at least ($nodes * 2) for the project, or pass --zone with another region."
            exit 1
        }
        let why = if ($result.stderr | str contains "ZONE_RESOURCE_POOL_EXHAUSTED") { "no GPUs free" } else { $result.stderr | lines | first }
        print $"($zone): ($why)"
    }
    print $"(ansi red_bold)None of ($zones | str join ', ') has ($nodes) GPU nodes free right now.(ansi reset) Try again in a few minutes."
    exit 1
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
    } else if $provider == "aws" {
        # The Secret's token lasts 15 minutes, so kubectl asks AWS for a new one
        # on every call instead. The user is named after the EKS cluster.
        let region = ($config.clusters.0.cluster.server | parse --regex '\.(?<region>[a-z0-9-]+)\.eks\.amazonaws\.com' | get 0.region)
        $config.users = ($config.users | each { |u| $u | upsert user { exec: {
            apiVersion: "client.authentication.k8s.io/v1beta1"
            command: "aws"
            args: [eks get-token --output json --region $region --cluster-name $u.name]
        } } })
    }
    $config | to yaml | save --force $WORKLOAD_KUBECONFIG
    chmod 600 $WORKLOAD_KUBECONFIG
    print $"Wrote (ansi yellow_bold)($WORKLOAD_KUBECONFIG)(ansi reset)"
}

# The IP behind an address: itself if it is one, otherwise what its name
# resolves to. AWS load balancers have names, and a new one takes a few minutes
# to resolve.
def "ip of" [address: string, zone: string] {
    if ($address =~ '^[0-9.]+$') {
        return $address
    }
    # An AWS NLB has an IP in every zone of the VPC, but forwards only to nodes
    # in its own zone, and DNS lists them all until health checks settle. Only
    # the IP in the GPU zone is sure to have nodes behind it.
    if $zone != "" and ($address | str ends-with ".amazonaws.com") {
        let name = ($address | split row "." | first | str replace --regex '^(.+)-([^-]+)$' 'net/$1/$2')
        mut ip = ""
        while $ip == "" {
            $ip = (
                aws ec2 describe-network-interfaces --region ($zone | str substring 0..-2)
                    --filters $"Name=description,Values=ELB ($name)" $"Name=availability-zone,Values=($zone)"
                    --query "NetworkInterfaces[0].Association.PublicIp" --output text
                    | str trim
            )
            if $ip in ["" "None"] { $ip = ""; sleep 10sec }
        }
        return $ip
    }
    mut ip = ""
    while $ip == "" {
        $ip = (^dig +short $address | lines | where { |l| $l =~ '^[0-9.]+$' } | get 0? | default "")
        if $ip == "" { sleep 10sec }
    }
    $ip
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
# Applies each one as the audience deployment in turn, records when its engines
# were ready and when the gateway first answered for it, and removes it. Run it
# in rehearsal: the slowest of the eight is how long the audience may wait on
# stage. Writes timings.md.
#
# Examples:
# > ./dot.nu measure
# > ./dot.nu measure --configs [AAB BBA]
def "main measure" [
    --configs = [AAA AAB ABA ABB BAA BAB BBA BBB]
    --cache                # Measure with the ModelCache
    --image = $IMAGE
] {
    # Timed to the gateway answering, which is what the audience waits for:
    # Modelplane routes it to ready engines some minutes after they are ready.
    let gateway = (kubectl get inferencegateway public --output jsonpath="{.status.address}")
    let key = (kubectl --namespace modelplane-system get secret inference-vote-keys --output json | from json | get data.loadtest | decode base64 | decode)
    let elapsed = { |start| ((date now) - $start | into int) // 1_000_000_000 }
    let clock = { |s| $"($s // 60)m ($s mod 60)s" }
    mut rows = []
    for config in $configs {
        print $"Measuring (ansi yellow_bold)($config)(ansi reset)..."
        let start = (date now)
        docker run --rm $image render --name audience --config $config $"--cache=($cache)"
            | kubectl apply --filename -
        # A config that can't start, like one that runs out of GPU memory,
        # crash-loops rather than failing, so give up after a while.
        mut engines = null
        mut answers = null
        while $answers == null and (do $elapsed $start) < 1200 {
            sleep 5sec
            if $engines == null {
                let status = (kubectl --namespace ml-team get modeldeployment audience --output json | from json | get status? | default {})
                let replicas = ($status.replicas? | default { total: 0, ready: 0 })
                if ($replicas.total? | default 0) > 0 and ($replicas.ready? | default 0) == ($replicas.total? | default 0) {
                    $engines = (do $elapsed $start)
                    print $"  engines ready after (do $clock $engines)"
                }
            } else {
                let probe = try {
                    (
                        http post --full --allow-errors --max-time 20sec --content-type application/json
                            --headers { Authorization: $"Bearer ($key)" }
                            $"http://($gateway)/v1/chat/completions"
                            { model: "ml-team/audience", messages: [{ role: user, content: Hi }], max_tokens: 1 }
                    )
                } catch { { status: 0 } }
                if $probe.status == 200 {
                    $answers = (do $elapsed $start)
                }
            }
        }
        let result = if $answers != null {
            $"(do $clock $answers), engines ready at (do $clock $engines)"
        } else if $engines != null {
            $"engines ready at (do $clock $engines), but the gateway did not answer within 20m"
        } else {
            "did not start within 20m"
        }
        print $"  ($result)"
        $rows = ($rows | append { config: $config, engines: $engines, answers: $answers, result: $result })
        kubectl --namespace ml-team delete modeldeployment audience --wait=true
    }
    $rows | to md | save --force timings.md
    $rows
}

# Clears the audience's deployment and every vote, ready for the talk
#
# The votes are cleared through the presenter API rather than by restarting the
# voting app: the app keeps its certificate across restarts, but there is no
# reason to risk it. Restarting the bridge drops its memory of what it last
# applied.
def "main reset" [] {
    kubectl --namespace ml-team delete modeldeployment audience --ignore-not-found --wait=true
    (
        http post --headers { Authorization: $"Bearer ($env.PRESENTER_TOKEN)" }
            $"https://($env.DOMAIN)/api/presenter/reset" ""
    ) | ignore
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
    # the deployments, services (their routes), caches and gateway go first.
    kubectl --namespace ml-team delete modeldeployments --all --wait=true
    kubectl --namespace ml-team delete modelservices --all --wait=true
    # A ModelService's deletion returns before Modelplane removes its routes,
    # and the cluster can't be deleted while a route uses it.
    mut attempts = 0
    while (kubectl get modelroutes --all-namespaces --output name | complete | get stdout | str trim) != "" and $attempts < 60 {
        sleep 5sec
        $attempts += 1
    }
    kubectl --namespace ml-team delete modelcaches --all --ignore-not-found --wait=true
    kubectl delete inferencegateway public --ignore-not-found --wait=true
    kubectl delete telemetrydestination datadog --ignore-not-found --wait=true
    kubectl delete inferencecluster vote --ignore-not-found --timeout=60m

    if $provider == "google" and ("PROJECT_ID" in $env) {
        gcloud projects delete $env.PROJECT_ID --quiet
    }
    # AWS reservations bill until cancelled, cluster or not.
    if $provider == "aws" and ("AWS_RESERVATIONS" in $env) {
        for id in ($env.AWS_RESERVATIONS | split row ",") {
            aws ec2 cancel-capacity-reservation --region $env.AWS_RESERVATION_REGION --capacity-reservation-id $id | ignore
        }
    }

    kind delete cluster --name $KIND_CLUSTER

    rm --force kubeconfig-kind.yaml $WORKLOAD_KUBECONFIG gcp-creds.json aws-creds.conf
    main delete temp_files

}

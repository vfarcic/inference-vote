// inference-vote is one binary with four jobs:
//
//	serve   the voting app, on the cluster Modelplane created
//	bridge  applies the winning config, on the control plane
//	render  prints a config's ModelDeployment, for the setup
//	load    the agent workload the verdict is based on
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/vfarcic/inference-vote/internal/bridge"
	"github.com/vfarcic/inference-vote/internal/choice"
	"github.com/vfarcic/inference-vote/internal/kube"
	"github.com/vfarcic/inference-vote/internal/load"
	"github.com/vfarcic/inference-vote/internal/poll"
	"github.com/vfarcic/inference-vote/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inference-vote serve|bridge|render|load [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "bridge":
		err = runBridge(ctx, os.Args[2:])
	case "render":
		err = render(os.Args[2:])
	case "load":
		err = runLoad(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "address for plain HTTP when no domain is set")
	domain := fs.String("domain", env("DOMAIN", ""), "serve HTTPS for this name with a Let's Encrypt certificate")
	staging := fs.Bool("acme-staging", env("ACME_STAGING", "") == "true", "use Let's Encrypt staging, for rehearsals (5 real certificates per name per week)")
	optimized := fs.String("optimized", env("OPTIMIZED", "BBA"), "the optimized config the audience is compared against")
	loadAgents := fs.Int("load-agents", 16, "concurrent agents per deployment once the audience's serves; 0 disables")
	loadDuration := fs.Duration("load-duration", 12*time.Minute, "how long the load test runs")
	fs.Parse(args)

	opt, err := choice.Parse(*optimized)
	if err != nil {
		return err
	}
	cfg := server.Config{
		PresenterToken: os.Getenv("PRESENTER_TOKEN"),
		BridgeToken:    os.Getenv("BRIDGE_TOKEN"),
		GatewayURL:     os.Getenv("GATEWAY_URL"),
		GatewayKey:     os.Getenv("GATEWAY_KEY"),
		PodSelector:    env("POD_SELECTOR", "modelplane.ai/deployment=audience,modelplane.ai/engine"),
		Load: load.Config{
			GatewayURL: os.Getenv("GATEWAY_URL"),
			Key:        os.Getenv("LOAD_KEY"),
			Targets:    []string{"audience", "optimized"},
			Agents:     *loadAgents,
			Duration:   *loadDuration,
			MaxTokens:  256,
			Seed:       42,
		},
	}
	if cfg.PresenterToken == "" || cfg.BridgeToken == "" {
		return fmt.Errorf("PRESENTER_TOKEN and BRIDGE_TOKEN must be set")
	}
	k, err := kube.InCluster()
	if err != nil {
		log.Printf("no cluster, so no pod progress: %v", err)
		k = nil
	}
	s := server.New(cfg, poll.New(opt), k)
	go s.WatchPods(ctx)
	go s.StartLoadWhenReady(ctx)

	handler := s.Handler()
	if *domain == "" {
		srv := &http.Server{Addr: *addr, Handler: handler}
		go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
		log.Printf("serving http on %s, optimized %s", *addr, opt)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		return nil
	}

	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(*domain),
		Cache:      autocert.DirCache(env("CERT_CACHE", "/tmp/certs")),
	}
	if *staging {
		m.Client = &acme.Client{DirectoryURL: "https://acme-staging-v02.api.letsencrypt.org/directory"}
	}
	// Port 80 answers the HTTP-01 challenge and sends everyone else to HTTPS.
	go func() { log.Fatal(http.ListenAndServe(":80", m.HTTPHandler(nil))) }()
	srv := &http.Server{Addr: ":443", Handler: handler, TLSConfig: &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12}}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	log.Printf("serving https for %s, optimized %s", *domain, opt)
	if err := srv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func runBridge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ExitOnError)
	voteURL := fs.String("vote-url", env("VOTE_URL", ""), "the voting app's public address")
	name := fs.String("name", "audience", "the ModelDeployment the bridge owns")
	every := fs.Duration("every", 3*time.Second, "how often to poll")
	cache := fs.Bool("cache", env("MODEL_CACHE", "") == "true", "load weights from a ModelCache")
	fs.Parse(args)
	if *voteURL == "" || os.Getenv("BRIDGE_TOKEN") == "" {
		return fmt.Errorf("--vote-url and BRIDGE_TOKEN must be set")
	}
	k, err := kube.InCluster()
	if err != nil {
		return err
	}
	b := &bridge.Bridge{
		VoteURL: strings.TrimRight(*voteURL, "/"), Token: os.Getenv("BRIDGE_TOKEN"), Name: *name,
		Options: choice.Options{Cache: *cache}, Kube: k, Every: *every,
		Insecure: env("ACME_STAGING", "") == "true",
	}
	log.Printf("bridge polling %s", b.VoteURL)
	b.Run(ctx)
	return nil
}

func render(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	name := fs.String("name", "optimized", "ModelDeployment name")
	config := fs.String("config", env("OPTIMIZED", "BBA"), "three letters, one answer per question")
	cache := fs.Bool("cache", env("MODEL_CACHE", "") == "true", "load weights from a ModelCache")
	fs.Parse(args)
	c, err := choice.Parse(*config)
	if err != nil {
		return err
	}
	out, err := choice.Manifest(*name, c, choice.Options{Cache: *cache})
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func runLoad(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	gateway := fs.String("gateway", env("GATEWAY_URL", ""), "InferenceGateway base URL")
	targets := fs.String("targets", "audience,optimized", "ModelServices to load, comma separated")
	agents := fs.Int("agents", 16, "concurrent agents per target")
	duration := fs.Duration("duration", 10*time.Minute, "how long to run")
	maxTokens := fs.Int("max-tokens", 256, "answer length cap")
	fs.Parse(args)
	if *gateway == "" || os.Getenv("GATEWAY_KEY") == "" {
		return fmt.Errorf("--gateway and GATEWAY_KEY must be set")
	}
	cfg := load.Config{
		GatewayURL: *gateway, Key: os.Getenv("GATEWAY_KEY"), Targets: strings.Split(*targets, ","),
		Agents: *agents, Duration: *duration, MaxTokens: *maxTokens, Seed: 42,
	}
	log.Printf("loading %v with %d agents each for %s", cfg.Targets, cfg.Agents, cfg.Duration)
	for _, r := range load.Run(ctx, cfg) {
		fmt.Println(r)
	}
	return nil
}

// Command llm-gateway-go is an OpenAI-compatible proxy in front of several LLM providers:
// model aliases with fallback, per-client rate limits, streaming, and token/cost metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	path := flag.String("config", "gateway.json", "path to the JSON config")
	flag.Parse()
	cfg, err := LoadConfig(*path)
	if err != nil {
		log.Fatalf("llm-gateway: %v", err)
	}
	if len(cfg.Keys) == 0 {
		log.Print("llm-gateway: no keys configured, every request is accepted as \"anonymous\"; keep it on localhost")
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: NewGateway(cfg).Handler(), ReadHeaderTimeout: 10 * time.Second}
	// SIGTERM too: it's what Docker and Kubernetes send, and open streams deserve a graceful drain.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second) // let streams finish
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("llm-gateway: listening on %s with models %v", cfg.Listen, cfg.aliases())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("llm-gateway: %v", err)
	}
}

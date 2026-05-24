package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/hanwen/runclaude/fakeapi"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address (host:port). With :0, picks a free port and prints it to stdout.")
	portFile := flag.String("port-file", "", "if set, write the chosen port to this file once listening")
	apiKey := flag.String("anthropic-api-key", "", "expected Anthropic API key (enables /v1/messages route)")
	awsAccessKey := flag.String("aws-access-key-id", "", "expected AWS access key id (enables Bedrock route)")
	awsSecretKey := flag.String("aws-secret-access-key", "", "expected AWS secret access key")
	awsSessionToken := flag.String("aws-session-token", "", "expected AWS session token (optional)")
	awsRegion := flag.String("aws-region", "us-east-1", "AWS region used to verify SigV4")
	oauthRefresh := flag.String("oauth-refresh-token", "", "expected refresh token at /v1/oauth/token (enables OAuth refresh route)")
	oauthNewRefresh := flag.String("oauth-new-refresh-token", "", "if set, /v1/oauth/token returns this as the rotated refresh token")
	flag.Parse()

	cfg := fakeapi.Config{
		APIKey:            *apiKey,
		AWSAccessKey:      *awsAccessKey,
		AWSSecretKey:      *awsSecretKey,
		AWSSessionToken:   *awsSessionToken,
		AWSRegion:         *awsRegion,
		OAuthRefreshToken: *oauthRefresh,
		OAuthNewRefresh:   *oauthNewRefresh,
		Logger:            log.New(os.Stderr, "fake-anthropic: ", log.LstdFlags|log.Lmicroseconds),
	}
	if cfg.APIKey == "" && cfg.AWSSecretKey == "" {
		log.Fatal("must set -anthropic-api-key or -aws-secret-access-key (or both)")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("%d\n", port)
	if *portFile != "" {
		if err := os.WriteFile(*portFile, []byte(fmt.Sprintf("%d\n", port)), 0644); err != nil {
			log.Fatalf("write port-file: %v", err)
		}
	}
	cfg.Logger.Printf("listening on %s", ln.Addr())
	srv := &http.Server{Handler: fakeapi.Handler(cfg)}
	if err := srv.Serve(ln); err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// refreshingCredentialProvider wraps an AWS credential provider and
// transparently re-runs awsAuthRefresh when credentials have expired.
// Only one refresh runs at a time; other goroutines block until it finishes.
type refreshingCredentialProvider struct {
	mu      sync.Mutex
	cfg     aws.Config
	refresh string
	logger  *log.Logger
}

func (p *refreshingCredentialProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	creds, err := p.cfg.Credentials.Retrieve(ctx)
	if err == nil {
		return creds, nil
	}
	if p.refresh == "" {
		return aws.Credentials{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if creds, err2 := p.cfg.Credentials.Retrieve(ctx); err2 == nil {
		return creds, nil
	}
	p.logger.Printf("AWS credentials expired (%v); running awsAuthRefresh: %s", err, p.refresh)
	cmd := exec.Command("sh", "-c", p.refresh)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if rerr := cmd.Run(); rerr != nil {
		return aws.Credentials{}, fmt.Errorf("awsAuthRefresh: %w", rerr)
	}
	newCfg, rerr := awsconfig.LoadDefaultConfig(ctx)
	if rerr != nil {
		return aws.Credentials{}, fmt.Errorf("reload AWS config after refresh: %w", rerr)
	}
	p.cfg = newCfg
	return p.cfg.Credentials.Retrieve(ctx)
}

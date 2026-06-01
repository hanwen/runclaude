package creds

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

// RefreshingCredentialProvider wraps an AWS credential provider and
// transparently re-runs a refresh command (e.g. `aws sso login`) when the
// underlying credentials have expired. Only one refresh runs at a time;
// other goroutines block until it finishes.
type RefreshingCredentialProvider struct {
	mu      sync.Mutex
	cfg     aws.Config
	refresh string
	logger  *log.Logger
}

// NewRefreshingCredentialProvider returns a provider that retrieves
// credentials from cfg, falling back to running refresh (under `sh -c`) and
// reloading the default AWS config when retrieval fails. refresh may be empty
// to disable fallback. logger must be non-nil.
func NewRefreshingCredentialProvider(cfg aws.Config, refresh string, logger *log.Logger) *RefreshingCredentialProvider {
	return &RefreshingCredentialProvider{cfg: cfg, refresh: refresh, logger: logger}
}

func (p *RefreshingCredentialProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
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

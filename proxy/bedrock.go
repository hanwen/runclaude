package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// IsBedrockHost reports whether host is a Bedrock Runtime endpoint
// (bedrock-runtime.<region>.amazonaws.com).
func IsBedrockHost(host string) bool {
	return strings.HasPrefix(host, "bedrock-runtime.") &&
		strings.HasSuffix(host, ".amazonaws.com")
}

// BedrockRegion extracts the AWS region from a Bedrock Runtime hostname, or
// returns "" if host is not a Bedrock host.
func BedrockRegion(host string) string {
	if !IsBedrockHost(host) {
		return ""
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(host, "bedrock-runtime."), ".amazonaws.com")
	if strings.Contains(mid, ".") {
		return ""
	}
	return mid
}

// InjectBedrock re-signs an in-flight request using AWS credentials retrieved
// from provider. The client (e.g. claude code's @aws-sdk/client-bedrock-runtime)
// may have signed with stub credentials; this strips that signature and
// recomputes SigV4 against the real credentials. Body is unchanged.
func InjectBedrock(host string, r *http.Request, provider aws.CredentialsProvider, signer *v4.Signer, logger *log.Logger) {
	injectBedrockAt(host, r, provider, signer, time.Now(), logger)
}

func injectBedrockAt(host string, r *http.Request, provider aws.CredentialsProvider, signer *v4.Signer, now time.Time, logger *log.Logger) {
	region := BedrockRegion(host)
	if region == "" {
		logger.Printf("bedrock: malformed host %q", host)
		return
	}
	// httputil.ReverseProxy strips hop-by-hop headers after the Director
	// runs. If we re-signed with them present, our SignedHeaders list
	// would name headers the upstream never sees, and Bedrock would
	// reject the signature. Strip them first so we sign exactly what
	// goes on the wire.
	StripHopByHopHeaders(r.Header)
	for _, h := range []string{
		"Authorization",
		"X-Amz-Date",
		"X-Amz-Security-Token",
		"X-Amz-Content-Sha256",
	} {
		r.Header.Del(h)
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			logger.Printf("bedrock: read body: %v", err)
			return
		}
		body = b
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	ctx := r.Context()
	creds, err := provider.Retrieve(ctx)
	if err != nil {
		logger.Printf("bedrock: retrieve credentials: %v", err)
		return
	}
	if err := signer.SignHTTP(ctx, creds, r, hash, "bedrock", region, now); err != nil {
		logger.Printf("bedrock: sign: %v", err)
		return
	}
}

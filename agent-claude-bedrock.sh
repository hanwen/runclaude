#!/bin/bash

curl -v --aws-sigv4 "aws:amz:region:bedrock" \
    --user "key:secret" \
    -H "x-amz-security-token: session_token" \
    -H "content-type: application/json" \
    -d '{
      "anthropic_version": "bedrock-2023-05-31",
      "max_tokens": 10,
      "messages": [{"role": "user", "content": "Say hello."}]
    }' \
    "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-sonnet-4-6/invoke"


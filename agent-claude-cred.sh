#!/bin/bash

curl -v https://api.anthropic.com/v1/messages -d '{"model":"claude-haiku-4-5","max_tokens":10,"messages":[{"role":"user","content":"say hello"}]}'

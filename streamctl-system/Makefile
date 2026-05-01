# streamctl deploy commands.
# Run `make help` to see the list. All commands assume `doctl` is authenticated
# (run `doctl auth init` first if not).

# Use bash, fail loudly, fail fast.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

# Pull the DO API token from doctl and export it for Terraform.
# `doctl auth token` prints the token for the current auth context.
# If you have multiple contexts, switch with `doctl auth switch`.
export DIGITALOCEAN_TOKEN := $(shell doctl auth token 2>/dev/null)

# Cache the droplet's IP across recipes that need it.
# This shells out to terraform once per make invocation, not per recipe.
IP := $(shell cd terraform && terraform output -raw ipv4 2>/dev/null)

.PHONY: help init plan create ip wait-for-nixos pull-hardware-config \
        bootstrap-secret deploy deploy-local-build deploy-dry update \
        build shell run-local \
        ssh logs timers destroy check-token check-ip stream-logs upload

# ---------- help ----------

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
	      /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# Internal sanity check: bail if doctl didn't return a token.
check-token:
	@if [ -z "$(DIGITALOCEAN_TOKEN)" ]; then \
	  echo "ERROR: doctl auth token returned nothing." >&2; \
	  echo "Run 'doctl auth init' first." >&2; \
	  exit 1; \
	fi

# Internal sanity check: bail if terraform hasn't been applied yet.
check-ip:
	@if [ -z "$(IP)" ]; then \
	  echo "ERROR: no droplet IP in terraform state." >&2; \
	  echo "Run 'make create' first." >&2; \
	  exit 1; \
	fi

# ---------- one-time setup ----------

init: ## Initialize Terraform (run once after clone).
	cd terraform && terraform init

plan: check-token ## Show what Terraform would do without applying.
	cd terraform && terraform plan

# ---------- provisioning ----------

create: check-token ## Create the droplet via Terraform (~1 min + ~5 min for nixos-infect).
	cd terraform && terraform apply
	@echo
	@echo "Droplet created. Wait ~5 minutes for nixos-infect to finish, then:"
	@echo "  make wait-for-nixos"
	@echo "  make pull-hardware-config"
	@echo "  make bootstrap-secret"
	@echo "  make deploy"

ip: check-ip ## Print the droplet's IPv4 address.
	@echo "$(IP)"

wait-for-nixos: check-ip ## Block until the droplet is reachable as NixOS.
	@echo "Waiting for NixOS to come up on $(IP)..."
	@for i in $$(seq 1 60); do \
	  if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
	      root@$(IP) 'test -e /etc/NIXOS' 2>/dev/null; then \
	    echo "NixOS is up."; \
	    exit 0; \
	  fi; \
	  echo "  attempt $$i/60: not ready yet, sleeping 10s..."; \
	  sleep 10; \
	done; \
	echo "Timed out after 10 minutes." >&2; \
	exit 1

pull-hardware-config: check-ip ## Copy hardware-configuration.nix from droplet into our flake.
	scp -o StrictHostKeyChecking=no \
	  root@$(IP):/etc/nixos/hardware-configuration.nix \
	  nixos/hardware-configuration.nix
	@echo "Pulled hardware-configuration.nix from $(IP)."

bootstrap-secret: check-ip ## Generate a streamctl login secret on the droplet.
	@SECRET=$$(openssl rand -base64 32 | tr -d '/+=' | head -c 32); \
	ssh -o StrictHostKeyChecking=no root@$(IP) \
	  "install -d -m 0750 -o streamctl -g streamctl /var/lib/streamctl 2>/dev/null || true; \
	   echo '$$SECRET' > /var/lib/streamctl/secret; \
	   chmod 0400 /var/lib/streamctl/secret; \
	   chown streamctl:streamctl /var/lib/streamctl/secret 2>/dev/null || true"; \
	echo; \
	echo "================================================================"; \
	echo "Login secret for streamctl web UI:"; \
	echo "  $$SECRET"; \
	echo "================================================================"; \
	echo "Save this somewhere safe. It's also at /var/lib/streamctl/secret on the droplet."

# ---------- deploys ----------

deploy: check-ip ## Push the current NixOS config to the droplet (build on droplet).
	nixos-rebuild switch \
	  --flake .#streamctl \
	  --target-host root@$(IP) \
	  --build-host root@$(IP)

deploy-local-build: check-ip ## Deploy but build locally, then copy result to droplet.
	nixos-rebuild switch \
	  --flake .#streamctl \
	  --target-host root@$(IP)

deploy-dry: check-ip ## Test the config without switching (validation only).
	nixos-rebuild dry-activate \
	  --flake .#streamctl \
	  --target-host root@$(IP) \
	  --build-host root@$(IP)

update: ## Update flake inputs (nixpkgs) to their latest versions.
	nix flake update

# ---------- local dev ----------

build: ## Build the streamctl binary locally (output: ./result/bin/cmd).
	nix build .#streamctl

shell: ## Enter the dev shell with go, terraform, doctl, etc.
	nix develop

run-local: build ## Run streamctl locally on :8080 with a test config.
	@mkdir -p /tmp/streamctl-local/videos
	@if [ ! -e /tmp/streamctl-local/secret ]; then \
	  echo "test-secret" > /tmp/streamctl-local/secret; \
	fi
	STREAMCTL_SECRET=$$(cat /tmp/streamctl-local/secret) ./result/bin/cmd \
	  -listen :8080 \
	  -db /tmp/streamctl-local/streamctl.db \
	  -video-dir /tmp/streamctl-local/videos \
	  -unit-dir /tmp/streamctl-local \
	  -unit-prefix test- \
	  -run-user $$USER

# ---------- ops ----------

ssh: check-ip ## SSH into the droplet.
	ssh root@$(IP)

logs: check-ip ## Tail streamctl logs.
	ssh root@$(IP) 'journalctl -u streamctl -f'

# Usage: make stream-logs ID=3
stream-logs: check-ip ## Tail logs for a specific scheduled stream (ID=N).
	@if [ -z "$(ID)" ]; then \
	  echo "Usage: make stream-logs ID=<n>" >&2; \
	  exit 1; \
	fi
	ssh root@$(IP) "journalctl -u streamctl-stream-$(ID) -f"

timers: check-ip ## List all scheduled streamctl timers.
	ssh root@$(IP) 'systemctl list-timers "streamctl-*"'

# Usage: make upload FILE=path/to/video.mp4
upload: check-ip ## Upload a video file via scp (FILE=path).
	@if [ -z "$(FILE)" ]; then \
	  echo "Usage: make upload FILE=<path>" >&2; \
	  exit 1; \
	fi
	scp "$(FILE)" "streamctl@$(IP):/var/lib/streamctl/videos/"
	@echo "Uploaded $(FILE). It should appear in the UI immediately."

# ---------- teardown ----------

destroy: check-token ## Destroy the droplet. Prompts for confirmation.
	cd terraform && terraform destroy

.DEFAULT_GOAL := help

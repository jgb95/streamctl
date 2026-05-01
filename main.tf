terraform {
  required_version = ">= 1.5"

  required_providers {
    digitalocean = {
      source  = "digitalocean/digitalocean"
      version = "~> 2.0"
    }
  }
}

provider "digitalocean" {
  # Token is read from the DIGITALOCEAN_TOKEN environment variable.
  # The Makefile/justfile populates this from `doctl auth list`.
}

# ---------- variables ----------

variable "droplet_name" {
  type    = string
  default = "streamctl"
}

variable "region" {
  type        = string
  default     = "nyc3"
  description = "DigitalOcean region. Pick one close to your audience."
}

variable "size" {
  type        = string
  default     = "s-2vcpu-4gb"
  description = "Droplet size. 2 vCPU / 4 GB is plenty for ffmpeg -c copy."
}

variable "ssh_key_path" {
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
  description = "Path to the SSH public key to install on the droplet."
}

variable "nix_channel" {
  type        = string
  default     = "nixos-25.05"
  description = "NixOS channel for the initial nixos-infect bootstrap."
}

# ---------- ssh key ----------

resource "digitalocean_ssh_key" "deploy" {
  name       = "${var.droplet_name}-deploy"
  public_key = file(pathexpand(var.ssh_key_path))
}

# ---------- the droplet ----------

resource "digitalocean_droplet" "streamctl" {
  name     = var.droplet_name
  image    = "ubuntu-24-04-x64"
  region   = var.region
  size     = var.size
  ssh_keys = [digitalocean_ssh_key.deploy.fingerprint]
  ipv6     = true

  # cloud-init runs nixos-infect on first boot, converting Ubuntu to NixOS.
  # After ~5 minutes the droplet reboots into NixOS and is ready for
  # nixos-rebuild --target-host deploys.
  user_data = templatefile("${path.module}/cloud-init.yaml", {
    nix_channel = var.nix_channel
  })

  # nixos-infect changes the kernel and reboots; ignore changes to user_data
  # so that subsequent applies don't try to recreate the droplet.
  lifecycle {
    ignore_changes = [user_data, image]
  }

  tags = ["streamctl", "nixos"]
}

# ---------- firewall ----------

resource "digitalocean_firewall" "streamctl" {
  name        = "${var.droplet_name}-fw"
  droplet_ids = [digitalocean_droplet.streamctl.id]

  # SSH
  inbound_rule {
    protocol         = "tcp"
    port_range       = "22"
    source_addresses = ["0.0.0.0/0", "::/0"]
  }

  # HTTPS (for the streamctl web UI behind nginx)
  inbound_rule {
    protocol         = "tcp"
    port_range       = "443"
    source_addresses = ["0.0.0.0/0", "::/0"]
  }

  # HTTP (for ACME challenges; Let's Encrypt redirects to 443)
  inbound_rule {
    protocol         = "tcp"
    port_range       = "80"
    source_addresses = ["0.0.0.0/0", "::/0"]
  }

  # All outbound allowed (RTMP push to YouTube/X needs this).
  outbound_rule {
    protocol              = "tcp"
    port_range            = "all"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "udp"
    port_range            = "all"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }

  outbound_rule {
    protocol              = "icmp"
    destination_addresses = ["0.0.0.0/0", "::/0"]
  }
}

# ---------- outputs ----------

output "ipv4" {
  value       = digitalocean_droplet.streamctl.ipv4_address
  description = "Public IPv4 address of the droplet."
}

output "ipv6" {
  value       = digitalocean_droplet.streamctl.ipv6_address
  description = "Public IPv6 address of the droplet."
}

output "ssh_command" {
  value       = "ssh root@${digitalocean_droplet.streamctl.ipv4_address}"
  description = "SSH into the droplet (after nixos-infect completes, ~5 min after creation)."
}

output "deploy_command" {
  value       = "nixos-rebuild switch --flake .#${var.droplet_name} --target-host root@${digitalocean_droplet.streamctl.ipv4_address}"
  description = "Run from the nixos/ directory to deploy configuration changes."
}

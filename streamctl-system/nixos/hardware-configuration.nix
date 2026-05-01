# This file is a placeholder.
#
# nixos-infect generates the real hardware-configuration.nix on the droplet
# during the first-boot conversion. After Terraform creates the droplet and
# nixos-infect finishes (~5 min after `terraform apply`), pull it back to
# your laptop:
#
#   just pull-hardware-config
#
# That command scp's /etc/nixos/hardware-configuration.nix from the droplet
# into this directory, replacing this placeholder. Then `just deploy` will
# work.
{ }

# streamer

The active streamctl application, NixOS deployment, and Terraform state live
in [`streamctl-system/`](streamctl-system/).

Commands may be run from that directory:

```bash
cd streamctl-system
make deploy
```

The root `Makefile` is a convenience wrapper, so `make deploy` from this
directory delegates to the same active project.

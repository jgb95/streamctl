# Convenience wrapper. The deployable project lives in streamctl-system/.
.DEFAULT_GOAL := help

.PHONY: help
help:
	$(MAKE) -C streamctl-system help

%:
	$(MAKE) -C streamctl-system $@
